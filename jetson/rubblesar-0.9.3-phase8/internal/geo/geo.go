// Package geo provides the small amount of WGS84 geometry RubbleSAR needs:
// a local east/north tangent plane around the incident centre, distances and
// bearings. Within the 0.5 mile operating radius the tangent-plane error is
// below one centimetre.
package geo

import "math"

const (
	wgs84A  = 6378137.0             // semi-major axis, m
	wgs84F  = 1 / 298.257223563     // flattening
	wgs84E2 = wgs84F * (2 - wgs84F) // first eccentricity squared
	// EarthMeanRadius is used for haversine distances.
	EarthMeanRadius = 6371008.8
)

// Point is a WGS84 latitude/longitude in degrees.
type Point struct{ Lat, Lon float64 }

// Local is a tangent plane anchored at Origin. East and North are metres.
type Local struct {
	Origin     Point
	mPerDegLat float64
	mPerDegLon float64
}

// NewLocal builds a tangent plane at origin using the WGS84 radii of curvature.
func NewLocal(origin Point) Local {
	phi := origin.Lat * math.Pi / 180
	s := math.Sin(phi)
	w := math.Sqrt(1 - wgs84E2*s*s)
	m := wgs84A * (1 - wgs84E2) / (w * w * w) // meridional radius
	n := wgs84A / w                           // prime-vertical radius
	return Local{
		Origin:     origin,
		mPerDegLat: m * math.Pi / 180,
		mPerDegLon: n * math.Cos(phi) * math.Pi / 180,
	}
}

// ToPoint converts local east/north metres to latitude/longitude.
func (l Local) ToPoint(east, north float64) Point {
	return Point{
		Lat: l.Origin.Lat + north/l.mPerDegLat,
		Lon: l.Origin.Lon + east/l.mPerDegLon,
	}
}

// ToLocal converts latitude/longitude to local east/north metres.
func (l Local) ToLocal(p Point) (east, north float64) {
	return (p.Lon - l.Origin.Lon) * l.mPerDegLon, (p.Lat - l.Origin.Lat) * l.mPerDegLat
}

// Distance returns the great-circle distance in metres (haversine).
func Distance(a, b Point) float64 {
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dp := p2 - p1
	dl := (b.Lon - a.Lon) * math.Pi / 180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * EarthMeanRadius * math.Asin(math.Min(1, math.Sqrt(h)))
}

// Bearing returns the initial bearing from a to b, degrees clockwise from north, 0..360.
func Bearing(a, b Point) float64 {
	p1, p2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dl := (b.Lon - a.Lon) * math.Pi / 180
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	return NormDeg(math.Atan2(y, x) * 180 / math.Pi)
}

// NormDeg wraps an angle into [0, 360).
func NormDeg(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// HeadingFromVector converts an east/north vector to a compass heading.
func HeadingFromVector(east, north float64) float64 {
	return NormDeg(math.Atan2(east, north) * 180 / math.Pi)
}
