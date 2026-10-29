// Package perceive is the onboard perception pipeline of edge-eye: thermal
// cue, geolocation, tracking and detection packets. It works on what a real
// drone has - the colour frame, the radiometric thermal frame and the pose its
// GPS and IMU report - and never on simulation truth.
package perceive

import (
	"fmt"
	"math"
	"sort"

	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/vcam"
)

// ThermalMap converts between colour and thermal pixel coordinates of two
// cameras sharing one optical centre and orientation (co-boresighted), which
// differ only in resolution and field of view.
type ThermalMap struct {
	rgbCx, rgbCy, rgbF float64
	thCx, thCy, thF    float64
	thW, thH           int
}

// NewThermalMap builds the mapping for two cameras.
func NewThermalMap(rgb, th vcam.Intrinsics) ThermalMap {
	return ThermalMap{rgbCx: float64(rgb.W) / 2, rgbCy: float64(rgb.H) / 2, rgbF: rgb.F(),
		thCx: float64(th.W) / 2, thCy: float64(th.H) / 2, thF: th.F(), thW: th.W, thH: th.H}
}

// ToThermal maps continuous colour image coordinates to thermal coordinates.
func (m ThermalMap) ToThermal(u, v float64) (float64, float64) {
	s := m.thF / m.rgbF
	return m.thCx + (u-m.rgbCx)*s, m.thCy + (v-m.rgbCy)*s
}

// ToRGB maps continuous thermal image coordinates to colour coordinates.
func (m ThermalMap) ToRGB(u, v float64) (float64, float64) {
	s := m.rgbF / m.thF
	return m.rgbCx + (u-m.thCx)*s, m.rgbCy + (v-m.thCy)*s
}

// CK converts a radiometric value (centikelvin) to degrees C.
func CK(v uint16) float64 { return float64(v)/100 - 273.15 }

// Reading is the temperature of one body part.
type Reading struct {
	Readable  bool
	Reason    string // why not readable
	SkinC     float64
	AmbientC  float64
	DeltaC    float64
	Pixels    int
	Warm      bool
	BoxTh     [4]int // thermal pixels covering the part (x0, y0, x1, y1), when in view
	CentreThU float64
	CentreThV float64
}

// ThermalParams are the thermal cue settings.
type ThermalParams struct {
	SkinMinC, SkinMaxC float64
	WarmDeltaC         float64
	MinPixels          int
	WarmSpotMinPx      int
}

// Read measures a part given its colour mask. A thermal pixel belongs to the
// part when at least half of its four sub-samples fall on the mask. Skin is the
// 75th percentile of those pixels, ambient the median of a ring 2..5 pixels
// outside the part's box.
func (m ThermalMap) Read(th *raster.U16, mask *masks.Bitmap, p ThermalParams) Reading {
	var r Reading
	if th == nil {
		r.Reason = "no thermal camera"
		return r
	}
	u0, v0 := m.ToThermal(float64(mask.X0), float64(mask.Y0))
	u1, v1 := m.ToThermal(float64(mask.X0+mask.W), float64(mask.Y0+mask.H))
	x0, y0 := int(math.Floor(u0)), int(math.Floor(v0))
	x1, y1 := int(math.Ceil(u1)), int(math.Ceil(v1))
	if x1 <= 0 || y1 <= 0 || x0 >= th.W || y0 >= th.H {
		r.Reason = "outside thermal view"
		return r
	}
	x0, y0, x1, y1 = max(x0, 0), max(y0, 0), min(x1, th.W), min(y1, th.H)
	r.BoxTh = [4]int{x0, y0, x1, y1}
	var vals []float64
	var su, sv float64
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			in := 0
			for _, o := range [][2]float64{{0.25, 0.25}, {0.75, 0.25}, {0.25, 0.75}, {0.75, 0.75}} {
				u, v := m.ToRGB(float64(x)+o[0], float64(y)+o[1])
				if mask.At(int(math.Floor(u)), int(math.Floor(v))) {
					in++
				}
			}
			if in >= 2 {
				vals = append(vals, CK(th.Pix[y*th.W+x]))
				su += float64(x) + 0.5
				sv += float64(y) + 0.5
			}
		}
	}
	r.Pixels = len(vals)
	if r.Pixels > 0 {
		r.CentreThU, r.CentreThV = su/float64(r.Pixels), sv/float64(r.Pixels)
	}
	if r.Pixels < p.MinPixels {
		r.Reason = fmt.Sprintf("too small (%d px)", r.Pixels)
		return r
	}
	ring := ringValues(th, x0, y0, x1, y1, 2, 5)
	if len(ring) < 8 {
		r.Reason = "at the edge of the thermal view"
		return r
	}
	r.Readable = true
	r.SkinC = percentile(vals, 0.75)
	r.AmbientC = percentile(ring, 0.5)
	r.DeltaC = r.SkinC - r.AmbientC
	r.Warm = r.SkinC >= p.SkinMinC && r.SkinC <= p.SkinMaxC && r.DeltaC >= p.WarmDeltaC
	return r
}

// ringValues returns the temperatures of the pixels between gap and gap+width
// pixels outside the box (x0, y0)-(x1, y1).
func ringValues(th *raster.U16, x0, y0, x1, y1, gap, width int) []float64 {
	var out []float64
	ox0, oy0, ox1, oy1 := x0-gap-width, y0-gap-width, x1+gap+width, y1+gap+width
	for y := max(oy0, 0); y < min(oy1, th.H); y++ {
		for x := max(ox0, 0); x < min(ox1, th.W); x++ {
			if x >= x0-gap && x < x1+gap && y >= y0-gap && y < y1+gap {
				continue
			}
			out = append(out, CK(th.Pix[y*th.W+x]))
		}
	}
	return out
}

func percentile(v []float64, p float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// WarmSpot is a skin-temperature blob in the thermal frame.
type WarmSpot struct {
	Reading
	// Centre in colour image coordinates, and the blob as a colour-frame box.
	U, V    float64
	BoxRGB  [4]float64
	MaskRGB masks.Bitmap // the blob's pixels in colour-frame coordinates (coarse)
	Area    int          // thermal pixels
	MaxC    float64
}

// WarmSpots finds blobs of thermal pixels in the skin band that are clearly
// warmer than their surroundings and are not explained by a body part already
// detected in colour (covered marks thermal pixels to ignore). Blobs next to
// something much hotter (a smouldering spot, a sun-heated sheet) are the
// cooling edge of that object, not a person, and are rejected.
func (m ThermalMap) WarmSpots(th *raster.U16, covered []bool, p ThermalParams) []WarmSpot {
	if th == nil {
		return nil
	}
	lo, hi := p.SkinMinC, p.SkinMaxC
	key := func(i int) uint32 {
		if covered != nil && covered[i] {
			return 0
		}
		c := CK(th.Pix[i])
		if c >= lo && c <= hi {
			return 1
		}
		return 0
	}
	var out []WarmSpot
	for _, c := range masks.Components(th.W, th.H, key) {
		if c.Area() < p.WarmSpotMinPx || c.Area() > 4000 {
			continue
		}
		vals := make([]float64, 0, c.Area())
		mx := -1e9
		for _, i := range c.Pixels {
			v := CK(th.Pix[i])
			vals = append(vals, v)
			mx = math.Max(mx, v)
		}
		ring := ringValues(th, c.X0, c.Y0, c.X1, c.Y1, 1, 4)
		if len(ring) < 8 {
			continue
		}
		ringMax := -1e9
		for _, v := range ring {
			ringMax = math.Max(ringMax, v)
		}
		if ringMax > hi+3 {
			continue // edge of a hotter object
		}
		skin, amb := percentile(vals, 0.75), percentile(ring, 0.5)
		if skin-amb < p.WarmDeltaC {
			continue
		}
		ws := WarmSpot{Reading: Reading{Readable: true, SkinC: skin, AmbientC: amb, DeltaC: skin - amb, Pixels: c.Area(), Warm: true,
			BoxTh: [4]int{c.X0, c.Y0, c.X1, c.Y1}, CentreThU: c.CX, CentreThV: c.CY}, Area: c.Area(), MaxC: mx}
		ws.U, ws.V = m.ToRGB(c.CX, c.CY)
		bx0, by0 := m.ToRGB(float64(c.X0), float64(c.Y0))
		bx1, by1 := m.ToRGB(float64(c.X1), float64(c.Y1))
		ws.BoxRGB = [4]float64{bx0, by0, bx1, by1}
		ws.MaskRGB = m.blobMask(&c, th.W)
		out = append(out, ws)
	}
	return out
}

// blobMask scales a thermal component up to colour-frame pixels.
func (m ThermalMap) blobMask(c *masks.Component, thW int) masks.Bitmap {
	u0, v0 := m.ToRGB(float64(c.X0), float64(c.Y0))
	u1, v1 := m.ToRGB(float64(c.X1), float64(c.Y1))
	b := masks.Bitmap{X0: int(math.Floor(u0)), Y0: int(math.Floor(v0))}
	b.W, b.H = int(math.Ceil(u1))-b.X0, int(math.Ceil(v1))-b.Y0
	if b.W <= 0 || b.H <= 0 {
		return masks.Bitmap{}
	}
	b.Pix = make([]bool, b.W*b.H)
	in := map[int32]bool{}
	for _, p := range c.Pixels {
		in[p] = true
	}
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			tu, tv := m.ToThermal(float64(b.X0+x)+0.5, float64(b.Y0+y)+0.5)
			tx, ty := int(math.Floor(tu)), int(math.Floor(tv))
			if tx >= 0 && ty >= 0 && in[int32(ty*thW+tx)] {
				b.Pix[y*b.W+x] = true
			}
		}
	}
	return b
}

// Covered marks the thermal pixels lying under colour masks (grown by one
// pixel), so warm spots are only looked for where colour saw nothing.
func (m ThermalMap) Covered(thW, thH int, ms []*masks.Bitmap) []bool {
	out := make([]bool, thW*thH)
	for _, mk := range ms {
		u0, v0 := m.ToThermal(float64(mk.X0), float64(mk.Y0))
		u1, v1 := m.ToThermal(float64(mk.X0+mk.W), float64(mk.Y0+mk.H))
		for y := max(int(v0)-1, 0); y < min(int(math.Ceil(v1))+1, thH); y++ {
			for x := max(int(u0)-1, 0); x < min(int(math.Ceil(u1))+1, thW); x++ {
				out[y*thW+x] = true
			}
		}
	}
	return out
}
