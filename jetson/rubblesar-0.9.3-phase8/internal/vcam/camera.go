// Package vcam is the virtual gimbal camera: it turns a drone pose and a
// scenario time into an RGB frame, a radiometric thermal frame and the
// matching ground-truth label frames, by sampling the rendered site layers.
//
// Coordinates: local metres east (E), north (N) and up (Z) from the incident
// centre at ground level. Image pixel (u, v) has its centre at (u+0.5, v+0.5);
// u grows to the right, v grows downwards.
package vcam

import (
	"math"

	"rubblesar/internal/diag"
)

// Intrinsics describes a pinhole camera with square pixels.
type Intrinsics struct {
	W       int     `json:"width" toml:"width"`
	H       int     `json:"height" toml:"height"`
	HFOVDeg float64 `json:"hfov_deg" toml:"hfov_deg"`
}

// DefaultRGB is the simulated visible camera (1080p, 70 degree horizontal field).
var DefaultRGB = Intrinsics{W: 1920, H: 1080, HFOVDeg: 70}

// DefaultThermal is the simulated radiometric thermal camera (640x512, 50 degrees).
var DefaultThermal = Intrinsics{W: 640, H: 512, HFOVDeg: 50}

// Validate checks the intrinsics.
func (in Intrinsics) Validate() error {
	if in.W < 16 || in.H < 16 || in.W > 8192 || in.H > 8192 || in.HFOVDeg < 1 || in.HFOVDeg > 170 {
		return diag.New("E-VCAM-002", "camera %dx%d hfov %.1f", in.W, in.H, in.HFOVDeg)
	}
	return nil
}

// F returns the focal length in pixels.
func (in Intrinsics) F() float64 { return float64(in.W) / 2 / math.Tan(in.HFOVDeg*math.Pi/360) }

// VFOVDeg returns the vertical field of view.
func (in Intrinsics) VFOVDeg() float64 {
	return 2 * math.Atan(float64(in.H)/2/in.F()) * 180 / math.Pi
}

// Pose is the camera position and attitude.
type Pose struct {
	E        float64 `json:"e_m"`
	N        float64 `json:"n_m"`
	AltAGL   float64 `json:"alt_agl_m"`
	YawDeg   float64 `json:"yaw_deg"`   // direction the top of the image faces, clockwise from north
	PitchDeg float64 `json:"pitch_deg"` // -90 = straight down
	RollDeg  float64 `json:"roll_deg"`
}

// Validate checks the pose.
func (p Pose) Validate() error {
	if !(p.AltAGL >= 2 && p.AltAGL <= 400) || !(p.PitchDeg >= -90 && p.PitchDeg <= -20) || math.Abs(p.RollDeg) > 30 ||
		math.IsNaN(p.E) || math.IsNaN(p.N) || math.IsNaN(p.YawDeg) {
		return diag.New("E-VCAM-001", "pose alt %.1f m, pitch %.1f, roll %.1f", p.AltAGL, p.PitchDeg, p.RollDeg)
	}
	return nil
}

// V3 is a 3-vector.
type V3 struct{ X, Y, Z float64 }

func (a V3) add(b V3) V3        { return V3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a V3) sub(b V3) V3        { return V3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a V3) scale(s float64) V3 { return V3{a.X * s, a.Y * s, a.Z * s} }
func (a V3) dot(b V3) float64   { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a V3) cross(b V3) V3 {
	return V3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}
func (a V3) norm() V3 { return a.scale(1 / math.Sqrt(a.dot(a))) }

// Camera is a posed pinhole camera.
type Camera struct {
	In               Intrinsics
	Pose             Pose
	F, Cx, Cy        float64
	C                V3 // centre of projection
	Fwd, Right, Down V3 // optical axis, image +u, image +v
}

// New builds a camera.
func New(in Intrinsics, p Pose) (*Camera, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	y, pt := p.YawDeg*math.Pi/180, p.PitchDeg*math.Pi/180
	fwd := V3{math.Sin(y) * math.Cos(pt), math.Cos(y) * math.Cos(pt), math.Sin(pt)}
	right := V3{math.Cos(y), -math.Sin(y), 0}
	down := fwd.cross(right)
	if p.RollDeg != 0 { // rotate right/down about the optical axis
		r := p.RollDeg * math.Pi / 180
		c, s := math.Cos(r), math.Sin(r)
		right, down = right.scale(c).add(down.scale(s)), down.scale(c).sub(right.scale(s))
	}
	return &Camera{In: in, Pose: p, F: in.F(), Cx: float64(in.W) / 2, Cy: float64(in.H) / 2,
		C: V3{p.E, p.N, p.AltAGL}, Fwd: fwd, Right: right, Down: down}, nil
}

// Ray returns the (unnormalised) viewing direction through image point (u, v).
func (c *Camera) Ray(u, v float64) V3 {
	return c.Fwd.add(c.Right.scale((u - c.Cx) / c.F)).add(c.Down.scale((v - c.Cy) / c.F))
}

// Hit intersects a ray with the horizontal plane z = h.
func (c *Camera) Hit(d V3, h float64) (e, n, t float64, ok bool) {
	if d.Z >= -1e-9 {
		return 0, 0, 0, false
	}
	t = (h - c.C.Z) / d.Z
	if t <= 0 {
		return 0, 0, 0, false
	}
	return c.C.X + d.X*t, c.C.Y + d.Y*t, t, true
}

// GroundAt returns where image point (u, v) meets the plane z = h.
func (c *Camera) GroundAt(u, v, h float64) (e, n float64, ok bool) {
	e, n, _, ok = c.Hit(c.Ray(u, v), h)
	return
}

// Project returns the image point of a world point, ok=false when it is behind the camera.
func (c *Camera) Project(e, n, h float64) (u, v float64, ok bool) {
	w := V3{e, n, h}.sub(c.C)
	z := w.dot(c.Fwd)
	if z <= 1e-6 {
		return 0, 0, false
	}
	return c.Cx + c.F*w.dot(c.Right)/z, c.Cy + c.F*w.dot(c.Down)/z, true
}

// InImage reports whether an image point is inside the frame.
func (c *Camera) InImage(u, v float64) bool {
	return u >= 0 && v >= 0 && u < float64(c.In.W) && v < float64(c.In.H)
}

// Footprint returns the ground (z = 0) points of the four image corners
// (top-left, top-right, bottom-right, bottom-left). Corners above the horizon
// are clamped to 2 km.
func (c *Camera) Footprint() [4][2]float64 {
	var out [4][2]float64
	for i, uv := range [][2]float64{{0, 0}, {float64(c.In.W), 0}, {float64(c.In.W), float64(c.In.H)}, {0, float64(c.In.H)}} {
		d := c.Ray(uv[0], uv[1])
		if e, n, t, ok := c.Hit(d, 0); ok && t*math.Sqrt(d.dot(d)) < 2000 {
			out[i] = [2]float64{e, n}
			continue
		}
		h := math.Hypot(d.X, d.Y)
		out[i] = [2]float64{c.C.X + d.X/h*2000, c.C.Y + d.Y/h*2000}
	}
	return out
}

// GSDCenter is the ground sample distance at the image centre (metres per pixel).
func (c *Camera) GSDCenter() float64 {
	d := c.Ray(c.Cx, c.Cy)
	_, _, t, ok := c.Hit(d, 0)
	if !ok {
		return math.Inf(1)
	}
	dist := t * math.Sqrt(d.dot(d))
	return dist / c.F / math.Max(0.2, -d.norm().Z)
}
