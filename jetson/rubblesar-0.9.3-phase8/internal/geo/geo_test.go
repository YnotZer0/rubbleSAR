package geo

import (
	"math"
	"testing"
)

var antakya = Point{36.20250, 36.16056}

func TestLocalRoundTrip(t *testing.T) {
	l := NewLocal(antakya)
	for _, v := range [][2]float64{{0, 0}, {805, 0}, {0, -805}, {-569, 569}, {123.4, -456.7}} {
		p := l.ToPoint(v[0], v[1])
		e, n := l.ToLocal(p)
		if math.Abs(e-v[0]) > 1e-6 || math.Abs(n-v[1]) > 1e-6 {
			t.Errorf("round trip %v -> %v -> (%v,%v)", v, p, e, n)
		}
	}
}

// The tangent plane must agree with haversine to within a few centimetres
// across the operating radius (haversine uses a sphere, so allow 0.5%).
func TestLocalAgreesWithHaversine(t *testing.T) {
	l := NewLocal(antakya)
	for _, v := range [][2]float64{{805, 0}, {0, 805}, {569, 569}, {-300, 700}} {
		p := l.ToPoint(v[0], v[1])
		want := math.Hypot(v[0], v[1])
		got := Distance(antakya, p)
		if math.Abs(got-want)/want > 0.005 {
			t.Errorf("offset %v: haversine %.2f m, plane %.2f m", v, got, want)
		}
	}
}

func TestKnownDistance(t *testing.T) {
	// One degree of latitude is about 111 km.
	d := Distance(Point{36, 36}, Point{37, 36})
	if d < 110_000 || d > 112_000 {
		t.Fatalf("1 degree latitude = %.0f m", d)
	}
}

func TestBearing(t *testing.T) {
	l := NewLocal(antakya)
	cases := []struct {
		e, n, want float64
	}{{0, 100, 0}, {100, 0, 90}, {0, -100, 180}, {-100, 0, 270}, {100, 100, 45}}
	for _, c := range cases {
		got := Bearing(antakya, l.ToPoint(c.e, c.n))
		diff := math.Abs(got - c.want)
		if diff > 180 {
			diff = 360 - diff
		}
		// Bearing uses a sphere, the plane uses the ellipsoid: allow 0.25 degrees.
		if diff > 0.25 {
			t.Errorf("bearing to (%v,%v) = %.3f want %.1f", c.e, c.n, got, c.want)
		}
		if h := HeadingFromVector(c.e, c.n); math.Abs(h-c.want) > 1e-9 && !(c.want == 0 && h == 0) {
			t.Errorf("HeadingFromVector(%v,%v)=%v want %v", c.e, c.n, h, c.want)
		}
	}
}

func TestNormDeg(t *testing.T) {
	for in, want := range map[float64]float64{0: 0, 360: 0, -90: 270, 450: 90, -720: 0} {
		if got := NormDeg(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("NormDeg(%v)=%v want %v", in, got, want)
		}
	}
}
