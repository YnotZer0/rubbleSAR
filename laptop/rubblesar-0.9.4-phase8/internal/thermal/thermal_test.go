package thermal

import (
	"math"
	"testing"
	"time"
)

var env = Environment{AirMinC: 3, AirMaxC: 14, SunPeakGainC: 12}

func at(h, m int) time.Time {
	return time.Date(2026, 2, 10, h, m, 0, 0, time.FixedZone("TRT", 3*3600))
}

func TestAirDiurnal(t *testing.T) {
	if v := env.AirC(at(6, 0)); math.Abs(v-3) > 1e-9 {
		t.Errorf("06:00 = %v want 3", v)
	}
	if v := env.AirC(at(15, 0)); math.Abs(v-14) > 1e-9 {
		t.Errorf("15:00 = %v want 14", v)
	}
	// Monotonic warming in the morning, cooling in the evening, continuous at midnight.
	prev := env.AirC(at(6, 0))
	for h := 7; h <= 15; h++ {
		v := env.AirC(at(h, 0))
		if v < prev {
			t.Errorf("not warming at %02d:00", h)
		}
		prev = v
	}
	if a, b := env.AirC(at(23, 59)), env.AirC(at(0, 0)); math.Abs(a-b) > 0.05 {
		t.Errorf("discontinuity at midnight: %v vs %v", a, b)
	}
}

func TestSunPosition(t *testing.T) {
	lat, lon := 36.2025, 36.16056
	// Antakya, 10 Feb: solar noon is about 12:49 local (UTC+3, longitude 36.2 E,
	// equation of time -14 min); the sun is due south, 90-36.2-14.6 = ~39 deg up.
	e, az := SunPosition(lat, lon, at(12, 49))
	if e < 37.5 || e > 40.5 || az < 177 || az > 183 {
		t.Errorf("noon sun elev %.1f az %.1f", e, az)
	}
	// Before sunrise.
	if e, _ := SunPosition(lat, lon, at(5, 0)); e > -5 {
		t.Errorf("05:00 elevation %.1f should be below horizon", e)
	}
	// Morning sun in the south-east.
	if _, az := SunPosition(lat, lon, at(9, 0)); az < 110 || az > 160 {
		t.Errorf("09:00 azimuth %.1f", az)
	}
	if g := env.SunGainC(90); math.Abs(g-12) > 1e-9 || env.SunGainC(-3) != 0 {
		t.Errorf("sun gain")
	}
}

func TestVictimModels(t *testing.T) {
	alive := VictimModel{Status: AliveResponsive, Alpha0: 0.7}
	if v := alive.SkinC(5, 10); math.Abs(v-(10+0.7*27)) > 1e-9 {
		t.Errorf("alive skin %v", v)
	}
	det := VictimModel{Status: Deteriorating, Alpha0: 0.7, BetaPerH: 0.05}
	if det.SkinC(4, 10) >= det.SkinC(0, 10) {
		t.Error("deteriorating victim must cool over time")
	}
	if v := det.SkinC(100, 10); math.Abs(v-(10+0.15*27)) > 1e-9 {
		t.Errorf("alpha floor not applied: %v", v)
	}
	dead := VictimModel{Status: Deceased, DeathHours: -2, SkinAtDeath: 30, KPerH: 0.25}
	v0 := dead.SkinC(0, 10)
	want := 10 + 20*math.Exp(-0.25*2)
	if math.Abs(v0-want) > 1e-9 {
		t.Errorf("deceased skin %v want %v", v0, want)
	}
	if dead.SkinC(48, 10)-10 > 0.01 {
		t.Error("deceased victim must reach ambient")
	}
}

func TestCentiK(t *testing.T) {
	for _, c := range []float64{-40, 0, 21.37, 37, 150} {
		if got := CentiKToC(CToCentiK(c)); math.Abs(got-c) > 0.006 {
			t.Errorf("%v -> %v", c, got)
		}
	}
	if CToCentiK(-300) != 0 || CToCentiK(1000) != 65535 {
		t.Error("clamping")
	}
}
