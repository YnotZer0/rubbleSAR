package infer

import (
	"fmt"
	"math"
	"testing"

	"rubblesar/internal/raster"
	"rubblesar/internal/tiles"
)

// synth builds a 1-image YOLO-seg output with the given candidates.
type synthCand struct {
	cx, cy, w, h float32
	cls          int
	score        float32
	coef0        float32
}

func synth(size, nc int, cs []synthCand, proto func(gx, gy int) float32) *RawOutput {
	const nm = 32
	A := 50
	C := 4 + nc + nm
	mh, mw := size/4, size/4
	o := &RawOutput{B: 1, C: C, A: A, NM: nm, MH: mh, MW: mw, Preds: make([]float32, C*A), Protos: make([]float32, nm*mh*mw)}
	for a, c := range cs {
		o.Preds[0*A+a], o.Preds[1*A+a], o.Preds[2*A+a], o.Preds[3*A+a] = c.cx, c.cy, c.w, c.h
		o.Preds[(4+c.cls)*A+a] = c.score
		o.Preds[(4+nc)*A+a] = c.coef0
	}
	for gy := 0; gy < mh; gy++ {
		for gx := 0; gx < mw; gx++ {
			o.Protos[gy*mw+gx] = proto(gx, gy)
		}
	}
	return o
}

func TestDecodeThresholdNMSAndMask(t *testing.T) {
	// Prototype 0 is positive on input pixels [80,120) x [96,104) (grid cells 20..29 x 24..25).
	proto := func(gx, gy int) float32 {
		if gx >= 20 && gx < 30 && gy >= 24 && gy < 26 {
			return 5
		}
		return -5
	}
	o := synth(640, 5, []synthCand{
		{100, 100, 60, 30, 1, 0.9, 1}, // kept, mask = the band inside the box
		{102, 101, 58, 28, 1, 0.8, 1}, // same class, overlaps: suppressed
		{103, 100, 58, 28, 2, 0.7, 1}, // other class at the same place: kept
		{400, 400, 20, 20, 3, 0.2, 1}, // below the score threshold
		{630, 630, 40, 40, 0, 0.5, 0}, // clipped at the edge, empty mask
	}, proto)
	if err := o.Check(5); err != nil {
		t.Fatal(err)
	}
	ds := Decode(o, 0, DefaultParams(), 640)
	if len(ds) != 3 {
		t.Fatalf("%d detections, want 3: %+v", len(ds), ds)
	}
	d := ds[0]
	if d.Class != 1 || d.Name != "arm" || d.Conf != 0.9 || d.Box != [4]float32{70, 85, 130, 115} {
		t.Fatalf("first detection %+v", d)
	}
	// The band is 40 x 8 px; bilinear edges move it by at most a pixel each side.
	if d.Area < 30*6 || d.Area > 44*10 {
		t.Fatalf("mask area %d, want about 320", d.Area)
	}
	if math.Abs(d.CX-100) > 1 || math.Abs(d.CY-100) > 1 {
		t.Fatalf("mask centroid %.2f,%.2f want 100,100", d.CX, d.CY)
	}
	edge := ds[2]
	if edge.Class != 0 || edge.Box[2] != 640 || edge.Box[3] != 640 || edge.Area != 0 || edge.CX != 625 {
		t.Fatalf("edge detection %+v", edge)
	}
	// Wrong class count is reported.
	if err := o.Check(80); err == nil {
		t.Fatal("80-class check passed on a 5-class output")
	}
}

func TestToCHW(t *testing.T) {
	m := raster.NewRGB(4, 3)
	m.Set(1, 2, [3]uint8{255, 128, 0})
	dst := make([]float32, 3*4*4)
	ToCHW(dst, m, tiles.Tile{X: 0, Y: 0, Size: 4})
	i := 2*4 + 1
	if dst[i] != 1 || math.Abs(float64(dst[16+i])-128.0/255) > 1e-6 || dst[32+i] != 0 {
		t.Fatalf("pixel values %v %v %v", dst[i], dst[16+i], dst[32+i])
	}
	if dst[3*4+3] != 0 { // padding row
		t.Fatal("padding not zero")
	}
}

func paint(m *raster.RGB, x0, y0, x1, y1 int, c [3]uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			m.Set(x, y, c)
		}
	}
}

func TestDetectFrameTilesAndMerges(t *testing.T) {
	m := raster.NewRGB(1920, 1080)
	paint(m, 0, 0, 1920, 1080, [3]uint8{90, 90, 90})
	red, blue := [3]uint8{255, 0, 0}, [3]uint8{0, 0, 255}
	paint(m, 100, 100, 140, 120, red)  // inside tile 0 only
	paint(m, 900, 500, 960, 540, blue) // inside tiles 1 and 4 (rows overlap from y=440 to 640)
	f := &Fake{Size: 640, Batch: 4, Colours: [][3]float32{{1, 0, 0}, {0, 0, 1}}}
	p := DefaultParams()
	p.NumClasses = 2
	d := &Detector{B: f, P: p}
	ds, tm, err := d.DetectFrame(m)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Tiles != 6 || tm.Batches != 2 || f.Calls != 2 {
		t.Fatalf("timing %+v calls %d", tm, f.Calls)
	}
	if len(ds) != 2 {
		t.Fatalf("%d detections after merging, want 2: %+v", len(ds), ds)
	}
	for _, x := range ds {
		var want [4]float32
		switch x.Class {
		case 0:
			want = [4]float32{100, 100, 140, 120}
		case 1:
			want = [4]float32{900, 500, 960, 540}
		}
		if x.Box != want {
			t.Errorf("class %d box %v want %v", x.Class, x.Box, want)
		}
		// Masks come from 4x4 prototype cells: area within a cell row/column of the truth.
		truth := float64((want[2] - want[0]) * (want[3] - want[1]))
		if math.Abs(float64(x.Area)-truth) > 0.35*truth {
			t.Errorf("class %d mask area %d, truth %.0f", x.Class, x.Area, truth)
		}
		cx, cy := float64(want[0]+want[2])/2, float64(want[1]+want[3])/2
		if math.Hypot(x.CX-cx, x.CY-cy) > 2 {
			t.Errorf("class %d centroid %.1f,%.1f want %.1f,%.1f", x.Class, x.CX, x.CY, cx, cy)
		}
		if x.Mask.X0 < int(want[0])-1 || x.Mask.Y0 < int(want[1])-1 {
			t.Errorf("mask not in frame coordinates: %d,%d", x.Mask.X0, x.Mask.Y0)
		}
	}
}

func TestMergePrefersCompleteDetections(t *testing.T) {
	a := Detection{Class: 2, Conf: 0.9, Box: [4]float32{630, 100, 640, 130}, Tile: 0, border: true}
	b := Detection{Class: 2, Conf: 0.8, Box: [4]float32{620, 100, 660, 130}, Tile: 1}
	c := Detection{Class: 3, Conf: 0.7, Box: [4]float32{620, 100, 660, 130}, Tile: 1}
	out := Merge([]Detection{a, b, c}, 0.5)
	if len(out) != 2 || out[0].Box != b.Box || out[1].Class != 3 {
		t.Fatalf("merge result %+v", out)
	}
}

func TestGoldenRoundTripAndParity(t *testing.T) {
	f := &Fake{Size: 64, Batch: 1, Colours: [][3]float32{{1, 0, 0}, {0, 0, 1}}}
	p := DefaultParams()
	p.NumClasses = 2
	var tl []*raster.RGB
	var names []string
	for i := 0; i < 3; i++ {
		m := raster.NewRGB(64, 64)
		paint(m, 5+i*3, 8, 25+i*3, 20, [3]uint8{255, 0, 0})
		paint(m, 40, 40, 52, 60, [3]uint8{0, 0, 255})
		tl = append(tl, m)
		names = append(names, fmt.Sprintf("t%d.png", i))
	}
	g, err := MakeGolden(f, tl, names, p, "abc")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := g.Save(dir, tl); err != nil {
		t.Fatal(err)
	}
	g2, tl2, err := LoadGolden(dir)
	if err != nil || len(tl2) != 3 || len(g2.Tiles[0].Detections) != 2 {
		t.Fatalf("reload: %v %d", err, len(tl2))
	}
	par, err := CheckParity(g2, tl2, f)
	if err != nil {
		t.Fatal(err)
	}
	if !par.ParityOK() || par.Matched != 6 || par.MaxScoreDiff != 0 {
		t.Fatalf("self parity %+v", par)
	}
	// A backend that misses the blue class fails parity.
	miss := &Fake{Size: 64, Batch: 1, Colours: [][3]float32{{1, 0, 0}, {0, 1, 0}}}
	par, _ = CheckParity(g2, tl2, miss)
	if par.ParityOK() || par.Matched != 3 || len(par.Problems) != 3 {
		t.Fatalf("parity with a broken backend %+v", par)
	}
}
