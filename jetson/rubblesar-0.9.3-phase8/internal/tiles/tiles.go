// Package tiles cuts camera frames into the square tiles the segmentation
// model sees. Training (dataset) and inference (infer) use the same plan, so
// the model always sees body parts at their native pixel size.
package tiles

import "fmt"

// Tile is one square window of a frame.
type Tile struct {
	X, Y int // top-left corner in frame pixels
	Size int
}

// Plan lists the tiles covering a W x H frame with square tiles of the given
// size. Tiles overlap evenly where the frame is not a multiple of the size,
// and every pixel is covered. A frame smaller than a tile gives one tile
// (the caller pads it).
func Plan(w, h, size int) []Tile {
	xs := starts(w, size)
	ys := starts(h, size)
	out := make([]Tile, 0, len(xs)*len(ys))
	for _, y := range ys {
		for _, x := range xs {
			out = append(out, Tile{X: x, Y: y, Size: size})
		}
	}
	return out
}

// starts returns evenly spread start offsets along one axis.
func starts(n, size int) []int {
	if n <= size {
		return []int{0}
	}
	k := (n + size - 1) / size // tiles needed
	out := make([]int, k)
	for i := 0; i < k; i++ {
		// Spread the overlap evenly; the last tile ends exactly at n.
		out[i] = int(float64(i) * float64(n-size) / float64(k-1))
	}
	return out
}

// String describes a plan, e.g. "6 tiles of 640 (3 x 2)".
func Describe(w, h, size int) string {
	return fmt.Sprintf("%d tiles of %d px (%d x %d) for %dx%d", len(starts(w, size))*len(starts(h, size)), size, len(starts(w, size)), len(starts(h, size)), w, h)
}
