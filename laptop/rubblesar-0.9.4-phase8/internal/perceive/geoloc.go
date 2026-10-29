package perceive

import (
	"math"

	"rubblesar/internal/vcam"
)

// PoseErrors are the 1-sigma errors of the reported camera pose.
type PoseErrors struct {
	GPSm      float64 // horizontal, per axis
	AltM      float64 // height above ground
	YawDeg    float64
	AttDeg    float64 // gimbal pitch/roll
	ObjHeight float64 // unknown height of the object above the assumed surface (rubble), metres
	// SurfaceM: the height above ground the parts are assumed to lie at (the
	// debris pile), metres. A part on a 3 m pile seen 20 degrees off nadir sits
	// 1.1 m further out on flat ground, on the other side when seen from there.
	SurfaceM float64
}

// DefaultPoseErrors for a GPS sigma (the flight simulator's model).
func DefaultPoseErrors(gps, yawDeg float64) PoseErrors {
	return PoseErrors{GPSm: gps, AltM: gps * 0.5, YawDeg: yawDeg, AttDeg: 0.5, ObjHeight: 1.0}
}

// Fix is a geolocated image point.
type Fix struct {
	E, N float64
	// Per-axis position variance in square metres, split into the part that
	// changes from frame to frame (gimbal jitter, the unknown height of the
	// object on the rubble seen from a new angle) and the part that drifts
	// slowly and is shared by frames a few seconds apart (GPS, altitude, heading).
	VarIndep, VarCorr float64
	RangeM            float64 // camera to ground point
	OffNadirDeg       float64
	GSDm              float64 // ground size of one pixel across the line of sight
	CosT              float64 // cosine of the off-nadir angle
}

// Var is the total per-axis variance of a single fix.
func (f Fix) Var() float64 { return f.VarIndep + f.VarCorr }

// AreaM2 converts a pixel count around this point to ground area.
func (f Fix) AreaM2(pixels int) float64 {
	return float64(pixels) * f.GSDm * f.GSDm / math.Max(f.CosT, 0.2)
}

// CEP is the circular error probable for a per-axis variance.
func CEP(varAxis float64) float64 { return 1.1774 * math.Sqrt(math.Max(varAxis, 0)) }

// Geolocate intersects the ray through image point (u, v) with a flat surface
// at pe.SurfaceM above the ground and estimates the uncertainty from the pose
// errors.
func Geolocate(cam *vcam.Camera, u, v float64, pe PoseErrors) (Fix, bool) {
	d := cam.Ray(u, v)
	surf := pe.SurfaceM
	if surf >= cam.C.Z-1 {
		surf = 0 // flying lower than the assumed pile: use the ground
	}
	e, n, t, ok := cam.Hit(d, surf)
	if !ok {
		return Fix{}, false
	}
	l := math.Sqrt(d.X*d.X + d.Y*d.Y + d.Z*d.Z)
	rng := t * l
	cosT := -d.Z / l // cosine of the off-nadir angle
	tanT := math.Sqrt(math.Max(0, 1-cosT*cosT)) / math.Max(cosT, 0.05)
	horiz := math.Hypot(e-cam.C.X, n-cam.C.Y)
	alt := cam.C.Z - surf
	yaw := pe.YawDeg * math.Pi / 180
	att := pe.AttDeg * math.Pi / 180
	// One-dimensional error terms (radial or tangential) are spread over two axes.
	// The object's unknown height above the assumed surface shifts every
	// sighting from about the same place the same way: like the GPS error it
	// does not average out over the frames of a hover (it changes as the
	// viewpoint does, which the tracker allows for as it does for GPS drift).
	corr := pe.GPSm*pe.GPSm + (math.Pow(pe.AltM*tanT, 2)+math.Pow(horiz*yaw, 2)+math.Pow(pe.ObjHeight*tanT, 2))/2
	indep := math.Pow(alt*att/(cosT*cosT), 2)/2 + 0.01
	f := Fix{E: e, N: n, VarIndep: indep, VarCorr: corr, RangeM: rng, OffNadirDeg: math.Acos(math.Min(1, cosT)) * 180 / math.Pi}
	f.GSDm = rng / cam.F
	f.CosT = cosT
	return f, true
}
