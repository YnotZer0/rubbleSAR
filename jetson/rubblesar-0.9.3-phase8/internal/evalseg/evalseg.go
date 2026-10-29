// Package evalseg measures the segmentation model on held-out frames: masks
// against the pixel-exact labels (precision, recall, AP50 per class and per
// kind of view), and geolocated detections against the scenario truth.
package evalseg

import (
	"fmt"
	"image"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"rubblesar/internal/dataset"
	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/score"
	"rubblesar/internal/vcam"
)

// Truth is one labelled body-part region of a frame.
type Truth struct {
	Class  int // model class 0..4
	Victim int
	Mask   masks.Bitmap
	Area   int
	CX, CY float64
}

// TruthInstances extracts the labelled regions of a frame (minPx and larger).
func TruthInstances(parts, inst *raster.U8, minPx int) []Truth {
	key := func(i int) uint32 {
		p := parts.Pix[i]
		if p == 0 || int(p) >= scenario.NumParts {
			return 0
		}
		return uint32(inst.Pix[i])<<8 | uint32(p)
	}
	var out []Truth
	for _, c := range masks.Components(parts.W, parts.H, key) {
		if c.Area() < minPx {
			continue
		}
		out = append(out, Truth{Class: int(c.Key&0xff) - 1, Victim: int(c.Key >> 8), Mask: c.Bitmap(parts.W), Area: c.Area(), CX: c.CX, CY: c.CY})
	}
	return out
}

// Options control an evaluation.
type Options struct {
	MinTruthPx int     // smaller regions are not expected (same as the training labels)
	StrictIoU  float64 // mask IoU for a strict match
	LenientIoU float64 // mask IoU for a lenient match ...
	LenientPx  float64 // ... or detection centre within this many pixels of the region
	Overlays   string  // folder for overlay pictures ("" = none)
	OverlayMax int     // per kind of view
}

// DefaultOptions are the phase 2 settings.
func DefaultOptions() Options {
	return Options{MinTruthPx: 6, StrictIoU: 0.5, LenientIoU: 0.25, LenientPx: 4, OverlayMax: 2}
}

// ClassReport is the result for one class (or all).
type ClassReport struct {
	Class            string  `json:"class"`
	Truth            int     `json:"truth"`
	Detections       int     `json:"detections"`
	TP               int     `json:"tp"`
	FP               int     `json:"fp"`
	FN               int     `json:"fn"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall"`
	AP50             float64 `json:"mask_ap50"`
	TPLenient        int     `json:"tp_lenient"`
	PrecisionLenient float64 `json:"precision_lenient"`
	RecallLenient    float64 `json:"recall_lenient"`
}

// KindReport groups frames by kind of view (investigator/scout, nadir/oblique).
type KindReport struct {
	Kind          string  `json:"kind"`
	Frames        int     `json:"frames"`
	Truth         int     `json:"truth"`
	Detections    int     `json:"detections"`
	TP            int     `json:"tp"`
	TPLenient     int     `json:"tp_lenient"`
	Precision     float64 `json:"precision"`
	Recall        float64 `json:"recall"`
	RecallLenient float64 `json:"recall_lenient"`
	FalsePerFrame float64 `json:"false_alarms_per_frame"`
}

// Timing summarises per-frame inference time.
type Timing struct {
	Frames      int     `json:"frames"`
	MedianMS    float64 `json:"median_ms"`
	P95MS       float64 `json:"p95_ms"`
	MedianInfer float64 `json:"median_infer_ms"`
	Tiles       int     `json:"tiles_per_frame"`
}

// Report is the whole evaluation.
type Report struct {
	Method     int           `json:"method"` // see Method; 0 or 1 = phase 2.0 report
	Scenario   string        `json:"scenario"`
	Backend    string        `json:"backend"`
	Frames     int           `json:"frames"`
	Options    Options       `json:"options"`
	Classes    []ClassReport `json:"classes"`
	Overall    ClassReport   `json:"overall"`
	Kinds      []KindReport  `json:"kinds"`
	Geo        *score.Result `json:"geolocation_reported_pose"`
	GeoTrue    *score.Result `json:"geolocation_true_pose"`
	Timing     Timing        `json:"timing"`
	Detections int           `json:"detections"`
	// Merged is the method-2 view (each visible part counted once, people found,
	// sizes, confusion). Classes/Overall/Kinds above keep the method-1 numbers.
	Merged *Merged `json:"merged,omitempty"`
	// Conf is the confidence threshold the detector used.
	Conf float64 `json:"conf,omitempty"`
}

type flag struct {
	score         float32
	strict, loose bool
}

type acc struct {
	flags        []flag
	truth        int
	tp, tpl, det int
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*1000) / 1000
}

// near reports whether (x, y) lies within r pixels of any pixel of the mask.
func near(m *masks.Bitmap, x, y, r float64) bool {
	x0, y0 := int(math.Floor(x-r-0.5)), int(math.Floor(y-r-0.5))
	x1, y1 := int(math.Ceil(x+r)), int(math.Ceil(y+r))
	for py := y0; py <= y1; py++ {
		for px := x0; px <= x1; px++ {
			if m.At(px, py) && math.Hypot(float64(px)+0.5-x, float64(py)+0.5-y) <= r+0.71 {
				return true
			}
		}
	}
	return false
}

// MatchFrame matches detections to truth (greedy by confidence, same class)
// and returns per-detection flags and per-truth hits.
func MatchFrame(ds []infer.Detection, ts []Truth, o Options) (flags []flag, strictHit, looseHit []bool) {
	order := make([]int, len(ds))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return ds[order[a]].Conf > ds[order[b]].Conf })
	strictHit = make([]bool, len(ts))
	looseHit = make([]bool, len(ts))
	flags = make([]flag, len(ds))
	for _, i := range order {
		d := ds[i]
		f := flag{score: d.Conf}
		bs, bsi := 0.0, -1
		bl, bli := 0.0, -1
		for j, t := range ts {
			if t.Class != d.Class {
				continue
			}
			v := masks.IoU(&d.Mask, &t.Mask)
			if !strictHit[j] && v > bs {
				bs, bsi = v, j
			}
			if !looseHit[j] {
				score := v
				if near(&t.Mask, d.CX, d.CY, o.LenientPx) {
					score = math.Max(score, o.LenientIoU)
				}
				if score > bl {
					bl, bli = score, j
				}
			}
		}
		if bsi >= 0 && bs >= o.StrictIoU {
			strictHit[bsi] = true
			f.strict = true
		}
		if bli >= 0 && bl >= o.LenientIoU {
			looseHit[bli] = true
			f.loose = true
		}
		flags[i] = f
	}
	return
}

// AP computes the all-point interpolated average precision.
func AP(fs []flag, nTruth int, strict bool) float64 {
	if nTruth == 0 {
		return 0
	}
	s := append([]flag(nil), fs...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].score > s[j].score })
	var prec, rec []float64
	tp, fp := 0, 0
	for _, f := range s {
		hit := f.loose
		if strict {
			hit = f.strict
		}
		if hit {
			tp++
		} else {
			fp++
		}
		prec = append(prec, float64(tp)/float64(tp+fp))
		rec = append(rec, float64(tp)/float64(nTruth))
	}
	for i := len(prec) - 2; i >= 0; i-- {
		prec[i] = math.Max(prec[i], prec[i+1])
	}
	ap, prev := 0.0, 0.0
	for i := range prec {
		ap += (rec[i] - prev) * prec[i]
		prev = rec[i]
	}
	return math.Round(ap*1000) / 1000
}

// FrameDetector finds body parts in a frame (infer.Detector, or a stand-in in tests).
type FrameDetector interface {
	Name() string
	DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error)
}

// Evaluate renders every test frame, runs the detector and scores it.
func Evaluate(w *vcam.World, frames []dataset.TestFrame, det FrameDetector, o Options, progress func(string)) (*Report, error) {
	r := &Report{Method: Method, Scenario: w.Scn.ID, Backend: det.Name(), Options: o}
	if d, ok := det.(*infer.Detector); ok {
		r.Conf = d.P.Conf
	}
	ma := newMergedAcc()
	var per [5]acc
	kinds := map[string]*KindReport{}
	var geoRep, geoTrue []score.Detection
	inView := map[int]bool{}
	var totals, infers []float64
	overlaysDone := map[string]int{}
	local := w.Scn.Local()
	for fi, tf := range frames {
		fr, err := w.Render(tf.Request())
		if err != nil {
			return nil, err
		}
		ds, tm, err := det.DetectFrame(fr.RGB)
		if err != nil {
			return nil, err
		}
		totals = append(totals, tm.TotalMS())
		infers = append(infers, tm.InferMS)
		r.Timing.Tiles = tm.Tiles
		ts := TruthInstances(fr.PartsRGB, fr.InstRGB, o.MinTruthPx)
		for _, t := range ts {
			inView[t.Victim] = true
		}
		ma.add(tf.Kind, ds, MergedTruth(fr.PartsRGB, fr.InstRGB, o.MinTruthPx), o)
		flags, _, _ := MatchFrame(ds, ts, o)
		k := kinds[tf.Kind]
		if k == nil {
			k = &KindReport{Kind: tf.Kind}
			kinds[tf.Kind] = k
		}
		k.Frames++
		k.Truth += len(ts)
		k.Detections += len(ds)
		for i, d := range ds {
			if d.Class < 0 || d.Class > 4 {
				continue
			}
			per[d.Class].flags = append(per[d.Class].flags, flags[i])
			per[d.Class].det++
			if flags[i].strict {
				per[d.Class].tp++
				k.TP++
			}
			if flags[i].loose {
				per[d.Class].tpl++
				k.TPLenient++
			}
		}
		for _, t := range ts {
			per[t.Class].truth++
		}
		// Geolocate with the reported pose (what the drone knows) and the true pose.
		for _, pose := range []struct {
			p   vcam.Pose
			out *[]score.Detection
		}{{tf.Reported, &geoRep}, {tf.Pose, &geoTrue}} {
			cam, err := vcam.New(vcam.DefaultRGB, pose.p)
			if err != nil {
				return nil, err
			}
			for _, d := range ds {
				e, n, ok := cam.GroundAt(d.CX, d.CY, 0)
				if !ok {
					continue
				}
				ll := local.ToPoint(e, n)
				*pose.out = append(*pose.out, score.Detection{Class: d.Name, Lat: ll.Lat, Lon: ll.Lon, Confidence: float64(d.Conf), FrameID: tf.ID})
			}
		}
		r.Detections += len(ds)
		if o.Overlays != "" && overlaysDone[tf.Kind] < o.OverlayMax {
			overlaysDone[tf.Kind]++
			if err := raster.SavePNG(filepath.Join(o.Overlays, tf.ID+".png"), Overlay(fr.RGB, ts, ds)); err != nil {
				return nil, err
			}
		}
		if progress != nil && (fi%10 == 9 || fi == len(frames)-1) {
			progress(fmt.Sprintf("%d/%d frames", fi+1, len(frames)))
		}
	}
	r.Frames = len(frames)
	r.Classes, r.Overall = classReports(per)
	r.Merged = ma.finish()
	for _, k := range kinds {
		finishKind(k)
		r.Kinds = append(r.Kinds, *k)
	}
	sort.Slice(r.Kinds, func(i, j int) bool { return r.Kinds[i].Kind < r.Kinds[j].Kind })
	// Geolocation against the scenario truth, limited to victims the frames showed.
	var ms []*render.Manifest
	for _, st := range w.Scn.Sites {
		ms = append(ms, w.Manifest(st.ID))
	}
	so := score.DefaultOptions()
	so.OnlyVictims = inView
	truth := score.AllTruth(ms)
	r.Geo = score.Score(w.Scn, truth, geoRep, so)
	r.GeoTrue = score.Score(w.Scn, truth, geoTrue, so)

	r.Timing.Frames = len(totals)
	r.Timing.MedianMS, r.Timing.P95MS, r.Timing.MedianInfer = pct(totals, 0.5), pct(totals, 0.95), pct(infers, 0.5)
	return r, nil
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return math.Round(s[i]*10) / 10
}

// Text renders the report as tables.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "held-out scenario %s, %d frames, backend %s\n", r.Scenario, r.Frames, r.Backend)
	fmt.Fprintf(&b, "strict = mask IoU >= %.2f; lenient = IoU >= %.2f or centre within %.0f px of the part\n\n", r.Options.StrictIoU, r.Options.LenientIoU, r.Options.LenientPx)
	if r.Conf > 0 {
		fmt.Fprintf(&b, "confidence threshold %.2f\n", r.Conf)
	}
	if r.Merged != nil {
		b.WriteString(r.Merged.Text())
		b.WriteString("\nEvery visible piece counted separately (phase 2.0 method, kept for comparison):\n")
	}
	fmt.Fprintf(&b, "%-7s %6s %6s %5s %5s %9s %7s %8s %9s %9s\n", "class", "truth", "dets", "TP", "FP", "precision", "recall", "AP50", "P(len.)", "R(len.)")
	for _, c := range append(r.Classes, r.Overall) {
		fmt.Fprintf(&b, "%-7s %6d %6d %5d %5d %9.3f %7.3f %8.3f %9.3f %9.3f\n", c.Class, c.Truth, c.Detections, c.TP, c.FP, c.Precision, c.Recall, c.AP50, c.PrecisionLenient, c.RecallLenient)
	}
	fmt.Fprintf(&b, "\n%-22s %6s %6s %6s %9s %7s %9s %12s\n", "view", "frames", "truth", "dets", "precision", "recall", "R(len.)", "false/frame")
	for _, k := range r.Kinds {
		fmt.Fprintf(&b, "%-22s %6d %6d %6d %9.3f %7.3f %9.3f %12.2f\n", k.Kind, k.Frames, k.Truth, k.Detections, k.Precision, k.Recall, k.RecallLenient, k.FalsePerFrame)
	}
	if r.Geo != nil {
		fmt.Fprintf(&b, "\ngeolocated (reported pose, flat ground): victims found %d/%d, part recall %.3f, precision %.3f, error median %.2f m, p95 %.2f m\n",
			r.Geo.VictimsFound, r.Geo.VictimsTotal, r.Geo.Overall.Recall, r.Geo.Overall.Precision, r.Geo.ErrMedianM, r.Geo.ErrP95M)
	}
	if r.GeoTrue != nil {
		fmt.Fprintf(&b, "geolocated (true pose, flat ground):     victims found %d/%d, part recall %.3f, precision %.3f, error median %.2f m, p95 %.2f m\n",
			r.GeoTrue.VictimsFound, r.GeoTrue.VictimsTotal, r.GeoTrue.Overall.Recall, r.GeoTrue.Overall.Precision, r.GeoTrue.ErrMedianM, r.GeoTrue.ErrP95M)
	}
	fmt.Fprintf(&b, "time per frame (%d tiles): median %.0f ms (inference %.0f ms), p95 %.0f ms\n", r.Timing.Tiles, r.Timing.MedianMS, r.Timing.MedianInfer, r.Timing.P95MS)
	return b.String()
}

// Overlay draws truth outlines (white) and detections (class colour boxes and masks).
func Overlay(rgb *raster.RGB, ts []Truth, ds []infer.Detection) *image.NRGBA {
	m := rgb.ToNRGBA()
	set := func(x, y int, c [3]uint8) {
		if x >= 0 && y >= 0 && x < rgb.W && y < rgb.H {
			o := m.PixOffset(x, y)
			m.Pix[o], m.Pix[o+1], m.Pix[o+2] = c[0], c[1], c[2]
		}
	}
	for _, t := range ts {
		b := &t.Mask
		for y := 0; y < b.H; y++ {
			for x := 0; x < b.W; x++ {
				if !b.Pix[y*b.W+x] {
					continue
				}
				gx, gy := b.X0+x, b.Y0+y
				if !b.At(gx-1, gy) || !b.At(gx+1, gy) || !b.At(gx, gy-1) || !b.At(gx, gy+1) {
					set(gx, gy, [3]uint8{255, 255, 255})
				}
			}
		}
	}
	for _, d := range ds {
		c := dataset.ClassColours[d.Class%len(dataset.ClassColours)]
		b := &d.Mask
		for y := 0; y < b.H; y++ {
			for x := 0; x < b.W; x++ {
				if b.Pix[y*b.W+x] {
					o := m.PixOffset(b.X0+x, b.Y0+y)
					if image.Pt(b.X0+x, b.Y0+y).In(m.Rect) {
						for k := 0; k < 3; k++ {
							m.Pix[o+k] = uint8((int(m.Pix[o+k]) + int(c[k])) / 2)
						}
					}
				}
			}
		}
		x0, y0, x1, y1 := int(d.Box[0])-2, int(d.Box[1])-2, int(d.Box[2])+2, int(d.Box[3])+2
		for x := x0; x <= x1; x++ {
			set(x, y0, c)
			set(x, y1, c)
		}
		for y := y0; y <= y1; y++ {
			set(x0, y, c)
			set(x1, y, c)
		}
	}
	return m
}
