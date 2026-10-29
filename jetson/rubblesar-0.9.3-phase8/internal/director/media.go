package director

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/dataset"
	"rubblesar/internal/evalseg"
	"rubblesar/internal/geo"
	"rubblesar/internal/infer"
	"rubblesar/internal/live"
	"rubblesar/internal/tiles"
	"rubblesar/internal/vcam"
)

// Media is what `rubblesar demo media` makes for the presenter (media/media.json):
// one camera frame a drone captured, rendered again on the ZBook, with the
// model's result on it.
type Media struct {
	Run     string        `json:"run"`
	Frame   string        `json:"frame"`
	UAV     string        `json:"uav"`
	Capture int64         `json:"capture"` // unix ms
	SimS    float64       `json:"sim_s"`
	Pose    *aar.PoseSnap `json:"pose"`
	// Exact: rendered from the drone's true pose, scenario clock and noise seed: the frame it saw, pixel for pixel.
	Exact bool    `json:"exact"`
	AltM  float64 `json:"alt_m"`
	GSDcm float64 `json:"gsd_cm"`
	// The pictures (in media/).
	RGB     string `json:"rgb"`
	Thermal string `json:"thermal"`
	Masks   string `json:"masks"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	ThermW  int    `json:"therm_w"`
	ThermH  int    `json:"therm_h"`
	// FrameBytes: the colour frame as a JPEG plus the radiometric thermal frame: what sending the frames would cost.
	FrameBytes int `json:"frame_bytes"`
	RGBBytes   int `json:"rgb_bytes"`
	ThermBytes int `json:"therm_bytes"`
	// Tiles: the model's input squares (x, y, size).
	Tiles [][3]int `json:"tiles"`
	// How the result was made: "ONNX Runtime 1.22 on the ZBook, the same model as the drone's TensorRT engine", or the labels.
	Model      string     `json:"model"`
	ModelName  string     `json:"model_name,omitempty"`
	InferMS    float64    `json:"infer_ms,omitempty"`
	Detections []MediaDet `json:"detections"`
	Truth      int        `json:"truth"`
	// Reports: the drone's reports from this frame (their crops are in the run's crops/).
	Reports []*aar.DetSnap `json:"reports"`
	Made    time.Time      `json:"made"`
	Note    string         `json:"note"`
}

// MediaDet is one body part the model found in the frame.
type MediaDet struct {
	Class string     `json:"class"`
	Conf  float64    `json:"conf"`
	Box   [4]float64 `json:"box"` // x0, y0, x1, y1 in frame pixels
	Tile  int        `json:"tile"`
	Area  int        `json:"area"`
}

// MediaOptions are what MakeMedia needs.
type MediaOptions struct {
	ScenarioDir string
	ModelDir    string
	// EdgeSeed: edge-eye's seed ([demo] edge_seed; the release config: 1).
	EdgeSeed int64
	// Frame: a frame id to render instead of the one chosen.
	Frame string
	Log   func(string)
}

// PickFrame chooses the frame to show: one of the scout's with a frame truth,
// with the most reports in it (the first Red casualty's preferred).
func PickFrame(p *live.Player) (string, []*aar.DetSnap) {
	frames := p.Frames()
	role := map[string]string{}
	for _, e := range p.Events() {
		if e.K == "tel" && e.Tel != nil {
			role[e.Tel.UAV] = e.Tel.Role
		}
	}
	byFrame := map[string][]*aar.DetSnap{}
	var order []string
	for _, d := range p.Detections() {
		if d.Frame == "" {
			continue
		}
		if byFrame[d.Frame] == nil {
			order = append(order, d.Frame)
		}
		byFrame[d.Frame] = append(byFrame[d.Frame], d)
	}
	firstRed := map[string]bool{}
	for _, b := range p.Beats() {
		if b.Name == "first-red" {
			for _, e := range p.Events() {
				if e.K == "casd" && e.CasD != nil && e.CasD.ID == b.Ref {
					for _, x := range e.CasD.Parts {
						firstRed[x.Det] = true
					}
				}
			}
		}
	}
	// The scout's frames first (the narration speaks of them), then any with their truth, then any.
	score := func(id string) float64 {
		v := 0.0
		for _, d := range byFrame[id] {
			v += 1 + d.Conf
			if firstRed[d.ID] {
				v += 3
			}
			if d.Crop {
				v += 0.5
			}
		}
		return v
	}
	isScout := func(id string) bool { ds := byFrame[id]; return len(ds) > 0 && role[ds[0].UAV] == "scout" }
	hasTruth := func(id string) bool { _, ok := frames[id]; return ok }
	best := ""
	for _, keep := range []func(string) bool{func(id string) bool { return isScout(id) && hasTruth(id) }, hasTruth, func(string) bool { return true }} {
		bestScore := -1.0
		for _, id := range order {
			if keep(id) && score(id) > bestScore {
				best, bestScore = id, score(id)
			}
		}
		if best != "" {
			break
		}
	}
	return best, byFrame[best]
}

// frameNumber is the frame counter in an id ("scout-1-000123" -> 123).
func frameNumber(id string) (int64, bool) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(id[i+1:], 10, 64)
	return n, err == nil
}

// MakeMedia renders the chosen frame into the run's media folder.
func MakeMedia(p *live.Player, o MediaOptions) (*Media, error) {
	logf := func(f string, a ...any) {
		if o.Log != nil {
			o.Log(fmt.Sprintf(f, a...))
		}
	}
	frameID, reports := PickFrame(p)
	if o.Frame != "" {
		frameID, reports = o.Frame, nil
		for _, d := range p.Detections() {
			if d.Frame == o.Frame {
				reports = append(reports, d)
			}
		}
	}
	if frameID == "" {
		return nil, fmt.Errorf("E-DIR-004: run %s has no detection reports with their frame (recorded with phase 8 or later?)", p.Run)
	}
	w, err := vcam.Open(o.ScenarioDir, false)
	if err != nil {
		return nil, fmt.Errorf("E-DIR-004: the scenario: %w", err)
	}
	loc := w.Scn.Local()
	m := &Media{Run: p.Run, Frame: frameID, Reports: reports, Made: time.Now().UTC(), Width: vcam.DefaultRGB.W, Height: vcam.DefaultRGB.H,
		ThermW: vcam.DefaultThermal.W, ThermH: vcam.DefaultThermal.H}
	req := vcam.Request{FrameID: frameID}
	pose := (*aar.PoseSnap)(nil)
	if f := p.Frames()[frameID]; f != nil && f.True != nil {
		pose, m.UAV, m.Capture, m.SimS = f.True, f.UAV, f.Capture, f.SimS
		if n, ok := frameNumber(frameID); ok {
			req.Seed = o.EdgeSeed*10_000_000 + n
			m.Exact = true
		}
	} else if len(reports) > 0 && reports[0].Pose != nil {
		pose, m.UAV, m.Capture, m.SimS = reports[0].Pose, reports[0].UAV, reports[0].Capture, reports[0].SimS
		m.Note = "rendered from the pose the drone reported (no frame truth recorded), so it is shifted by the drone's GPS error"
	} else {
		return nil, fmt.Errorf("E-DIR-004: frame %s: no pose recorded", frameID)
	}
	m.Pose = pose
	e, n := loc.ToLocal(geo.Point{Lat: pose.Lat, Lon: pose.Lon})
	req.UAV = m.UAV
	req.Pose = vcam.Pose{E: e, N: n, AltAGL: pose.Alt, YawDeg: pose.Yaw, PitchDeg: pose.Pitch, RollDeg: pose.Roll}
	req.Reported = req.Pose
	req.TOffsetS = m.SimS
	m.AltM = math.Round(pose.Alt*10) / 10
	m.GSDcm = math.Round(2*pose.Alt*math.Tan(vcam.DefaultRGB.HFOVDeg/2*math.Pi/180)/float64(vcam.DefaultRGB.W)*1000) / 10
	logf("rendering %s (%s, %.0f m up, scenario clock %.0f s%s)", frameID, m.UAV, pose.Alt, m.SimS, map[bool]string{true: ", exact", false: ""}[m.Exact])
	fr, err := w.Render(req)
	if err != nil {
		return nil, fmt.Errorf("E-DIR-004: rendering %s: %w", frameID, err)
	}
	dir := filepath.Join(p.Dir, "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("E-DIR-004: %w", err)
	}
	// The colour frame.
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, fr.RGB.ToNRGBA(), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	m.RGB, m.RGBBytes = "frame-rgb.jpg", jb.Len()
	if err := os.WriteFile(filepath.Join(dir, m.RGB), jb.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("E-DIR-004: %w", err)
	}
	// The thermal frame: its radiometric size, and a false-colour picture.
	var tb bytes.Buffer
	_ = png.Encode(&tb, fr.Thermal.Image())
	m.ThermBytes = tb.Len()
	m.FrameBytes = m.RGBBytes + m.ThermBytes
	m.Thermal = "frame-thermal.png"
	if err := savePNG(filepath.Join(dir, m.Thermal), vcam.ThermalView(fr.Thermal, math.NaN(), math.NaN())); err != nil {
		return nil, err
	}
	for _, t := range tiles.Plan(fr.RGB.W, fr.RGB.H, 640) {
		m.Tiles = append(m.Tiles, [3]int{t.X, t.Y, t.Size})
	}
	truth := evalseg.TruthInstances(fr.PartsRGB, fr.InstRGB, 6)
	m.Truth = len(truth)
	// The model, as on the drone (ONNX Runtime here); the labels when there is no model.
	var dets []infer.Detection
	if b, card, err := infer.Open(o.ModelDir, 6); err == nil {
		d := &infer.Detector{B: b, P: infer.DefaultParams()}
		var tm infer.Timing
		name := b.Name()
		dets, tm, err = d.DetectFrame(fr.RGB)
		b.Close()
		if err != nil {
			return nil, fmt.Errorf("E-DIR-004: the model on %s: %w", frameID, err)
		}
		m.Model, m.InferMS = name+" on the command-post laptop (the same model the drone runs with TensorRT)", math.Round(tm.TotalMS())
		if card != nil {
			m.ModelName = card.Name + " (" + card.Arch + ")"
		}
		logf("model: %d body parts found (%d labelled in view) in %.0f ms", len(dets), len(truth), tm.TotalMS())
	} else {
		m.Model = "no model on this computer: the outlines are the scenario's labels"
		for _, t := range truth {
			dets = append(dets, infer.Detection{Class: t.Class, Name: className(t.Class), Conf: 1, Mask: t.Mask, Area: t.Area,
				Box: [4]float32{float32(t.Mask.X0), float32(t.Mask.Y0), float32(t.Mask.X0 + t.Mask.W), float32(t.Mask.Y0 + t.Mask.H)}})
		}
		logf("no model here (%v): showing the labels instead", err)
	}
	for _, d := range dets {
		m.Detections = append(m.Detections, MediaDet{Class: d.Name, Conf: math.Round(float64(d.Conf)*1000) / 1000,
			Box: [4]float64{float64(d.Box[0]), float64(d.Box[1]), float64(d.Box[2]), float64(d.Box[3])}, Tile: d.Tile, Area: d.Area})
	}
	sort.Slice(m.Detections, func(i, j int) bool { return m.Detections[i].Conf > m.Detections[j].Conf })
	// The masks over the frame (boxes, labels and tiles are drawn by the page).
	m.Masks = "frame-masks.jpg"
	var mb bytes.Buffer
	if err := jpeg.Encode(&mb, maskOverlay(fr, dets), &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, m.Masks), mb.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("E-DIR-004: %w", err)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "media.json"), append(b, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("E-DIR-004: %w", err)
	}
	return m, nil
}

func className(c int) string {
	if c >= 0 && c < len(infer.ClassNames) {
		return infer.ClassNames[c]
	}
	return "part"
}

// maskOverlay tints each found part in its class colour, with a bright edge.
func maskOverlay(fr *vcam.Frame, dets []infer.Detection) image.Image {
	m := fr.RGB.ToNRGBA()
	for _, d := range dets {
		c := dataset.ClassColours[d.Class%len(dataset.ClassColours)]
		b := &d.Mask
		for y := 0; y < b.H; y++ {
			for x := 0; x < b.W; x++ {
				if !b.Pix[y*b.W+x] {
					continue
				}
				gx, gy := b.X0+x, b.Y0+y
				if !image.Pt(gx, gy).In(m.Rect) {
					continue
				}
				edge := !b.At(gx-1, gy) || !b.At(gx+1, gy) || !b.At(gx, gy-1) || !b.At(gx, gy+1)
				a := 0.5
				if edge {
					a = 1
				}
				o := m.PixOffset(gx, gy)
				for k := 0; k < 3; k++ {
					m.Pix[o+k] = uint8(float64(m.Pix[o+k])*(1-a) + float64(c[k])*a)
				}
			}
		}
	}
	return m
}

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("E-DIR-004: %w", err)
	}
	defer f.Close()
	return png.Encode(f, img)
}
