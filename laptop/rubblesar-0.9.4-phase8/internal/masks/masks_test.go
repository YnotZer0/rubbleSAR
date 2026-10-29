package masks

import (
	"math/rand"
	"testing"
)

func blob(rng *rand.Rand, w, h int) []uint32 {
	img := make([]uint32, w*h)
	// A few overlapping discs and bars of two keys.
	for k := uint32(1); k <= 2; k++ {
		for n := 0; n < 4; n++ {
			cx, cy, r := rng.Intn(w), rng.Intn(h), 1+rng.Intn(6)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
						img[y*w+x] = k
					}
				}
			}
		}
	}
	return img
}

// fillHoles returns the component's mask with interior holes filled.
func fillHoles(b Bitmap) Bitmap {
	out := Bitmap{X0: b.X0, Y0: b.Y0, W: b.W, H: b.H, Pix: make([]bool, len(b.Pix))}
	outside := make([]bool, len(b.Pix))
	var st []int
	for x := 0; x < b.W; x++ {
		st = append(st, x, (b.H-1)*b.W+x)
	}
	for y := 0; y < b.H; y++ {
		st = append(st, y*b.W, y*b.W+b.W-1)
	}
	for len(st) > 0 {
		i := st[len(st)-1]
		st = st[:len(st)-1]
		if outside[i] || b.Pix[i] {
			continue
		}
		outside[i] = true
		x, y := i%b.W, i/b.W
		// 4-connected background flood (complements 8-connected foreground).
		if x > 0 {
			st = append(st, i-1)
		}
		if x < b.W-1 {
			st = append(st, i+1)
		}
		if y > 0 {
			st = append(st, i-b.W)
		}
		if y < b.H-1 {
			st = append(st, i+b.W)
		}
	}
	for i := range out.Pix {
		out.Pix[i] = !outside[i]
	}
	return out
}

func TestTraceIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	checked := 0
	for trial := 0; trial < 200; trial++ {
		w, h := 30, 20
		img := blob(rng, w, h)
		for _, c := range Components(w, h, func(i int) uint32 { return img[i] }) {
			bm := c.Bitmap(w)
			poly := Trace(bm)
			if len(poly) < 4 {
				t.Fatalf("trial %d: polygon with %d vertices", trial, len(poly))
			}
			fp := make([][2]float64, len(poly))
			for i, p := range poly {
				fp[i] = [2]float64{float64(p[0]), float64(p[1])}
			}
			got := Fill(fp, bm.X0, bm.Y0, bm.W, bm.H)
			want := fillHoles(bm)
			for i := range want.Pix {
				if got.Pix[i] != want.Pix[i] {
					t.Fatalf("trial %d key %d: filled outline differs from the component at %d,%d (area %d)\npoly %v",
						trial, c.Key, i%bm.W+bm.X0, i/bm.W+bm.Y0, c.Area(), poly)
				}
			}
			checked++
		}
	}
	if checked < 300 {
		t.Fatalf("only %d components checked", checked)
	}
}

func TestComponentsEightConnected(t *testing.T) {
	// Two diagonal pixels form one component; a different key does not join.
	w, h := 5, 4
	img := []uint32{
		1, 0, 0, 0, 2,
		0, 1, 0, 0, 2,
		0, 0, 0, 1, 0,
		0, 0, 0, 0, 0,
	}
	cs := Components(w, h, func(i int) uint32 { return img[i] })
	if len(cs) != 3 {
		t.Fatalf("%d components, want 3", len(cs))
	}
	if cs[0].Key != 1 || cs[0].Area() != 2 || cs[0].X0 != 0 || cs[0].X1 != 2 || cs[0].CX != 1 || cs[0].CY != 1 {
		t.Fatalf("first component %+v", cs[0])
	}
	if Components(3, 3, func(int) uint32 { return 0 }) != nil {
		t.Fatal("empty image has components")
	}
}

func TestSimplify(t *testing.T) {
	// A staircase diagonal collapses; the square's corners survive.
	var poly [][2]float64
	for i := 0; i < 10; i++ {
		poly = append(poly, [2]float64{float64(i), float64(i)}, [2]float64{float64(i + 1), float64(i)})
	}
	poly = append(poly, [2]float64{10, 10}, [2]float64{0, 10})
	s := Simplify(poly, 0.8)
	if len(s) > 5 || len(s) < 3 {
		t.Fatalf("simplified to %d vertices: %v", len(s), s)
	}
	sq := [][2]float64{{0, 0}, {5, 0}, {5, 5}, {0, 5}}
	if len(Simplify(sq, 0.5)) != 4 {
		t.Fatal("square lost a corner")
	}
}

func TestIoU(t *testing.T) {
	a := Fill([][2]float64{{0, 0}, {4, 0}, {4, 4}, {0, 4}}, 0, 0, 10, 10)
	b := Fill([][2]float64{{2, 0}, {6, 0}, {6, 4}, {2, 4}}, 0, 0, 10, 10)
	if a.Area() != 16 || b.Area() != 16 {
		t.Fatalf("areas %d %d", a.Area(), b.Area())
	}
	if v := IoU(&a, &b); v < 0.333 || v > 0.334 {
		t.Fatalf("IoU %v want 1/3", v)
	}
}

func TestMergeKeepsAllPieces(t *testing.T) {
	// Two pieces of key 7 separated by a gap, plus another key.
	w, h := 12, 4
	img := []uint32{
		7, 7, 0, 0, 0, 0, 0, 0, 0, 7, 0, 3,
		7, 7, 0, 0, 0, 0, 0, 0, 0, 7, 0, 3,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	cs := Components(w, h, func(i int) uint32 { return img[i] })
	var sevens []Component
	for _, c := range cs {
		if c.Key == 7 {
			sevens = append(sevens, c)
		}
	}
	if len(sevens) != 2 {
		t.Fatalf("want 2 pieces of key 7, got %d", len(sevens))
	}
	m := Merge(sevens)
	b := m.Bitmap(w)
	if m.Area() != 6 || b.Area() != 6 || m.X0 != 0 || m.X1 != 10 || m.Y1 != 2 {
		t.Fatalf("merged %+v area %d", m, b.Area())
	}
	// Centroid: 4 px around x=1, 2 px at x=9.5 -> (4*1 + 2*9.5)/6
	if want := (4*1.0 + 2*9.5) / 6; abs(m.CX-want) > 1e-9 {
		t.Fatalf("centroid %.3f want %.3f", m.CX, want)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// Bridging the traced outlines of the pieces of a key and filling the result
// gives exactly the union of the pieces (the joins enclose no pixel centre).
func TestBridgeFillsUnion(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	checked := 0
	for trial := 0; trial < 300; trial++ {
		w, h := 40, 30
		img := make([]uint32, w*h)
		// A few blobs of key 1, kept apart by at least one empty pixel.
		for b := 0; b < 2+rng.Intn(3); b++ {
			cx, cy := rng.Intn(w-6)+3, rng.Intn(h-6)+3
			rx, ry := 1+rng.Intn(3), 1+rng.Intn(3)
			for y := cy - ry; y <= cy+ry; y++ {
				for x := cx - rx; x <= cx+rx; x++ {
					if rng.Float64() < 0.85 {
						img[y*w+x] = 1
					}
				}
			}
		}
		cs := Components(w, h, func(i int) uint32 { return img[i] })
		if len(cs) < 2 {
			continue
		}
		var rings [][][2]float64
		var areas []int
		union := map[[2]int]bool{}
		for _, c := range cs {
			bm := c.Bitmap(w)
			// holes are filled by Trace: count the filled piece as the expected area
			r := Trace(bm)
			pf := make([][2]float64, len(r))
			for i, p := range r {
				pf[i] = [2]float64{float64(p[0]), float64(p[1])}
			}
			f := Fill(pf, 0, 0, w, h)
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					if f.At(x, y) {
						union[[2]int{x, y}] = true
					}
				}
			}
			rings = append(rings, pf)
			areas = append(areas, c.Area())
		}
		ord := ChainOrder(rings, areas)
		chain := make([][][2]float64, len(ord))
		for i, k := range ord {
			chain[i] = rings[k]
		}
		bf := Fill(Bridge(chain), 0, 0, w, h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if bf.At(x, y) != union[[2]int{x, y}] {
					t.Fatalf("trial %d: pixel %d,%d filled %v, union %v (%d pieces)", trial, x, y, bf.At(x, y), union[[2]int{x, y}], len(rings))
				}
			}
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d multi-piece cases checked", checked)
	}
}
