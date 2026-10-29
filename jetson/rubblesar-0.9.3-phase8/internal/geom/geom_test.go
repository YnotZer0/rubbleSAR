package geom

import (
	"math"
	"testing"
)

func TestRotRect(t *testing.T) {
	r := RotRect(P{10, 20}, 30, 10, 0) // long axis north-south
	b := r.Bounds()
	if math.Abs(b.Max.Y-b.Min.Y-30) > 1e-9 || math.Abs(b.Max.X-b.Min.X-10) > 1e-9 {
		t.Fatalf("heading 0 bounds %+v", b)
	}
	r = RotRect(P{0, 0}, 30, 10, 90) // long axis east-west
	b = r.Bounds()
	if math.Abs(b.Max.X-b.Min.X-30) > 1e-9 || math.Abs(b.Max.Y-b.Min.Y-10) > 1e-9 {
		t.Fatalf("heading 90 bounds %+v", b)
	}
	if math.Abs(r.Area()-300) > 1e-9 {
		t.Fatalf("area %v", r.Area())
	}
	if c := RotRect(P{5, -7}, 12, 4, 33).Centroid(); c.Dist(P{5, -7}) > 1e-9 {
		t.Fatalf("centroid %v", c)
	}
}

func TestContainsAndDistances(t *testing.T) {
	sq := Poly{{0, 0}, {10, 0}, {10, 10}, {0, 10}}
	if !sq.Contains(P{5, 5}) || sq.Contains(P{11, 5}) || sq.Contains(P{-0.1, 5}) {
		t.Fatal("contains")
	}
	if d := sq.SignedDist(P{5, 5}); math.Abs(d+5) > 1e-9 {
		t.Fatalf("signed inside %v", d)
	}
	if d := sq.SignedDist(P{13, 14}); math.Abs(d-5) > 1e-9 {
		t.Fatalf("signed outside %v", d)
	}
	other := Poly{{15, 0}, {20, 0}, {20, 5}, {15, 5}}
	if d := PolyDist(sq, other); math.Abs(d-5) > 1e-9 {
		t.Fatalf("PolyDist %v", d)
	}
	if PolyDist(sq, Poly{{5, 5}, {6, 5}, {6, 6}}) != 0 {
		t.Fatal("contained polygon distance must be 0")
	}
	d, tt := SegDist(P{5, 3}, P{0, 0}, P{10, 0})
	if d != 3 || tt != 0.5 {
		t.Fatalf("SegDist %v %v", d, tt)
	}
}

func TestBox(t *testing.T) {
	a := Box{P{0, 0}, P{1, 1}}
	if !a.Overlaps(Box{P{1, 1}, P{2, 2}}) || a.Overlaps(Box{P{1.1, 0}, P{2, 1}}) || !a.Grow(0.2).ContainsP(P{1.1, -0.1}) {
		t.Fatal("box ops")
	}
}
