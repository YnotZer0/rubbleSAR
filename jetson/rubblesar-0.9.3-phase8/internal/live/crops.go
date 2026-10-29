package live

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Crops keeps the latest detections' crops in memory and finds older ones in
// casualty-analyst's store (crops/<uav>/<id>.jpg and .png).
type Crops struct {
	// Dir is casualty-analyst's folder for the incident ([analyst] store_dir/<incident>); "" = memory only.
	Dir string
	// Max crops kept in memory (default 400).
	Max int

	mu    sync.Mutex
	order []string
	m     map[string]*cropPair
}

type cropPair struct{ visible, thermal []byte }

var detIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// ValidID reports whether id can be a detection id (and is safe in a path).
func ValidID(id string) bool {
	return detIDRE.MatchString(id) && !bytes.Contains([]byte(id), []byte(".."))
}

// Put keeps a detection's crops (either may be nil).
func (c *Crops) Put(id string, visible, thermal []byte) {
	if len(visible) == 0 && len(thermal) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*cropPair{}
	}
	if c.Max <= 0 {
		c.Max = 400
	}
	if _, ok := c.m[id]; !ok {
		c.order = append(c.order, id)
	}
	c.m[id] = &cropPair{visible, thermal}
	for len(c.order) > c.Max {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

// Visible is a detection's colour crop (JPEG).
func (c *Crops) Visible(id string) ([]byte, bool) { return c.get(id, true) }

// Thermal is a detection's thermal crop (16-bit grey PNG, kelvin x 10).
func (c *Crops) Thermal(id string) ([]byte, bool) { return c.get(id, false) }

func (c *Crops) get(id string, visible bool) ([]byte, bool) {
	if !ValidID(id) {
		return nil, false
	}
	c.mu.Lock()
	p := c.m[id]
	c.mu.Unlock()
	if p != nil {
		if visible && len(p.visible) > 0 {
			return p.visible, true
		}
		if !visible && len(p.thermal) > 0 {
			return p.thermal, true
		}
	}
	if c.Dir == "" {
		return nil, false
	}
	ext := ".png"
	if visible {
		ext = ".jpg"
	}
	// A recorded run keeps them flat (crops/<id>.jpg), casualty-analyst by drone (crops/<uav>/<id>.jpg).
	if b, err := os.ReadFile(filepath.Join(c.Dir, "crops", id+ext)); err == nil {
		return b, true
	}
	dirs, _ := os.ReadDir(filepath.Join(c.Dir, "crops"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(c.Dir, "crops", d.Name(), id+ext)); err == nil {
			return b, true
		}
	}
	return nil, false
}

// ThermalView turns a thermal crop (16-bit grey PNG, kelvin x 10) into a
// false-colour PNG between lo and hi degrees C (NaN: from the crop itself),
// enlarged to at least minSide pixels (bilinear).
func ThermalView(png16 []byte, lo, hi float64, minSide int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(png16))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, errors.New("empty thermal crop")
	}
	c := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := color.Gray16Model.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray16).Y
			c[y*w+x] = float64(v)/10 - 273.15
		}
	}
	if math.IsNaN(lo) || math.IsNaN(hi) || hi-lo < 1 {
		s := append([]float64(nil), c...)
		sort.Float64s(s)
		lo, hi = s[len(s)*2/100], s[len(s)*99/100]
		if hi-lo < 4 {
			mid := (hi + lo) / 2
			lo, hi = mid-2, mid+2
		}
	}
	scale := 1
	for max(w, h)*scale < minSide {
		scale++
	}
	W, H := w*scale, h*scale
	out := image.NewNRGBA(image.Rect(0, 0, W, H))
	at := func(x, y int) float64 {
		x = min(max(x, 0), w-1)
		y = min(max(y, 0), h-1)
		return c[y*w+x]
	}
	for Y := 0; Y < H; Y++ {
		fy := (float64(Y)+0.5)/float64(scale) - 0.5
		y0 := int(math.Floor(fy))
		ty := fy - float64(y0)
		for X := 0; X < W; X++ {
			fx := (float64(X)+0.5)/float64(scale) - 0.5
			x0 := int(math.Floor(fx))
			tx := fx - float64(x0)
			v := (at(x0, y0)*(1-tx)+at(x0+1, y0)*tx)*(1-ty) + (at(x0, y0+1)*(1-tx)+at(x0+1, y0+1)*tx)*ty
			col := Ironbow((v - lo) / (hi - lo))
			i := out.PixOffset(X, Y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = col.R, col.G, col.B, 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var ironStops = []struct{ x, r, g, b float64 }{{0, 0, 0, 10}, {0.2, 60, 0, 120}, {0.45, 190, 30, 90}, {0.65, 240, 110, 0}, {0.85, 255, 210, 40}, {1, 255, 255, 230}}

// Ironbow is the thermal palette (0 cold .. 1 hot), as in the scenario previews.
func Ironbow(x float64) color.NRGBA {
	x = math.Max(0, math.Min(1, x))
	for i := 1; i < len(ironStops); i++ {
		a, b := ironStops[i-1], ironStops[i]
		if x <= b.x {
			t := (x - a.x) / (b.x - a.x)
			return color.NRGBA{uint8(a.r + (b.r-a.r)*t), uint8(a.g + (b.g-a.g)*t), uint8(a.b + (b.b-a.b)*t), 255}
		}
	}
	return color.NRGBA{255, 255, 230, 255}
}
