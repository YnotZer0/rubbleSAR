// Package thermal holds the simulation's temperature and sun models.
//
// All models are illustrative and deliberately simple; they exist so that the
// imagery behaves plausibly (living skin warmer than rubble, deceased victims
// cooling towards ambient, sun-warmed debris confusing the picture). Every
// constant is a parameter in the scenario and must be reviewed by a medical
// lead before any operational interpretation (plan decision D9).
package thermal

import (
	"math"
	"time"
)

// CoreC is normal human core temperature.
const CoreC = 37.0

// Environment describes the weather at the incident.
type Environment struct {
	AirMinC      float64 `json:"air_min_c" toml:"air_min_c"`             // overnight minimum, reached at 06:00 local
	AirMaxC      float64 `json:"air_max_c" toml:"air_max_c"`             // afternoon maximum, reached at 15:00 local
	SunPeakGainC float64 `json:"sun_peak_gain_c" toml:"sun_peak_gain_c"` // extra heating of a fully absorbing surface with the sun overhead
}

// AirC returns the air temperature at local clock time t using a smooth
// diurnal curve: minimum at 06:00, maximum at 15:00.
func (e Environment) AirC(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60 + float64(t.Second())/3600
	mean := (e.AirMinC + e.AirMaxC) / 2
	amp := (e.AirMaxC - e.AirMinC) / 2
	var phase float64
	switch {
	case h >= 6 && h < 15: // warming: 9 hours from min to max
		phase = math.Pi * (h - 6) / 9
		return mean - amp*math.Cos(phase)
	default: // cooling: 15 hours from max to min
		dh := h - 15
		if dh < 0 {
			dh += 24
		}
		phase = math.Pi * dh / 15
		return mean + amp*math.Cos(phase)
	}
}

// SunGainC is the extra surface heating (°C) of a fully absorbing surface for
// a given sun elevation.
func (e Environment) SunGainC(elevDeg float64) float64 {
	if elevDeg <= 0 {
		return 0
	}
	return e.SunPeakGainC * math.Sin(elevDeg*math.Pi/180)
}

// SunPosition returns solar elevation and azimuth (degrees, azimuth clockwise
// from north) using the NOAA approximation (accurate to a fraction of a degree).
func SunPosition(latDeg, lonDeg float64, t time.Time) (elevDeg, azDeg float64) {
	u := t.UTC()
	day := float64(u.YearDay())
	hour := float64(u.Hour()) + float64(u.Minute())/60 + float64(u.Second())/3600
	g := 2 * math.Pi / 365 * (day - 1 + (hour-12)/24) // fractional year, radians
	eqt := 229.18 * (0.000075 + 0.001868*math.Cos(g) - 0.032077*math.Sin(g) - 0.014615*math.Cos(2*g) - 0.040849*math.Sin(2*g))
	decl := 0.006918 - 0.399912*math.Cos(g) + 0.070257*math.Sin(g) - 0.006758*math.Cos(2*g) +
		0.000907*math.Sin(2*g) - 0.002697*math.Cos(3*g) + 0.00148*math.Sin(3*g)
	tst := hour*60 + eqt + 4*lonDeg // true solar time, minutes (UTC based)
	ha := (tst/4 - 180) * math.Pi / 180
	lat := latDeg * math.Pi / 180
	cosZen := math.Sin(lat)*math.Sin(decl) + math.Cos(lat)*math.Cos(decl)*math.Cos(ha)
	cosZen = math.Max(-1, math.Min(1, cosZen))
	zen := math.Acos(cosZen)
	elevDeg = 90 - zen*180/math.Pi
	// Azimuth clockwise from north.
	az := math.Atan2(math.Sin(ha), math.Cos(ha)*math.Sin(lat)-math.Tan(decl)*math.Cos(lat))
	azDeg = math.Mod(az*180/math.Pi+180, 360)
	return elevDeg, azDeg
}

// Victim status values.
const (
	AliveResponsive   = "alive_responsive"
	AliveUnresponsive = "alive_unresponsive"
	Deteriorating     = "deteriorating"
	Deceased          = "deceased"
)

// VictimModel parameterises one victim's exposed-skin temperature.
//
// Living: skin = air + alpha(t) * (core - air), alpha(t) = alpha0 - beta*hours
// (beta > 0 only for deteriorating victims; alpha never drops below 0.15).
// Deceased: skin cools exponentially from its value at death towards air:
// skin = air + (skinAtDeath - air) * exp(-k * hoursSinceDeath).
type VictimModel struct {
	Status      string  `json:"status"`
	Alpha0      float64 `json:"alpha0,omitempty"`
	BetaPerH    float64 `json:"beta_per_h,omitempty"`
	DeathHours  float64 `json:"death_hours,omitempty"` // hours after scenario start (negative: before)
	SkinAtDeath float64 `json:"skin_at_death_c,omitempty"`
	KPerH       float64 `json:"k_per_h,omitempty"`
}

// SkinC returns the exposed-skin temperature at elapsed hours since scenario
// start, for the given air temperature.
func (m VictimModel) SkinC(hours, airC float64) float64 {
	if m.Status == Deceased {
		since := hours - m.DeathHours
		if since < 0 { // not yet dead (scenario starts earlier): treat as living
			return airC + 0.7*(CoreC-airC)
		}
		return airC + (m.SkinAtDeath-airC)*math.Exp(-m.KPerH*since)
	}
	a := m.Alpha0 - m.BetaPerH*hours
	if a < 0.15 {
		a = 0.15
	}
	return airC + a*(CoreC-airC)
}

// CToCentiK converts Celsius to the radiometric unit used in thermal images
// (hundredths of a kelvin, as radiometric cameras report).
func CToCentiK(c float64) uint16 {
	v := math.Round((c + 273.15) * 100)
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// CentiKToC converts back to Celsius.
func CentiKToC(v uint16) float64 { return float64(v)/100 - 273.15 }
