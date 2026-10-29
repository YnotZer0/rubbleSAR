package perceive

import (
	"math"
	"sort"
	"time"

	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/vcam"
)

// Detector finds body parts in a colour frame (infer.Detector, or a stand-in).
type Detector interface {
	Name() string
	DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error)
}

// Params configure a pipeline.
type Params struct {
	Thermal   ThermalParams
	WarmSpots bool
	Track     TrackParams
	Errors    PoseErrors
}

// Capture is one frame as the drone has it.
type Capture struct {
	FrameID  string
	Time     time.Time
	SimTimeS float64
	Reported vcam.Pose // as the GPS and IMU report it (local metres)
	PoseLL   PoseReport
	RGB      *raster.RGB
	Thermal  *raster.U16 // centikelvin; nil without a thermal camera
	// Phase 4: the drone is holding over the target of task TaskID; frames
	// are registered to the hover's first frame and outlines compared.
	Hover  bool
	TaskID string
}

// StageTimes is the time each stage took for one frame.
type StageTimes struct {
	Segment, Thermal, Track, Package time.Duration
	// Registering a hover frame to the hover's first (included in Track).
	Register time.Duration
	// Shift of a hover frame against the hover's first, pixels.
	ShiftX, ShiftY                 int
	Detections, WarmSpots, Reports int
}

// Pipeline is one drone's perception: segment, thermal cue, geolocate, track, package.
type Pipeline struct {
	UAV     string
	p       Params
	det     Detector
	rgbIn   vcam.Intrinsics
	tm      ThermalMap
	Tracker *Tracker
	pos     Position

	hover     *regRef // reference frame of the hover in progress
	hoverTask string
}

// NewPipeline builds a pipeline for one drone.
func NewPipeline(uav string, det Detector, rgbIn, thIn vcam.Intrinsics, p Params, pos Position) *Pipeline {
	return &Pipeline{UAV: uav, p: p, det: det, rgbIn: rgbIn, tm: NewThermalMap(rgbIn, thIn), Tracker: NewTracker(uav, p.Track), pos: pos}
}

// ThermalMap returns the colour/thermal mapping.
func (pl *Pipeline) ThermalMap() ThermalMap { return pl.tm }

// Process runs one frame and returns the detection packets to send (headers
// not yet set). Tracks reported here are marked as reported.
func (pl *Pipeline) Process(c Capture) ([]*sarpb.Detection, StageTimes, error) {
	var st StageTimes
	t0 := time.Now()
	ds, _, err := pl.det.DetectFrame(c.RGB)
	if err != nil {
		return nil, st, err
	}
	ds = mergeOverlapping(ds)
	st.Segment = time.Since(t0)
	st.Detections = len(ds)

	// Phase 4: hover frames are registered to the hover's first frame.
	var dx, dy int
	first := false
	if c.Hover {
		tr := time.Now()
		if pl.hover == nil || pl.hoverTask != c.TaskID {
			pl.hover, pl.hoverTask, first = newRegRef(c.RGB), c.TaskID, true
			pl.Tracker.NewSegment(c.Time)
		} else {
			dx, dy = pl.hover.Register(c.RGB)
		}
		st.Register, st.ShiftX, st.ShiftY = time.Since(tr), dx, dy
	} else if pl.hover != nil {
		pl.hover, pl.hoverTask = nil, ""
		pl.Tracker.NewSegment(c.Time)
	}

	t1 := time.Now()
	cam, err := vcam.New(pl.rgbIn, c.Reported)
	if err != nil {
		return nil, st, err
	}
	readings := make([]Reading, len(ds))
	var ms []*masks.Bitmap
	for i := range ds {
		readings[i] = pl.tm.Read(c.Thermal, &ds[i].Mask, pl.p.Thermal)
		ms = append(ms, &ds[i].Mask)
	}
	var spots []WarmSpot
	if pl.p.WarmSpots && c.Thermal != nil {
		spots = pl.tm.WarmSpots(c.Thermal, pl.tm.Covered(c.Thermal.W, c.Thermal.H, ms), pl.p.Thermal)
	}
	st.WarmSpots = len(spots)
	st.Thermal = time.Since(t1)

	t2 := time.Now()
	var obs []Obs
	for i := range ds {
		d := &ds[i]
		// A detection needs mask pixels inside the frame to be located; a box
		// whose mask is empty (or lies outside the image) is dropped.
		if d.Area <= 0 || d.CX < 0 || d.CY < 0 || d.CX >= float64(c.RGB.W) || d.CY >= float64(c.RGB.H) {
			continue
		}
		f, ok := Geolocate(cam, d.CX, d.CY, pl.p.Errors)
		if !ok || d.Class < 0 || d.Class > 4 {
			continue
		}
		box := [4]float64{float64(d.Box[0]), float64(d.Box[1]), float64(d.Box[2]), float64(d.Box[3])}
		mask := d.Mask
		o := Obs{Class: d.Class, Conf: d.Conf, Fix: f, AreaM2: f.AreaM2(d.Area), Thermal: readings[i], Time: c.Time,
			Evidence: pl.evidence(c, box, &mask, f.RangeM)}
		if c.Hover {
			o.Hover, o.TaskID = true, hoverTask(c)
			// Only outlines in the middle of the frame are compared for
			// movement: near the edge an outline is cut by the frame or seen
			// at an angle past nearby debris, and changes as the drone drifts.
			// The casualty the drone was sent to is in the middle.
			if m := &ds[i].Mask; inCentre(m, c.RGB.W, c.RGB.H) {
				sx, sy := dx, dy
				if !first {
					tl := time.Now()
					if pr := pl.hover.localAlign(c.RGB, m.X0, m.Y0, m.W, m.H, dx, dy, pl.p.Track.Motion.TolPx); pr != nil {
						sx, sy, o.photo = pr.sx, pr.sy, pr
					}
					st.Register += time.Since(tl)
				}
				o.Mask = shifted(m, sx, sy)
			}
		}
		obs = append(obs, o)
	}
	for i := range spots {
		s := &spots[i]
		f, ok := Geolocate(cam, s.U, s.V, pl.p.Errors)
		if !ok {
			continue
		}
		thGSD := f.GSDm * pl.tm.rgbF / pl.tm.thF
		mask := s.MaskRGB
		obs = append(obs, Obs{Class: WarmClass, Fix: f, AreaM2: float64(s.Area) * thGSD * thGSD / math.Max(f.CosT, 0.2), Thermal: s.Reading, Time: c.Time,
			Evidence: pl.evidence(c, s.BoxRGB, &mask, f.RangeM), Hover: c.Hover, TaskID: hoverTask(c)})
	}
	reps := pl.Tracker.Update(obs, c.Time)
	st.Track = time.Since(t2) + st.Register

	t3 := time.Now()
	var out []*sarpb.Detection
	for _, r := range reps {
		out = append(out, Packet(r, pl.Tracker, pl.UAV, pl.pos))
		pl.Tracker.MarkReported(r.Track, c.Time)
	}
	st.Package = time.Since(t3)
	st.Reports = len(out)
	return out, st, nil
}

func hoverTask(c Capture) string {
	if c.Hover {
		return c.TaskID
	}
	return ""
}

// mergeOverlapping: a real model sometimes outputs one part twice in a frame
// (a whole leg and its lower half: their boxes differ too much for the
// model's own suppression, their outlines lie on top of each other). Outlines
// of one class that mostly overlap (overlapMerge of the smaller) are one
// part: the more confident detection is kept, with both outlines.
func mergeOverlapping(ds []infer.Detection) []infer.Detection {
	if len(ds) < 2 {
		return ds
	}
	order := make([]int, len(ds))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return ds[order[a]].Conf > ds[order[b]].Conf })
	var keep []infer.Detection
	for _, i := range order {
		d := ds[i]
		merged := false
		for k := range keep {
			kd := &keep[k]
			if kd.Class != d.Class || d.Area <= 0 || kd.Area <= 0 {
				continue
			}
			if in := overlapPx(&kd.Mask, &d.Mask); float64(in) >= overlapMerge*float64(min(kd.Area, d.Area)) {
				u := unionMasks(&kd.Mask, &d.Mask)
				kd.Mask = *u
				var sx, sy float64
				n := 0
				for y := 0; y < u.H; y++ {
					for x := 0; x < u.W; x++ {
						if u.Pix[y*u.W+x] {
							sx, sy, n = sx+float64(u.X0+x), sy+float64(u.Y0+y), n+1
						}
					}
				}
				kd.Area, kd.CX, kd.CY = n, sx/float64(n), sy/float64(n)
				kd.Box = [4]float32{min(kd.Box[0], d.Box[0]), min(kd.Box[1], d.Box[1]), max(kd.Box[2], d.Box[2]), max(kd.Box[3], d.Box[3])}
				merged = true
				break
			}
		}
		if !merged {
			keep = append(keep, d)
		}
	}
	return keep
}

const overlapMerge = 0.5

// overlapPx counts the pixels two outlines share.
func overlapPx(a, b *masks.Bitmap) int {
	x0, y0 := max(a.X0, b.X0), max(a.Y0, b.Y0)
	x1, y1 := min(a.X0+a.W, b.X0+b.W), min(a.Y0+a.H, b.Y0+b.H)
	n := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if a.At(x, y) && b.At(x, y) {
				n++
			}
		}
	}
	return n
}

// centreShare: the middle of the frame (this share of its width and height)
// where outlines are compared for movement while hovering.
const centreShare = 0.7

func inCentre(m *masks.Bitmap, w, h int) bool {
	mx, my := int(float64(w)*(1-centreShare)/2), int(float64(h)*(1-centreShare)/2)
	return m.X0 >= mx && m.Y0 >= my && m.X0+m.W <= w-mx && m.Y0+m.H <= h-my
}

// EndHover reports every confirmed track within radius of the task's target
// (local metres) that was watched during the hover of taskID, with reason
// INVESTIGATED and the movement verdict, whether or not anything changed.
func (pl *Pipeline) EndHover(taskID string, e, n, radius float64, at time.Time) []*sarpb.Detection {
	var out []*sarpb.Detection
	for _, t := range pl.Tracker.Near(e, n, radius, taskID) {
		if t.Best == nil {
			continue
		}
		p := Packet(Report{Track: t, Reason: int(sarpb.ReportReason_REPORT_REASON_INVESTIGATED)}, pl.Tracker, pl.UAV, pl.pos)
		p.TaskId = taskID
		out = append(out, p)
		pl.Tracker.MarkReported(t, at)
	}
	pl.hover, pl.hoverTask = nil, ""
	return out
}

func (pl *Pipeline) evidence(c Capture, box [4]float64, mask *masks.Bitmap, rng float64) func() *Evidence {
	return func() *Evidence {
		ev := MakeEvidence(c.RGB, c.Thermal, pl.tm, box, mask)
		if ev == nil {
			return nil
		}
		ev.FrameID, ev.CaptureNS, ev.SimTimeS, ev.Pose, ev.RangeM = c.FrameID, c.Time.UnixNano(), c.SimTimeS, c.PoseLL, rng
		return ev
	}
}
