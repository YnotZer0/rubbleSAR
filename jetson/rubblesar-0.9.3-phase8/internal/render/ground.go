package render

import (
	"math"

	"rubblesar/internal/raster"
)

// GroundAbsorb is the solar absorptivity of open ground between sites.
const GroundAbsorb = 0.75

// GroundRGB is the colour of open ground at local metres (e, n). The site
// layers use exactly this function outside the rubble, so the camera can
// switch between layers and this function without visible seams.
//
// detail 0 = full detail (layer resolution), 1 = no texture finer than about
// 3 cm, 2 = no texture finer than about 10 cm.
func GroundRGB(seed uint64, e, n float64, detail int) [3]float64 {
	m := materials[matGround]
	f := 1 + 0.10*raster.FBM(seed+1, e/14, n/14, 3) + 0.05*raster.FBM(seed+2, e/1.7, n/1.7, 2)
	if detail == 0 {
		f += m.grain * raster.Value(seed+3, e/0.03, n/0.03)
	}
	c := [3]float64{m.rgb[0] * f, m.rgb[1] * f, m.rgb[2] * f}
	// Mild warm/cool tint drift (dust, soil, asphalt patches).
	t := 0.06 * raster.FBM(seed+4, e/22, n/22, 2)
	c[0] *= 1 + t
	c[2] *= 1 - t
	if detail >= 2 {
		return c
	}
	// Pebbles and small litter: one candidate per 0.4 m cell.
	const cell = 0.4
	cx, cy := math.Floor(e/cell), math.Floor(n/cell)
	h := raster.Hash(seed+5, int64(cx), int64(cy))
	if raster.Unit(h) < 0.14 {
		h2 := raster.Hash(seed+6, int64(cx), int64(cy))
		px := (cx + 0.2 + 0.6*raster.Unit(h2)) * cell
		py := (cy + 0.2 + 0.6*raster.Unit(h2>>17)) * cell
		r := 0.015 + 0.04*raster.Unit(h2>>34)
		d := math.Hypot(e-px, n-py)
		cov := 0.0
		if detail == 0 {
			if d < r {
				cov = 1
			}
		} else {
			cov = clamp01((r + 0.015 - d) / 0.03)
		}
		if cov > 0 {
			k := 0.7
			if raster.Unit(h>>40) < 0.5 {
				k = 1.3
			}
			k = 1 + (k-1)*cov
			c = [3]float64{c[0] * k, c[1] * k, c[2] * k}
		}
	}
	return c
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

func smoothstep(t float64) float64 {
	t = clamp01(t)
	return t * t * (3 - 2*t)
}
