package evalseg

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"rubblesar/internal/dataset"
	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

func TestMatchAndAP(t *testing.T) {
	sq := func(x0, y0, x1, y1 int) masks.Bitmap {
		return masks.Fill([][2]float64{{float64(x0), float64(y0)}, {float64(x1), float64(y0)}, {float64(x1), float64(y1)}, {float64(x0), float64(y1)}}, x0, y0, x1-x0, y1-y0)
	}
	ts := []Truth{
		{Class: 0, Mask: sq(10, 10, 20, 20)},
		{Class: 1, Mask: sq(50, 50, 60, 52)},
	}
	det := func(c int, conf float32, m masks.Bitmap) infer.Detection {
		d := infer.Detection{Class: c, Conf: conf, Mask: m}
		var sx, sy float64
		n := 0
		for i, v := range m.Pix {
			if v {
				sx += float64(m.X0+i%m.W) + 0.5
				sy += float64(m.Y0+i/m.W) + 0.5
				n++
			}
		}
		d.CX, d.CY = sx/float64(n), sy/float64(n)
		return d
	}
	ds := []infer.Detection{
		det(0, 0.9, sq(11, 10, 21, 20)), // IoU 0.82: strict
		det(0, 0.8, sq(10, 10, 20, 20)), // duplicate: false positive
		det(1, 0.7, sq(55, 49, 62, 53)), // poor overlap but centre on the part: lenient only
		det(2, 0.6, sq(80, 80, 85, 85)), // nothing there
	}
	o := DefaultOptions()
	fl, sh, lh := MatchFrame(ds, ts, o)
	if !fl[0].strict || fl[1].strict || fl[1].loose || fl[2].strict || !fl[2].loose || fl[3].loose {
		t.Fatalf("flags %+v", fl)
	}
	if !sh[0] || sh[1] || !lh[1] {
		t.Fatalf("hits %v %v", sh, lh)
	}
	if ap := AP([]flag{{0.9, true, true}, {0.8, false, false}, {0.7, true, true}}, 2, true); math.Abs(ap-(0.5*1+0.5*2.0/3)) > 0.002 {
		t.Fatalf("AP %v", ap)
	}
	if AP(nil, 3, true) != 0 {
		t.Fatal("AP without detections")
	}
}

// oracle answers with the labelled truth of each frame, in order.
type oracle struct {
	w      *vcam.World
	frames []dataset.TestFrame
	i      int
	merged bool // answer with merged parts instead of separate pieces
	minPx  int  // smallest piece answered (0 = 6, the training threshold)
}

func (o *oracle) Name() string { return "oracle" }

func (o *oracle) DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error) {
	fr, err := o.w.Render(o.frames[o.i].Request())
	o.i++
	if err != nil {
		return nil, infer.Timing{}, err
	}
	var ds []infer.Detection
	min := o.minPx
	if min == 0 {
		min = 6
	}
	ts := TruthInstances(fr.PartsRGB, fr.InstRGB, min)
	if o.merged {
		ts = MergedTruth(fr.PartsRGB, fr.InstRGB, 6)
	}
	for _, t := range ts {
		ds = append(ds, infer.Detection{Class: t.Class, Name: infer.ClassNames[t.Class], Conf: 0.9, Mask: t.Mask, Area: t.Area, CX: t.CX, CY: t.CY,
			Box: [4]float32{float32(t.Mask.X0), float32(t.Mask.Y0), float32(t.Mask.X0 + t.Mask.W), float32(t.Mask.Y0 + t.Mask.H)}})
	}
	return ds, infer.Timing{Tiles: 6}, nil
}

func testWorld(t *testing.T) (*vcam.World, *scenario.Scenario) {
	t.Helper()
	sc := scenario.DefaultConfig()
	sc.Scenario.ID = "eval-test"
	sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 10, 12, 50
	sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 3, 3
	sc.Render.GSDm = 0.03
	s, err := scenario.Generate(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s.Save(dir)
	if _, err := render.RenderAll(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	w, err := vcam.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return w, s
}

func TestEvaluatePerfectDetector(t *testing.T) {
	w, s := testWorld(t)
	frames := dataset.TestFrames(dataset.Default(), s)
	o := DefaultOptions()
	o.Overlays = filepath.Join(t.TempDir(), "ov")
	r, err := Evaluate(w, frames, &oracle{w: w, frames: frames}, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.Text())
	t.Logf("geo true: %+v", r.GeoTrue.Overall)
	if r.Overall.Truth == 0 || r.Overall.Recall != 1 || r.Overall.Precision != 1 || r.Overall.AP50 != 1 {
		t.Fatalf("perfect detector: %+v", r.Overall)
	}
	if r.GeoTrue.VictimsFound != r.GeoTrue.VictimsTotal || r.GeoTrue.VictimsTotal != 3 {
		t.Fatalf("victims found with the true pose: %d/%d", r.GeoTrue.VictimsFound, r.GeoTrue.VictimsTotal)
	}
	if r.GeoTrue.ErrMedianM > 1.5 {
		t.Fatalf("median geolocation error with the true pose %.2f m", r.GeoTrue.ErrMedianM)
	}
	if len(r.Kinds) != 4 {
		t.Fatalf("kinds %+v", r.Kinds)
	}
	ov, _ := os.ReadDir(o.Overlays)
	if len(ov) == 0 {
		t.Fatal("no overlays written")
	}
}

// A detector that outlines each visible part once (pieces merged) is perfect in
// the merged view: every part and every person found, a diagonal confusion
// table and no false alarms. One that outlines every piece separately still
// finds every part and every person.
func TestMergedView(t *testing.T) {
	w, s := testWorld(t)
	frames := dataset.TestFrames(dataset.Default(), s)
	o := DefaultOptions()
	r, err := Evaluate(w, frames, &oracle{w: w, frames: frames, merged: true}, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Merged
	t.Log("\n" + m.Text())
	if r.Method != Method || m == nil {
		t.Fatalf("method %d, merged %v", r.Method, m != nil)
	}
	if m.Overall.Truth == 0 || m.Overall.Recall != 1 || m.Overall.Precision != 1 {
		t.Fatalf("merged oracle: %+v", m.Overall)
	}
	if v := m.VictimsFor("all"); v.Visible == 0 || v.Recall != 1 {
		t.Fatalf("people found %+v", v)
	}
	diag := 0
	for i, row := range m.Confusion.Counts {
		for j, c := range row {
			if i == j {
				diag += c
			} else if c != 0 {
				t.Fatalf("confusion off the diagonal: true %s -> %s %d", m.Confusion.Labels[i], m.Confusion.Labels[j], c)
			}
		}
	}
	if diag != m.Overall.Truth {
		t.Fatalf("confusion diagonal %d, truth %d", diag, m.Overall.Truth)
	}
	for c, n := range m.Confusion.FalseAlarms {
		if n != 0 {
			t.Fatalf("false alarms for %s: %d", m.Confusion.Labels[c], n)
		}
	}
	if r.Overall.Truth < m.Overall.Truth {
		t.Fatalf("pieces (%d) must be at least the merged parts (%d)", r.Overall.Truth, m.Overall.Truth)
	}

	r2, err := Evaluate(w, frames, &oracle{w: w, frames: frames, minPx: 1}, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sz := range r2.Merged.Sizes {
		if sz.Truth > 0 && sz.RecallAny != 1 {
			t.Fatalf("piece oracle must touch every merged part: %+v", sz)
		}
	}
	if v := r2.Merged.VictimsFor("all"); v.Recall != 1 {
		t.Fatalf("piece oracle people found %+v", v)
	}
	if pi := r2.Merged.VictimsPrefix("investigator"); pi.Visible == 0 {
		t.Fatalf("no investigator views counted: %+v", r2.Merged.Victims)
	}
}
