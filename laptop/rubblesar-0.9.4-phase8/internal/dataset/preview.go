package dataset

import (
	"bufio"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"rubblesar/internal/raster"
)

// ClassColours are the outline colours per model class.
var ClassColours = [][3]uint8{{255, 64, 64}, {255, 170, 0}, {64, 160, 255}, {0, 230, 120}, {200, 80, 255}}

// ReadLabels parses a YOLO segmentation label file into class + polygons (pixels).
func ReadLabels(path string, size int) ([]Instance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Instance
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 7 || len(fs)%2 == 0 {
			continue
		}
		cl, err := strconv.Atoi(fs[0])
		if err != nil {
			continue
		}
		in := Instance{Class: cl}
		for i := 1; i+1 < len(fs); i += 2 {
			x, _ := strconv.ParseFloat(fs[i], 64)
			y, _ := strconv.ParseFloat(fs[i+1], 64)
			in.Poly = append(in.Poly, [2]float64{x * float64(size), y * float64(size)})
		}
		out = append(out, in)
	}
	return out, sc.Err()
}

func drawLine(m *image.NRGBA, x0, y0, x1, y1 float64, c [3]uint8) {
	n := int(math.Max(math.Abs(x1-x0), math.Abs(y1-y0))*2) + 1
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		x, y := int(x0+(x1-x0)*t), int(y0+(y1-y0)*t)
		if image.Pt(x, y).In(m.Rect) {
			o := m.PixOffset(x, y)
			m.Pix[o], m.Pix[o+1], m.Pix[o+2], m.Pix[o+3] = c[0], c[1], c[2], 255
		}
	}
}

// DrawInstances outlines label polygons on an image (scale maps polygon pixels to image pixels).
func DrawInstances(m *image.NRGBA, ins []Instance, scale float64) {
	for _, in := range ins {
		c := ClassColours[in.Class%len(ClassColours)]
		for i := range in.Poly {
			a, b := in.Poly[i], in.Poly[(i+1)%len(in.Poly)]
			drawLine(m, a[0]*scale, a[1]*scale, b[0]*scale, b[1]*scale, c)
		}
	}
}

// Preview draws n random labelled body parts from the training tiles into
// one contact sheet: a 160 px window around each part, enlarged twice, with
// every label outline drawn in its class colour.
func Preview(root, out string, n int, seed int64) error {
	files, err := filepath.Glob(filepath.Join(root, "labels", "train", "*.txt"))
	if err != nil || len(files) == 0 {
		return err
	}
	sort.Strings(files)
	var withParts []string
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
			withParts = append(withParts, f)
		}
	}
	if len(withParts) == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(withParts), func(i, j int) { withParts[i], withParts[j] = withParts[j], withParts[i] })
	if len(withParts) > n {
		withParts = withParts[:n]
	}
	const win, zoom = 160, 2
	const cell = win * zoom
	cols := 6
	rows := (len(withParts) + cols - 1) / cols
	sheet := image.NewNRGBA(image.Rect(0, 0, cols*cell, rows*cell))
	for i, lf := range withParts {
		stem := strings.TrimSuffix(filepath.Base(lf), ".txt")
		f, err := os.Open(filepath.Join(root, "images", "train", stem+".jpg"))
		if err != nil {
			continue
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		size := img.Bounds().Dx()
		ins, _ := ReadLabels(lf, size)
		if len(ins) == 0 {
			continue
		}
		// Centre the window on one random part.
		pick := ins[rng.Intn(len(ins))]
		var cx, cy float64
		for _, p := range pick.Poly {
			cx += p[0]
			cy += p[1]
		}
		cx, cy = cx/float64(len(pick.Poly)), cy/float64(len(pick.Poly))
		x0 := int(math.Max(0, math.Min(float64(size-win), cx-win/2)))
		y0 := int(math.Max(0, math.Min(float64(size-win), cy-win/2)))
		th := image.NewNRGBA(image.Rect(0, 0, cell, cell))
		for y := 0; y < cell; y++ {
			for x := 0; x < cell; x++ {
				r, g, b, _ := img.At(x0+x/zoom, y0+y/zoom).RGBA()
				o := th.PixOffset(x, y)
				th.Pix[o], th.Pix[o+1], th.Pix[o+2], th.Pix[o+3] = uint8(r>>8), uint8(g>>8), uint8(b>>8), 255
			}
		}
		shifted := make([]Instance, len(ins))
		for k, in := range ins {
			shifted[k] = Instance{Class: in.Class}
			for _, p := range in.Poly {
				shifted[k].Poly = append(shifted[k].Poly, [2]float64{p[0] - float64(x0), p[1] - float64(y0)})
			}
		}
		DrawInstances(th, shifted, zoom)
		draw.Draw(sheet, image.Rect((i%cols)*cell, (i/cols)*cell, (i%cols+1)*cell, (i/cols+1)*cell), th, image.Point{}, draw.Src)
	}
	return raster.SavePNG(out, sheet)
}
