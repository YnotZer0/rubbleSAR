package vcam

import (
	"encoding/json"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"

	"rubblesar/internal/diag"
	"rubblesar/internal/raster"
	"rubblesar/internal/thermal"
)

// ClassColours are the overlay colours per part class (index = class id).
var ClassColours = [][3]uint8{{0, 0, 0}, {255, 64, 64}, {255, 170, 0}, {64, 160, 255}, {0, 230, 120}, {200, 80, 255}}

// Save writes the frame files into dir:
//
//	rgb.png            colour frame
//	thermal.png        16-bit radiometric frame, centikelvin (value/100 - 273.15 = deg C)
//	thermal-view.png   false-colour thermal for people
//	labels-parts.png   class per RGB pixel (0 background, 1 hand, 2 arm, 3 leg, 4 head, 5 torso)
//	labels-victim.png  victim index per RGB pixel (instance id)
//	labels-thermal-parts.png, labels-thermal-victim.png   same at thermal resolution
//	overlay.png        colour frame with the labels painted on (for checking by eye)
//	frame.json         metadata and ground truth
func (f *Frame) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return diag.Wrap("E-RND-001", err, "creating %s", dir)
	}
	save := func(name string, img image.Image) error {
		if err := raster.SavePNG(filepath.Join(dir, name), img); err != nil {
			return diag.Wrap("E-RND-001", err, "writing %s", name)
		}
		return nil
	}
	if f.RGB != nil {
		if err := save("rgb.png", f.RGB.Image()); err != nil {
			return err
		}
		if err := save("labels-parts.png", f.PartsRGB.Image()); err != nil {
			return err
		}
		if err := save("labels-victim.png", f.InstRGB.Image()); err != nil {
			return err
		}
		if err := save("overlay.png", Overlay(f.RGB, f.PartsRGB)); err != nil {
			return err
		}
	}
	if f.Thermal != nil {
		if err := save("thermal.png", f.Thermal.Image()); err != nil {
			return err
		}
		if err := save("thermal-view.png", ThermalView(f.Thermal, math.NaN(), math.NaN())); err != nil {
			return err
		}
		if err := save("labels-thermal-parts.png", f.PartsTh.Image()); err != nil {
			return err
		}
		if err := save("labels-thermal-victim.png", f.InstTh.Image()); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(f.Meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "frame.json"), append(b, '\n'), 0o644); err != nil {
		return diag.Wrap("E-RND-001", err, "writing frame.json")
	}
	return nil
}

// Overlay paints label colours over an RGB frame (60% opacity) with outlines.
func Overlay(rgb *raster.RGB, parts *raster.U8) *image.NRGBA {
	m := rgb.ToNRGBA()
	W, H := rgb.W, rgb.H
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			p := parts.Pix[y*W+x]
			if p == 0 || int(p) >= len(ClassColours) {
				continue
			}
			c := ClassColours[p]
			edge := x == 0 || y == 0 || x == W-1 || y == H-1 || parts.Pix[y*W+x-1] != p || parts.Pix[y*W+x+1] != p ||
				parts.Pix[(y-1)*W+x] != p || parts.Pix[(y+1)*W+x] != p
			a := 0.45
			if edge {
				a = 1
			}
			i := m.PixOffset(x, y)
			for ch := 0; ch < 3; ch++ {
				m.Pix[i+ch] = uint8(float64(m.Pix[i+ch])*(1-a) + float64(c[ch])*a)
			}
		}
	}
	return m
}

// ThermalView renders a thermal frame with an "ironbow"-like palette between
// lo and hi degrees C (NaN = automatic 1st..99.5th percentile).
func ThermalView(t *raster.U16, lo, hi float64) *image.NRGBA {
	if math.IsNaN(lo) || math.IsNaN(hi) {
		hist := make([]int, 65536)
		for _, v := range t.Pix {
			hist[v]++
		}
		n := len(t.Pix)
		acc, l, h := 0, -1, -1
		for v, c := range hist {
			acc += c
			if l < 0 && acc >= n/100 {
				l = v
			}
			if h < 0 && acc >= n*995/1000 {
				h = v
			}
		}
		lo, hi = thermal.CentiKToC(uint16(l)), thermal.CentiKToC(uint16(h))
		if hi-lo < 4 {
			hi = lo + 4
		}
	}
	m := image.NewNRGBA(image.Rect(0, 0, t.W, t.H))
	for i, v := range t.Pix {
		x := (thermal.CentiKToC(v) - lo) / (hi - lo)
		c := ironbow(x)
		m.Pix[4*i], m.Pix[4*i+1], m.Pix[4*i+2], m.Pix[4*i+3] = c.R, c.G, c.B, 255
	}
	return m
}

var ironStops = []struct {
	x       float64
	r, g, b float64
}{{0, 0, 0, 10}, {0.2, 60, 0, 120}, {0.45, 190, 30, 90}, {0.65, 240, 110, 0}, {0.85, 255, 210, 40}, {1, 255, 255, 230}}

func ironbow(x float64) color.NRGBA {
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
