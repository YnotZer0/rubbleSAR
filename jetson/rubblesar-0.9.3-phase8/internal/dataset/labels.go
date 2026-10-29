package dataset

import (
	"fmt"
	"strings"

	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/scenario"
	"rubblesar/internal/tiles"
)

// ClassNames are the model's classes in YOLO order (scenario part class - 1).
var ClassNames = []string{"hand", "arm", "leg", "head", "torso"}

// YOLOClass maps a scenario part class (1..5) to the model class (0..4).
func YOLOClass(part int) int { return part - 1 }

// Instance is one labelled body-part region inside a tile.
type Instance struct {
	Class  int // model class 0..4
	Victim int
	Area   int          // pixels
	Poly   [][2]float64 // tile pixel coordinates (pixel corners)
}

// YOLOLine formats the instance as an Ultralytics segmentation label line.
func (in Instance) YOLOLine(size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", in.Class)
	s := float64(size)
	for _, p := range in.Poly {
		fmt.Fprintf(&b, " %.6f %.6f", clamp01(p[0]/s), clamp01(p[1]/s))
	}
	return b.String()
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// TileInstances extracts the instances of one tile from a frame's label
// images: every 8-connected region of one victim and one part class with at
// least minPx pixels becomes an outline polygon, simplified by eps pixels.
func TileInstances(parts, inst *raster.U8, t tiles.Tile, minPx int, eps float64) []Instance {
	size := t.Size
	key := func(i int) uint32 {
		x, y := t.X+i%size, t.Y+i/size
		if x >= parts.W || y >= parts.H {
			return 0
		}
		p := parts.Pix[y*parts.W+x]
		if p == 0 || int(p) >= scenario.NumParts {
			return 0
		}
		return uint32(inst.Pix[y*parts.W+x])<<8 | uint32(p)
	}
	var out []Instance
	for _, c := range masks.Components(size, size, key) {
		if c.Area() < minPx {
			continue
		}
		bm := c.Bitmap(size)
		ring := masks.Trace(bm)
		poly := make([][2]float64, len(ring))
		for i, p := range ring {
			poly[i] = [2]float64{float64(p[0]), float64(p[1])}
		}
		poly = masks.Simplify(poly, eps)
		if len(poly) < 3 {
			continue
		}
		out = append(out, Instance{Class: YOLOClass(int(c.Key & 0xff)), Victim: int(c.Key >> 8), Area: c.Area(), Poly: poly})
	}
	return out
}

// CropRGB copies a tile out of a frame (zero padding beyond the frame).
func CropRGB(m *raster.RGB, t tiles.Tile) *raster.RGB {
	o := raster.NewRGB(t.Size, t.Size)
	for y := 0; y < t.Size; y++ {
		fy := t.Y + y
		if fy >= m.H {
			break
		}
		w := min(t.Size, m.W-t.X)
		copy(o.Pix[3*y*t.Size:3*(y*t.Size+w)], m.Pix[3*(fy*m.W+t.X):3*(fy*m.W+t.X+w)])
	}
	return o
}

// TileInstancesMerged is TileInstances with the pieces of each victim's body
// part merged into one instance: pieces of at least minPiecePx are traced,
// simplified and joined by zero-area bridges; the instance is kept when the
// pieces total at least minPx.
func TileInstancesMerged(parts, inst *raster.U8, t tiles.Tile, minPx, minPiecePx int, eps float64) []Instance {
	size := t.Size
	key := func(i int) uint32 {
		x, y := t.X+i%size, t.Y+i/size
		if x >= parts.W || y >= parts.H {
			return 0
		}
		p := parts.Pix[y*parts.W+x]
		if p == 0 || int(p) >= scenario.NumParts {
			return 0
		}
		return uint32(inst.Pix[y*parts.W+x])<<8 | uint32(p)
	}
	groups := map[uint32][]masks.Component{}
	var order []uint32
	for _, c := range masks.Components(size, size, key) {
		if c.Area() < minPiecePx {
			continue
		}
		if _, ok := groups[c.Key]; !ok {
			order = append(order, c.Key)
		}
		groups[c.Key] = append(groups[c.Key], c)
	}
	var out []Instance
	for _, k := range order {
		var rings [][][2]float64
		var areas []int
		total := 0
		for _, c := range groups[k] {
			bm := c.Bitmap(size)
			ring := masks.Trace(bm)
			poly := make([][2]float64, len(ring))
			for i, p := range ring {
				poly[i] = [2]float64{float64(p[0]), float64(p[1])}
			}
			poly = masks.Simplify(poly, eps)
			if len(poly) < 3 {
				continue
			}
			rings = append(rings, poly)
			areas = append(areas, c.Area())
			total += c.Area()
		}
		if total < minPx || len(rings) == 0 {
			continue
		}
		ord := masks.ChainOrder(rings, areas)
		chain := make([][][2]float64, len(ord))
		for i, j := range ord {
			chain[i] = rings[j]
		}
		out = append(out, Instance{Class: YOLOClass(int(k & 0xff)), Victim: int(k >> 8), Area: total, Poly: masks.Bridge(chain)})
	}
	return out
}
