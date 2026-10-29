package flightsim

import (
	"math"
	"testing"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/mission"
)

func uav(beh string) config.UAV {
	return config.UAV{ID: "t", Role: "scout", Behaviour: beh, AltAGLm: 40, SpeedMps: 8,
		OrbitRadiusM: 150, TelemetryHz: 1, EnduranceMin: 30, GPSNoiseM: 1.5,
		HomeOffsetEastM: -50, HomeOffsetNorthM: -600}
}

func TestLandedStaysPut(t *testing.T) {
	s := New(uav("landed"), 1)
	s.Step(10 * time.Minute)
	st := s.State()
	if st.Phase != Landed || st.AltAGL != 0 || st.East != -50 || st.North != -600 || st.BatteryPct != 100 {
		t.Fatalf("landed drone moved: %+v", st)
	}
}

func TestHoverReachesAltitude(t *testing.T) {
	s := New(uav("hover"), 1)
	s.Step(20 * time.Second) // 40 m at 3 m/s takes ~13.4 s
	st := s.State()
	if st.Phase != Hover || math.Abs(st.AltAGL-40) > 0.1 {
		t.Fatalf("want hover at 40 m, got %+v", st)
	}
	if st.East != -50 || st.North != -600 {
		t.Fatalf("hover drifted: %+v", st)
	}
}

func TestOrbitHoldsRadiusAndSpeed(t *testing.T) {
	s := New(uav("orbit"), 1)
	s.Step(3 * time.Minute) // take off + transit (~450 m at 8 m/s) + some orbit
	st := s.State()
	if st.Phase != Survey {
		t.Fatalf("want survey phase after 3 min, got %v", st.Phase)
	}
	for i := 0; i < 60; i++ {
		s.Step(time.Second)
		st = s.State()
		r := math.Hypot(st.East, st.North)
		if math.Abs(r-150) > 0.01 {
			t.Fatalf("orbit radius %.3f, want 150", r)
		}
		if math.Abs(st.SpeedMps-8) > 1e-9 || math.Abs(st.AltAGL-40) > 0.05 {
			t.Fatalf("speed/alt wrong: %+v", st)
		}
	}
	if st.RollDeg >= 0 || st.RollDeg < -30 {
		t.Fatalf("left turn should give small negative roll, got %v", st.RollDeg)
	}
}

func TestBatteryReturnLandSwap(t *testing.T) {
	u := uav("orbit")
	u.EnduranceMin = 10 // 25% reached after 7.5 min, while orbiting far from home
	s := New(u, 1)
	sawReturn, sawLanding := false, false
	for i := 0; i < 12000; i++ {
		s.Step(100 * time.Millisecond)
		switch s.State().Phase {
		case Return:
			sawReturn = true
		case Landing:
			sawLanding = true
		}
		if sawLanding && s.State().Phase == Landed {
			break
		}
	}
	st := s.State()
	if !sawReturn || !sawLanding || st.Phase != Landed {
		t.Fatalf("return/landing not seen: return=%v landing=%v state=%+v", sawReturn, sawLanding, st)
	}
	if e, n := st.East, st.North; math.Abs(e+50) > 0.01 || math.Abs(n+600) > 0.01 {
		t.Fatalf("did not land at home: %+v", st)
	}
	if st.BatteryPct > rtlBatteryPct+0.01 {
		t.Fatalf("battery should be at or below RTL level on landing, got %.2f", st.BatteryPct)
	}
	s.Step(29 * time.Second)
	if st := s.State(); st.Phase != Landed || st.BatteryPct >= 100 {
		t.Fatalf("battery swap must take 30 s on the ground: %+v", st)
	}
	s.Step(1100 * time.Millisecond)
	if st := s.State(); st.BatteryPct < 99.9 {
		t.Fatalf("battery not swapped after 30 s on ground: %+v", st)
	}
	s.Step(2 * time.Second)
	if st := s.State(); st.Phase != Takeoff || st.Flights != 2 {
		t.Fatalf("drone should take off again for its second flight: %+v", st)
	}
}

func TestDeterministic(t *testing.T) {
	l := geo.NewLocal(geo.Point{Lat: 36.2025, Lon: 36.16056})
	a, b := New(uav("orbit"), 42), New(uav("orbit"), 42)
	for i := 0; i < 100; i++ {
		a.Step(time.Second)
		b.Step(time.Second)
		fa, fb := a.GPS(l, 80), b.GPS(l, 80)
		if fa != fb {
			t.Fatalf("step %d: same seed gave different fixes %+v vs %+v", i, fa, fb)
		}
	}
}

func TestGPSNoiseMagnitude(t *testing.T) {
	l := geo.NewLocal(geo.Point{Lat: 36.2025, Lon: 36.16056})
	s := New(uav("landed"), 7)
	var sum float64
	const n = 2000
	for i := 0; i < n; i++ {
		s.Step(5 * time.Second) // samples a few correlation times apart in total
		f := s.GPS(l, 80)
		e, no := l.ToLocal(f.Point)
		sum += (e+50)*(e+50) + (no+600)*(no+600)
	}
	rms := math.Sqrt(sum / n) // expected sqrt(2)*1.5 = 2.12 m
	if rms < 1.8 || rms > 2.5 {
		t.Fatalf("GPS noise RMS %.2f m outside expected range", rms)
	}
}

// The GPS error drifts: two fixes a second apart differ much less than two
// fixes far apart.
func TestGPSErrorIsCorrelated(t *testing.T) {
	s := New(uav("landed"), 3)
	var near, far float64
	const n = 500
	for i := 0; i < n; i++ {
		e0, n0, _ := s.ReportedENU()
		s.Step(time.Second)
		e1, n1, _ := s.ReportedENU()
		near += math.Hypot(e1-e0, n1-n0)
		s.Step(120 * time.Second)
		e2, n2, _ := s.ReportedENU()
		far += math.Hypot(e2-e1, n2-n1)
	}
	if near/n > 0.6 || far/n < 1.5 {
		t.Fatalf("mean change after 1 s %.2f m, after 120 s %.2f m", near/n, far/n)
	}
}

func surveyUAV() config.UAV {
	u := uav("survey")
	u.AltAGLm, u.YawNoiseDeg = 30, 1
	return u
}

// A survey drone takes off, flies every waypoint (Survey on survey legs),
// returns home, lands and stays down; RestartMission flies it again.
func TestSurveyMission(t *testing.T) {
	s := New(surveyUAV(), 1)
	m := mission.Mission{ID: "m", Waypoints: []mission.Waypoint{
		{E: 0, N: -500, AltAGL: 30, SpeedMps: 10},
		{E: 100, N: -500, AltAGL: 30, SpeedMps: 5, Survey: true},
		{E: 100, N: -480, AltAGL: 25, SpeedMps: 10},
		{E: 0, N: -480, AltAGL: 25, SpeedMps: 5, Survey: true},
	}}
	s.SetMission(m)
	sawSurvey := false
	for i := 0; i < 600 && !(s.State().Phase == Landed && s.State().Flights > 0); i++ {
		s.Step(time.Second)
		st := s.State()
		if st.Phase == Survey {
			sawSurvey = true
			if math.Abs(st.SpeedMps-5) > 0.01 {
				t.Fatalf("survey leg at %.1f m/s", st.SpeedMps)
			}
		}
	}
	id, step, steps, done := s.MissionStatus()
	st := s.State()
	if !sawSurvey || !done || step != steps || id != "m" || st.Phase != Landed || st.East != -50 || st.North != -600 {
		t.Fatalf("after mission: survey %v done %v step %d/%d state %+v", sawSurvey, done, step, steps, st)
	}
	s.Step(5 * time.Minute)
	if s.State().Phase != Landed {
		t.Fatal("drone took off again after completing its mission")
	}
	s.RestartMission()
	if _, step, _, done := s.MissionStatus(); done || step != 0 {
		t.Fatalf("restart: step %d done %v", step, done)
	}
	s.Step(15 * time.Second) // battery was swapped while landed: takes off at once
	if st := s.State(); st.Phase == Landed || st.Flights != 2 {
		t.Fatalf("restart: %+v", st)
	}
}

// Battery return in the middle of a mission resumes at the same waypoint.
func TestSurveyResumesAfterBatterySwap(t *testing.T) {
	u := surveyUAV()
	u.EnduranceMin = 2
	s := New(u, 1)
	var wps []mission.Waypoint
	for i := 0; i < 20; i++ {
		wps = append(wps, mission.Waypoint{E: float64(i%2) * 300, N: -600 + float64(i)*10, AltAGL: 30, SpeedMps: 8, Survey: i%2 == 1})
	}
	s.SetMission(mission.Mission{ID: "long", Waypoints: wps})
	for i := 0; i < 3000 && s.State().Phase != Return; i++ {
		s.Step(100 * time.Millisecond)
	}
	_, stepAtRTL, _, _ := s.MissionStatus()
	if s.State().Phase != Return || stepAtRTL == 0 {
		t.Fatalf("expected a battery return mid-mission, step %d phase %v", stepAtRTL, s.State().Phase)
	}
	for i := 0; i < 3000 && s.State().Flights < 2; i++ {
		s.Step(100 * time.Millisecond)
	}
	if _, step, _, _ := s.MissionStatus(); s.State().Flights != 2 || step != stepAtRTL {
		t.Fatalf("second flight should resume at waypoint %d, at %d (flights %d)", stepAtRTL, step, s.State().Flights)
	}
}

func standbyUAV() config.UAV {
	u := uav("standby")
	u.Role, u.AltAGLm, u.SpeedMps, u.TaskLoiterS, u.HoverWanderM = "investigator", 15, 10, 20, 0.15
	return u
}

// stepUntil steps 100 ms at a time until cond, at most max; returns the steps taken.
func stepUntil(s *Sim, max time.Duration, cond func() bool) bool {
	for t := time.Duration(0); t < max; t += 100 * time.Millisecond {
		if cond() {
			return true
		}
		s.Step(100 * time.Millisecond)
	}
	return cond()
}

// A standby drone waits on the ground, flies a task, holds over the target at
// the task height with its heading fixed and a small drift, waits for the next
// task, then flies home and lands.
func TestStandbyTask(t *testing.T) {
	s := New(standbyUAV(), 3)
	s.Step(2 * time.Minute)
	if s.State().Phase != Landed {
		t.Fatalf("standby drone took off without a task: %v", s.State().Phase)
	}
	tk := Task{ID: "t-1", CasualtyID: "C-001", E: 60, N: -450, AltAGL: 12, HoverS: 30, RadiusM: 6}
	s.StartTask(tk)
	if !stepUntil(s, 3*time.Minute, func() bool { return s.TaskStatus().Holding }) {
		t.Fatalf("never on station: %+v %+v", s.State(), s.TaskStatus())
	}
	yaw := s.State().YawDeg
	var maxOff float64
	for s.TaskStatus().State == TaskOnStation {
		st := s.State()
		if st.Phase != Investigate || math.Abs(st.AltAGL-12) > 0.3 || st.YawDeg != yaw {
			t.Fatalf("not holding: %+v", st)
		}
		maxOff = math.Max(maxOff, math.Hypot(st.East-60, st.North+450))
		s.Step(100 * time.Millisecond)
	}
	ti := s.TaskStatus()
	if ti.State != TaskDone || ti.HoverElapsed < 30 || maxOff > 0.8 || maxOff < 0.01 {
		t.Fatalf("task %+v, drift up to %.2f m (want some drift, under 0.8 m)", ti, maxOff)
	}
	if s.State().Phase != Loiter {
		t.Fatalf("after the task: %v, want loiter", s.State().Phase)
	}
	if !stepUntil(s, 3*time.Minute, func() bool { return s.State().Phase == Landed }) {
		t.Fatalf("did not fly home: %+v", s.State())
	}
	if st := s.State(); math.Hypot(st.East+50, st.North+600) > 1.1 {
		t.Fatalf("landed away from home: %+v", st)
	}
}

// A new task replaces the one in progress; a task given while loitering is
// flown straight away.
func TestTaskPreemptionAndLoiter(t *testing.T) {
	s := New(standbyUAV(), 4)
	s.StartTask(Task{ID: "a", E: 300, N: -600, AltAGL: 15, HoverS: 10})
	s.Step(20 * time.Second)
	s.StartTask(Task{ID: "b", E: -100, N: -600, AltAGL: 15, HoverS: 10})
	if ti := s.TaskStatus(); ti.Task.ID != "b" || ti.State != TaskEnRoute {
		t.Fatalf("pre-emption: %+v", ti)
	}
	if !stepUntil(s, 2*time.Minute, func() bool { return s.TaskStatus().State == TaskDone }) {
		t.Fatalf("task b not done: %+v", s.TaskStatus())
	}
	if st := s.State(); math.Hypot(st.East+100, st.North+600) > 1 {
		t.Fatalf("flew task b to %v, %v", st.East, st.North)
	}
	s.Step(5 * time.Second)
	s.StartTask(Task{ID: "c", E: -100, N: -560, AltAGL: 15, HoverS: 10})
	if !stepUntil(s, time.Minute, func() bool { return s.TaskStatus().State == TaskDone && s.TaskStatus().Task.ID == "c" }) {
		t.Fatalf("task c from loiter: %+v %v", s.TaskStatus(), s.State().Phase)
	}
	if s.State().Flights != 1 {
		t.Fatalf("landed between tasks (flights %d)", s.State().Flights)
	}
}

// A survey drone given a task flies it and resumes its survey at the same waypoint.
func TestSurveyResumesAfterTask(t *testing.T) {
	s := New(surveyUAV(), 5)
	var wps []mission.Waypoint
	for i := 0; i < 10; i++ {
		wps = append(wps, mission.Waypoint{E: float64(i%2) * 200, N: -600 + float64(i)*10, AltAGL: 30, SpeedMps: 8, Survey: i%2 == 1})
	}
	s.SetMission(mission.Mission{ID: "m", Waypoints: wps})
	stepUntil(s, 2*time.Minute, func() bool { _, step, _, _ := s.MissionStatus(); return step == 3 })
	s.StartTask(Task{ID: "t", E: 0, N: -400, AltAGL: 15, HoverS: 10})
	if !stepUntil(s, 2*time.Minute, func() bool { return s.TaskStatus().State == TaskDone }) {
		t.Fatal("task not done")
	}
	if _, step, _, _ := s.MissionStatus(); step != 3 || s.State().Phase != Transit {
		t.Fatalf("after the task: step %d phase %v (want waypoint 3, transit)", step, s.State().Phase)
	}
	if !stepUntil(s, 5*time.Minute, func() bool { _, _, _, done := s.MissionStatus(); return done }) {
		t.Fatal("survey not completed after the task")
	}
}

// Low battery aborts a task; cancelling one goes on to loiter; a mission
// restart sends a standby drone home.
func TestTaskAbortCancelRestart(t *testing.T) {
	u := standbyUAV()
	u.EnduranceMin = 1.5
	s := New(u, 6)
	s.StartTask(Task{ID: "far", E: 400, N: 400, AltAGL: 15, HoverS: 60})
	if !stepUntil(s, 3*time.Minute, func() bool { return s.TaskStatus().State == TaskAborted }) {
		t.Fatalf("no abort: %+v", s.TaskStatus())
	}
	if ti := s.TaskStatus(); ti.Reason != "battery low" || s.State().Phase != Return {
		t.Fatalf("abort %+v phase %v", ti, s.State().Phase)
	}

	s = New(standbyUAV(), 7)
	s.StartTask(Task{ID: "x", E: 0, N: -550, AltAGL: 15, HoverS: 60})
	s.Step(20 * time.Second)
	if s.CancelTask("other") || !s.CancelTask("x") {
		t.Fatal("cancel by id")
	}
	if ti := s.TaskStatus(); ti.State != TaskAborted || ti.Reason != "cancelled" || s.State().Phase != Loiter {
		t.Fatalf("after cancel: %+v %v", ti, s.State().Phase)
	}
	s.StartTask(Task{ID: "y", E: 0, N: -550, AltAGL: 15, HoverS: 60})
	s.Step(5 * time.Second)
	s.RestartMission()
	if ti := s.TaskStatus(); ti.State != TaskAborted || s.State().Phase != Return {
		t.Fatalf("after restart: %+v %v", ti, s.State().Phase)
	}
}
