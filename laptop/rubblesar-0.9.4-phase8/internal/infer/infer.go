// Package infer runs the body-part segmentation model on camera frames:
// tiling, pre-processing, the model call through a backend (TensorRT on the
// Jetson, ONNX Runtime on either machine), decoding of the YOLO11-seg outputs,
// non-maximum suppression, instance masks and merging of tiles into frame
// detections. Everything except the backends is pure Go and unit-tested.
package infer

import (
	"fmt"
	"math"
	"sort"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/tiles"
)

// ClassNames are the model's classes (YOLO order).
var ClassNames = []string{"hand", "arm", "leg", "head", "torso"}

// RawOutput holds the two YOLO11-seg output tensors of a batch.
//
//	Preds  [B][4+NC+NM][A]  box (cx, cy, w, h in input pixels), class scores (0..1), mask coefficients
//	Protos [B][NM][MH][MW]  mask prototypes at 1/4 of the input size
type RawOutput struct {
	B, C, A    int
	Preds      []float32
	NM, MH, MW int
	Protos     []float32
}

// Check validates the shapes against the expected class count.
func (o *RawOutput) Check(nc int) error {
	if o.B < 1 || o.A < 1 || o.NM < 1 || o.MH < 1 || o.MW < 1 {
		return diag.New("E-INF-003", "empty output: preds %dx%dx%d, protos %dx%dx%dx%d", o.B, o.C, o.A, o.B, o.NM, o.MH, o.MW)
	}
	if o.C != 4+nc+o.NM {
		return diag.New("E-INF-003", "output0 has %d rows; a YOLO11-seg model with %d classes and %d mask coefficients has %d", o.C, nc, o.NM, 4+nc+o.NM)
	}
	if len(o.Preds) != o.B*o.C*o.A || len(o.Protos) != o.B*o.NM*o.MH*o.MW {
		return diag.New("E-INF-003", "output sizes disagree with their shapes")
	}
	return nil
}

// Backend runs the network.
type Backend interface {
	Name() string
	InputSize() int // square input, pixels
	MaxBatch() int
	// Infer runs batch images, given as B x 3 x S x S float32 (RGB, 0..1).
	Infer(input []float32, batch int) (*RawOutput, error)
	Close() error
}

// Params are the decoding thresholds.
type Params struct {
	NumClasses int
	Conf       float64 // minimum class score
	IoU        float64 // NMS overlap within a tile (same class)
	MergeIoU   float64 // NMS overlap when merging tiles
	MaskThresh float64 // mask probability threshold
	MaxDet     int     // per tile
}

// DefaultParams are the settings used everywhere unless changed.
func DefaultParams() Params {
	return Params{NumClasses: 5, Conf: 0.25, IoU: 0.6, MergeIoU: 0.5, MaskThresh: 0.5, MaxDet: 100}
}

// Detection is one body part found in a frame.
type Detection struct {
	Class  int          `json:"class"`
	Name   string       `json:"class_name"`
	Conf   float32      `json:"conf"`
	Box    [4]float32   `json:"box_xyxy"` // frame pixels
	Area   int          `json:"mask_px"`
	CX     float64      `json:"cx"` // mask centroid (box centre if the mask is empty), frame pixels
	CY     float64      `json:"cy"`
	Tile   int          `json:"tile"`
	Mask   masks.Bitmap `json:"-"` // frame coordinates
	border bool
}

// Timing reports where the time went for one frame.
type Timing struct {
	PreMS, InferMS, PostMS float64
	Tiles, Batches         int
}

// TotalMS is the whole frame time.
func (t Timing) TotalMS() float64 { return t.PreMS + t.InferMS + t.PostMS }

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ToCHW appends a tile of the frame as planar float32 RGB scaled to 0..1.
func ToCHW(dst []float32, m *raster.RGB, t tiles.Tile) {
	s := t.Size
	plane := s * s
	for y := 0; y < s; y++ {
		fy := t.Y + y
		for x := 0; x < s; x++ {
			fx := t.X + x
			var r, g, b float32
			if fx < m.W && fy < m.H {
				i := 3 * (fy*m.W + fx)
				r, g, b = float32(m.Pix[i])/255, float32(m.Pix[i+1])/255, float32(m.Pix[i+2])/255
			}
			o := y*s + x
			dst[o], dst[plane+o], dst[2*plane+o] = r, g, b
		}
	}
}

func iou(a, b [4]float32) float64 {
	ix0, iy0 := math.Max(float64(a[0]), float64(b[0])), math.Max(float64(a[1]), float64(b[1]))
	ix1, iy1 := math.Min(float64(a[2]), float64(b[2])), math.Min(float64(a[3]), float64(b[3]))
	iw, ih := math.Max(0, ix1-ix0), math.Max(0, iy1-iy0)
	inter := iw * ih
	ua := float64((a[2]-a[0])*(a[3]-a[1])) + float64((b[2]-b[0])*(b[3]-b[1])) - inter
	if ua <= 0 {
		return 0
	}
	return inter / ua
}

// ios is the intersection over the smaller box.
func ios(a, b [4]float32) float64 {
	ix0, iy0 := math.Max(float64(a[0]), float64(b[0])), math.Max(float64(a[1]), float64(b[1]))
	ix1, iy1 := math.Min(float64(a[2]), float64(b[2])), math.Min(float64(a[3]), float64(b[3]))
	inter := math.Max(0, ix1-ix0) * math.Max(0, iy1-iy0)
	sa := float64((a[2] - a[0]) * (a[3] - a[1]))
	sb := float64((b[2] - b[0]) * (b[3] - b[1]))
	s := math.Min(sa, sb)
	if s <= 0 {
		return 0
	}
	return inter / s
}

type cand struct {
	cls   int
	score float32
	box   [4]float32
	a     int
}

// Decode turns image b of a raw output into detections in input (tile)
// pixel coordinates: score threshold, class-aware NMS, then masks.
func Decode(o *RawOutput, b int, p Params, size int) []Detection {
	nc := p.NumClasses
	C, A := o.C, o.A
	base := b * C * A
	at := func(row, a int) float32 { return o.Preds[base+row*A+a] }
	var cs []cand
	for a := 0; a < A; a++ {
		best, bc := float32(-1), -1
		for c := 0; c < nc; c++ {
			if s := at(4+c, a); s > best {
				best, bc = s, c
			}
		}
		if float64(best) < p.Conf {
			continue
		}
		cx, cy, w, h := at(0, a), at(1, a), at(2, a), at(3, a)
		bx := [4]float32{cx - w/2, cy - h/2, cx + w/2, cy + h/2}
		for i := range bx {
			bx[i] = float32(math.Max(0, math.Min(float64(size), float64(bx[i]))))
		}
		if bx[2] <= bx[0] || bx[3] <= bx[1] {
			continue
		}
		cs = append(cs, cand{bc, best, bx, a})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	var keep []cand
	for _, c := range cs {
		ok := true
		for _, k := range keep {
			if k.cls == c.cls && iou(k.box, c.box) > p.IoU {
				ok = false
				break
			}
		}
		if ok {
			keep = append(keep, c)
			if len(keep) >= p.MaxDet {
				break
			}
		}
	}
	thr := float32(math.Log(p.MaskThresh / (1 - p.MaskThresh)))
	out := make([]Detection, 0, len(keep))
	for _, k := range keep {
		d := Detection{Class: k.cls, Name: name(k.cls), Conf: k.score, Box: k.box}
		coef := make([]float32, o.NM)
		for m := 0; m < o.NM; m++ {
			coef[m] = at(4+nc+m, k.a)
		}
		d.Mask = maskFor(o, b, coef, k.box, size, thr)
		finishMask(&d)
		out = append(out, d)
	}
	return out
}

func name(c int) string {
	if c >= 0 && c < len(ClassNames) {
		return ClassNames[c]
	}
	return fmt.Sprintf("class%d", c)
}

// maskFor builds the instance mask inside the box at input resolution by
// bilinear sampling of the prototype combination (logits).
func maskFor(o *RawOutput, b int, coef []float32, box [4]float32, size int, thr float32) masks.Bitmap {
	x0, y0 := int(math.Floor(float64(box[0]))), int(math.Floor(float64(box[1])))
	x1, y1 := int(math.Ceil(float64(box[2]))), int(math.Ceil(float64(box[3])))
	x0, y0 = max(0, x0), max(0, y0)
	x1, y1 = min(size, x1), min(size, y1)
	bm := masks.Bitmap{X0: x0, Y0: y0, W: x1 - x0, H: y1 - y0}
	if bm.W <= 0 || bm.H <= 0 {
		return bm
	}
	bm.Pix = make([]bool, bm.W*bm.H)
	sx, sy := float64(o.MW)/float64(size), float64(o.MH)/float64(size)
	// Logits on the prototype grid cells the box touches (plus a margin for interpolation).
	gx0 := max(0, int(math.Floor(float64(x0)*sx))-1)
	gy0 := max(0, int(math.Floor(float64(y0)*sy))-1)
	gx1 := min(o.MW, int(math.Ceil(float64(x1)*sx))+1)
	gy1 := min(o.MH, int(math.Ceil(float64(y1)*sy))+1)
	gw, gh := gx1-gx0, gy1-gy0
	logit := make([]float32, gw*gh)
	plane := o.MH * o.MW
	pb := b * o.NM * plane
	for m, c := range coef {
		if c == 0 {
			continue
		}
		pm := pb + m*plane
		for gy := 0; gy < gh; gy++ {
			row := o.Protos[pm+(gy0+gy)*o.MW+gx0 : pm+(gy0+gy)*o.MW+gx1]
			l := logit[gy*gw : (gy+1)*gw]
			for gx, v := range row {
				l[gx] += c * v
			}
		}
	}
	for y := y0; y < y1; y++ {
		fy := (float64(y)+0.5)*sy - 0.5 - float64(gy0)
		for x := x0; x < x1; x++ {
			fx := (float64(x)+0.5)*sx - 0.5 - float64(gx0)
			if bilin(logit, gw, gh, fx, fy) > thr {
				bm.Pix[(y-y0)*bm.W+(x-x0)] = true
			}
		}
	}
	return bm
}

func bilin(p []float32, w, h int, x, y float64) float32 {
	x = math.Max(0, math.Min(float64(w-1), x))
	y = math.Max(0, math.Min(float64(h-1), y))
	xi, yi := int(x), int(y)
	x1, y1 := min(xi+1, w-1), min(yi+1, h-1)
	fx, fy := float32(x-float64(xi)), float32(y-float64(yi))
	a := p[yi*w+xi]*(1-fx) + p[yi*w+x1]*fx
	b := p[y1*w+xi]*(1-fx) + p[y1*w+x1]*fx
	return a*(1-fy) + b*fy
}

func finishMask(d *Detection) {
	var sx, sy float64
	n := 0
	for i, v := range d.Mask.Pix {
		if v {
			sx += float64(d.Mask.X0+i%d.Mask.W) + 0.5
			sy += float64(d.Mask.Y0+i/d.Mask.W) + 0.5
			n++
		}
	}
	d.Area = n
	if n > 0 {
		d.CX, d.CY = sx/float64(n), sy/float64(n)
	} else {
		d.CX, d.CY = float64(d.Box[0]+d.Box[2])/2, float64(d.Box[1]+d.Box[3])/2
	}
}

// Detector ties a backend to the frame pipeline.
type Detector struct {
	B Backend
	P Params
}

// Name describes the backend.
func (d *Detector) Name() string { return d.B.Name() }

// DetectFrame tiles a frame, runs the model and merges the tiles.
func (d *Detector) DetectFrame(m *raster.RGB) ([]Detection, Timing, error) {
	var tm Timing
	size := d.B.InputSize()
	plan := tiles.Plan(m.W, m.H, size)
	tm.Tiles = len(plan)
	maxB := max(1, d.B.MaxBatch())
	var all []Detection
	for start := 0; start < len(plan); start += maxB {
		end := min(len(plan), start+maxB)
		n := end - start
		t0 := time.Now()
		in := make([]float32, n*3*size*size)
		for i := 0; i < n; i++ {
			ToCHW(in[i*3*size*size:(i+1)*3*size*size], m, plan[start+i])
		}
		t1 := time.Now()
		out, err := d.B.Infer(in, n)
		if err != nil {
			return nil, tm, err
		}
		if err := out.Check(d.P.NumClasses); err != nil {
			return nil, tm, err
		}
		t2 := time.Now()
		for i := 0; i < n; i++ {
			t := plan[start+i]
			for _, det := range Decode(out, i, d.P, size) {
				all = append(all, toFrame(det, t, start+i, m.W, m.H))
			}
		}
		tm.PreMS += ms(t1.Sub(t0))
		tm.InferMS += ms(t2.Sub(t1))
		tm.PostMS += ms(time.Since(t2))
		tm.Batches++
	}
	t3 := time.Now()
	res := Merge(all, d.P.MergeIoU)
	tm.PostMS += ms(time.Since(t3))
	return res, tm, nil
}

// toFrame shifts a tile detection into frame coordinates and notes whether it
// touches an inner tile edge (where the object may continue in a neighbour).
func toFrame(d Detection, t tiles.Tile, idx, fw, fh int) Detection {
	fx, fy := float32(t.X), float32(t.Y)
	d.Box = [4]float32{d.Box[0] + fx, d.Box[1] + fy, d.Box[2] + fx, d.Box[3] + fy}
	d.Mask.X0 += t.X
	d.Mask.Y0 += t.Y
	d.CX += float64(t.X)
	d.CY += float64(t.Y)
	d.Tile = idx
	const e = 2
	s := float32(t.Size)
	d.border = (t.X > 0 && d.Box[0]-fx < e) || (t.Y > 0 && d.Box[1]-fy < e) ||
		(t.X+t.Size < fw && fx+s-d.Box[2] < e) || (t.Y+t.Size < fh && fy+s-d.Box[3] < e)
	return d
}

// Merge removes duplicates where tiles overlap: same class and boxes that
// overlap (IoU above iouThr, or one mostly inside the other). Detections cut
// by an inner tile edge give way to complete ones.
func Merge(ds []Detection, iouThr float64) []Detection {
	rank := func(d Detection) float32 {
		if d.border {
			return d.Conf * 0.8
		}
		return d.Conf
	}
	sort.SliceStable(ds, func(i, j int) bool { return rank(ds[i]) > rank(ds[j]) })
	var keep []Detection
	for _, d := range ds {
		dup := false
		for _, k := range keep {
			if k.Class == d.Class && k.Tile != d.Tile && (iou(k.Box, d.Box) > iouThr || ios(k.Box, d.Box) > 0.7) {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, d)
		}
	}
	return keep
}
