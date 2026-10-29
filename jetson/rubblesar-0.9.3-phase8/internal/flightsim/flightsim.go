// Package flightsim is a deterministic kinematic model of a small multirotor.
//
// Behaviours: "landed", "hover" (take off and hold over home), "orbit" (take
// off, fly to a circle around the incident centre and circle it), from
// phase 3 "survey" (take off, fly a mission of waypoints, return and land) and,
// from phase 4, "standby" (wait on the ground at home for investigation tasks).
//
// Any drone can be given an investigation task (phase 4): it flies to the
// target, holds position and heading over it at the task's height for the
// hover time (drifting slightly, as a real airframe does), then resumes an
// interrupted survey, or waits over the target for the next task and finally
// flies home and lands.
// Every behaviour includes battery drain, return-to-home at 25%, landing and a
// simulated 30 s battery swap; an interrupted mission resumes where it stopped.
//
// GPS and heading errors are first-order Gauss-Markov processes: they drift
// slowly (30 s and 60 s correlation times) rather than jumping on every fix,
// as real receivers do, so averaging over a few seconds does not remove them.
//
// Coordinates are local metres (east, north) around the incident centre.
package flightsim

import (
	"math"
	"math/rand"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/mission"
)

// Phase is the drone's flight phase.
type Phase int

const (
	Landed Phase = iota + 1
	Takeoff
	Transit
	Survey
	Hover
	Return
	Landing
	Investigate // holding over a task's target, watching it
	Loiter      // waiting over the last target for the next task
)

func (p Phase) String() string {
	switch p {
	case Landed:
		return "landed"
	case Takeoff:
		return "takeoff"
	case Transit:
		return "transit"
	case Survey:
		return "survey"
	case Hover:
		return "hover"
	case Return:
		return "return"
	case Landing:
		return "landing"
	case Investigate:
		return "investigate"
	case Loiter:
		return "loiter"
	}
	return "unknown"
}

// Task is an investigation: fly to (E, N), hold at AltAGL for HoverS seconds.
type Task struct {
	ID, CasualtyID string
	E, N           float64
	AltAGL         float64
	HoverS         float64
	RadiusM        float64
}

// TaskState is where a task stands on the drone.
type TaskState int

const (
	TaskNone TaskState = iota
	TaskEnRoute
	TaskOnStation
	TaskDone
	TaskAborted
)

func (t TaskState) String() string {
	return [...]string{"none", "en route", "on station", "done", "aborted"}[t]
}

// TaskInfo is the current (or last) task.
type TaskInfo struct {
	Task         Task
	State        TaskState
	HoverElapsed float64 // seconds held at the task height over the target
	Reason       string  // why it was aborted
	// Holding: on station at the task height (frames now count as watching).
	Holding bool
}

const (
	climbRate     = 3.0              // m/s up
	descentRate   = 2.0              // m/s down
	rtlBatteryPct = 25.0             // return home at or below this
	swapTime      = 30 * time.Second // battery swap on the ground
	arriveTolM    = 1.0
	gravity       = 9.80665
	maxRollDeg    = 30.0
	gpsTau        = 30.0 // s, correlation time of the GPS error
	yawTau        = 60.0 // s, correlation time of the heading error
)

// State is a snapshot of the simulated airframe.
type State struct {
	East, North float64 // metres from the incident centre
	AltAGL      float64
	YawDeg      float64
	CourseDeg   float64
	SpeedMps    float64
	ClimbMps    float64
	RollDeg     float64
	PitchDeg    float64
	BatteryPct  float64
	Phase       Phase
	Flights     int // completed take-offs
}

// Sim simulates one drone.
type Sim struct {
	cfg        config.UAV
	st         State
	homeE      float64
	homeN      float64
	groundTime time.Duration
	orbitAng   float64 // current angle on the orbit circle, radians, 0 = east
	rng        *rand.Rand

	gpsErr [3]float64 // east, north, up error of the GPS fix, metres
	yawErr float64    // heading error of the IMU, degrees

	mis      mission.Mission
	misStep  int
	misDone  bool
	misStart int // flights counter when the mission was (re)started

	task       Task
	tState     TaskState
	tReason    string
	hoverEl    float64
	resume     bool // a survey mission was interrupted by the task
	loiterLeft float64
	holdYaw    float64
	wander     [2]float64
}

// New creates a simulator for one drone. The same seed always produces the same flight.
func New(u config.UAV, seed int64) *Sim {
	s := &Sim{
		cfg:   u,
		homeE: u.HomeOffsetEastM,
		homeN: u.HomeOffsetNorthM,
		rng:   rand.New(rand.NewSource(seed)),
	}
	s.st = State{East: s.homeE, North: s.homeN, BatteryPct: 100, Phase: Landed}
	// Start the error processes in their stationary distribution.
	s.gpsErr = [3]float64{s.rng.NormFloat64() * u.GPSNoiseM, s.rng.NormFloat64() * u.GPSNoiseM, s.rng.NormFloat64() * u.GPSNoiseM * 0.5}
	s.yawErr = s.rng.NormFloat64() * u.YawNoiseDeg
	return s
}

// SetMission gives a "survey" drone its mission and starts it from the first waypoint.
func (s *Sim) SetMission(m mission.Mission) {
	s.mis = m
	s.misStep, s.misDone = 0, false
}

// RestartMission flies the mission again from the first waypoint (taking off
// again if the drone has landed).
func (s *Sim) RestartMission() {
	s.misStep, s.misDone = 0, false
	if s.taskActive() {
		s.tState, s.tReason = TaskAborted, "mission restarted"
	}
	s.resume = false
	if s.cfg.Behaviour == "standby" && s.airborne() && s.st.Phase != Landing {
		s.st.Phase = Return
	}
	switch s.st.Phase {
	case Survey, Transit, Return, Hover:
		if s.cfg.Behaviour == "survey" && s.st.BatteryPct > rtlBatteryPct {
			s.st.Phase = Transit
		}
	}
}

// StartTask gives the drone an investigation task. A task in progress is
// replaced (pre-emption). An interrupted survey is resumed afterwards.
func (s *Sim) StartTask(t Task) {
	if s.taskActive() && s.task.ID != t.ID {
		s.tState, s.tReason = TaskAborted, "replaced by task "+t.ID
	}
	if !s.taskActive() {
		s.resume = s.cfg.Behaviour == "survey" && !s.misDone && len(s.mis.Waypoints) > 0 &&
			(s.st.Phase == Transit || s.st.Phase == Survey || s.st.Phase == Takeoff)
	}
	s.task, s.tState, s.tReason, s.hoverEl = t, TaskEnRoute, "", 0
	if s.airborne() && s.st.Phase != Takeoff {
		s.st.Phase = Transit
	}
}

// CancelTask abandons the task with this id (false if it is not the current one).
func (s *Sim) CancelTask(id string) bool {
	if !s.taskActive() || s.task.ID != id {
		return false
	}
	s.tState, s.tReason = TaskAborted, "cancelled"
	s.afterTask()
	return true
}

// TaskStatus returns the current or last task.
func (s *Sim) TaskStatus() TaskInfo {
	return TaskInfo{Task: s.task, State: s.tState, HoverElapsed: s.hoverEl, Reason: s.tReason, Holding: s.holding()}
}

func (s *Sim) taskActive() bool { return s.tState == TaskEnRoute || s.tState == TaskOnStation }

func (s *Sim) holding() bool {
	return s.st.Phase == Investigate && s.tState == TaskOnStation && math.Abs(s.st.AltAGL-s.task.AltAGL) < 0.3
}

// afterTask: resume the survey, or wait over the target for the next task, or go home.
func (s *Sim) afterTask() {
	switch {
	case !s.airborne():
	case s.resume && !s.misDone:
		s.st.Phase = Transit
	case s.cfg.TaskLoiterS > 0 && s.st.BatteryPct > rtlBatteryPct:
		s.st.Phase, s.loiterLeft = Loiter, s.cfg.TaskLoiterS
	default:
		s.st.Phase = Return
	}
	s.resume = false
}

// MissionStatus reports the mission id, the waypoint being flown to (0-based),
// the number of waypoints and whether the mission is complete.
func (s *Sim) MissionStatus() (id string, step, steps int, done bool) {
	return s.mis.ID, s.misStep, len(s.mis.Waypoints), s.misDone
}

// Mission returns the current mission.
func (s *Sim) Mission() mission.Mission { return s.mis }

// State returns the current state.
func (s *Sim) State() State { return s.st }

// Config returns the drone's configuration.
func (s *Sim) Config() config.UAV { return s.cfg }

// Step advances the simulation by dt. Large steps are split into 100 ms ticks.
func (s *Sim) Step(dt time.Duration) {
	const tick = 100 * time.Millisecond
	for dt > 0 {
		d := tick
		if dt < tick {
			d = dt
		}
		s.step(d.Seconds())
		dt -= d
	}
}

func (s *Sim) airborne() bool { return s.st.Phase != Landed }

func (s *Sim) step(dt float64) {
	st := &s.st
	// Gauss-Markov errors: x <- a x + sigma sqrt(1-a^2) w.
	a, ay := math.Exp(-dt/gpsTau), math.Exp(-dt/yawTau)
	for i := range s.gpsErr {
		sigma := s.cfg.GPSNoiseM
		if i == 2 {
			sigma *= 0.5
		}
		s.gpsErr[i] = a*s.gpsErr[i] + sigma*math.Sqrt(1-a*a)*s.rng.NormFloat64()
	}
	s.yawErr = ay*s.yawErr + s.cfg.YawNoiseDeg*math.Sqrt(1-ay*ay)*s.rng.NormFloat64()
	if s.airborne() {
		st.BatteryPct -= dt * 100 / (s.cfg.EnduranceMin * 60)
		if st.BatteryPct < 0 {
			st.BatteryPct = 0
		}
		if st.BatteryPct <= rtlBatteryPct && st.Phase != Return && st.Phase != Landing {
			if s.taskActive() {
				s.tState, s.tReason = TaskAborted, "battery low"
			}
			s.resume = false
			st.Phase = Return
		}
	}

	pending := s.tState == TaskEnRoute
	switch st.Phase {
	case Landed:
		st.SpeedMps, st.ClimbMps, st.RollDeg, st.PitchDeg = 0, 0, 0, 0
		if pending && st.BatteryPct >= 50 {
			// A task: take off now (a battery swap in progress is cut short).
			s.groundTime = 0
			st.Phase = Takeoff
			st.Flights++
			return
		}
		if st.BatteryPct < 100 {
			s.groundTime += time.Duration(dt * float64(time.Second))
			if s.groundTime >= swapTime {
				st.BatteryPct = 100
				s.groundTime = 0
			}
			return
		}
		if s.cfg.Behaviour == "survey" && (s.misDone || len(s.mis.Waypoints) == 0) {
			return // mission complete (or none): stay on the ground
		}
		if s.cfg.Behaviour == "standby" {
			return // wait for a task
		}
		if s.cfg.Behaviour != "landed" {
			st.Phase = Takeoff
			st.Flights++
		}
	case Takeoff:
		s.climbTo(s.cfg.AltAGLm, dt)
		if math.Abs(st.AltAGL-s.cfg.AltAGLm) < 0.05 {
			if pending {
				st.Phase = Transit
				return
			}
			switch s.cfg.Behaviour {
			case "orbit":
				st.Phase = Transit
				s.orbitAng = math.Atan2(st.North, st.East) // nearest point on the circle
			case "survey":
				st.Phase = Transit
			default:
				st.Phase = Hover
			}
		}
	case Transit, Survey:
		if pending {
			s.flyTask(dt)
			return
		}
		if s.cfg.Behaviour == "standby" {
			st.Phase = Return
			return
		}
		if s.cfg.Behaviour == "survey" {
			s.flyMission(dt)
			return
		}
		if st.Phase == Survey {
			s.orbit(dt)
			return
		}
		r := s.cfg.OrbitRadiusM
		te, tn := r*math.Cos(s.orbitAng), r*math.Sin(s.orbitAng)
		if s.flyTo(te, tn, s.cfg.SpeedMps, dt) {
			st.Phase = Survey
		}
	case Investigate:
		s.holdTask(dt)
	case Loiter:
		st.SpeedMps, st.ClimbMps, st.RollDeg, st.PitchDeg = 0, 0, 0, 0
		s.loiterLeft -= dt
		if s.loiterLeft <= 0 {
			st.Phase = Return
		}
	case Hover:
		st.SpeedMps, st.ClimbMps, st.RollDeg, st.PitchDeg = 0, 0, 0, 0
		st.YawDeg = geo.NormDeg(st.YawDeg + 2*dt) // slow pirouette so the camera sweeps
	case Return:
		s.climbTo(s.cfg.AltAGLm, dt)
		if s.flyTo(s.homeE, s.homeN, s.cfg.SpeedMps, dt) {
			st.Phase = Landing
		}
	case Landing:
		st.SpeedMps, st.RollDeg, st.PitchDeg = 0, 0, 0
		st.ClimbMps = -descentRate
		st.AltAGL -= descentRate * dt
		if st.AltAGL <= 0 {
			st.AltAGL, st.ClimbMps = 0, 0
			st.Phase = Landed
			s.groundTime = 0
		}
	}
}

// flyTask flies to the task's target at the cruise height, then goes on station.
func (s *Sim) flyTask(dt float64) {
	st := &s.st
	st.Phase = Transit
	s.climbTo(s.cfg.AltAGLm, dt)
	if s.flyTo(s.task.E, s.task.N, s.cfg.SpeedMps, dt) {
		st.Phase = Investigate
		s.tState = TaskOnStation
		s.holdYaw = st.YawDeg
		s.wander = [2]float64{}
	}
}

// holdTask holds over the target at the task height, heading fixed, with a
// small drift (first-order Gauss-Markov, 5 s), and counts the time watched.
func (s *Sim) holdTask(dt float64) {
	st := &s.st
	if s.tState != TaskOnStation {
		s.afterTask()
		return
	}
	s.climbTo(s.task.AltAGL, dt)
	a := math.Exp(-dt / 5)
	for i := range s.wander {
		s.wander[i] = a*s.wander[i] + s.cfg.HoverWanderM*math.Sqrt(1-a*a)*s.rng.NormFloat64()
	}
	st.East, st.North = s.task.E+s.wander[0], s.task.N+s.wander[1]
	st.YawDeg = s.holdYaw
	st.SpeedMps, st.RollDeg, st.PitchDeg = 0, 0, 0
	if math.Abs(st.AltAGL-s.task.AltAGL) < 0.3 {
		s.hoverEl += dt
	}
	if s.hoverEl >= s.task.HoverS {
		s.tState = TaskDone
		s.afterTask()
	}
}

func (s *Sim) climbTo(alt, dt float64) {
	st := &s.st
	diff := alt - st.AltAGL
	step := math.Min(math.Abs(diff), climbRate*dt)
	if diff < 0 {
		step = -step
	}
	st.AltAGL += step
	if dt > 0 {
		st.ClimbMps = step / dt
	}
}

// flyMission flies towards the current waypoint; Survey on survey legs,
// Transit otherwise. After the last waypoint the drone returns home and lands.
func (s *Sim) flyMission(dt float64) {
	st := &s.st
	if s.misStep >= len(s.mis.Waypoints) {
		s.misDone = true
		st.Phase = Return
		return
	}
	w := s.mis.Waypoints[s.misStep]
	if w.Survey {
		st.Phase = Survey
	} else {
		st.Phase = Transit
	}
	s.climbTo(w.AltAGL, dt)
	speed := w.SpeedMps
	if speed <= 0 {
		speed = s.cfg.SpeedMps
	}
	if s.flyTo(w.E, w.N, speed, dt) {
		s.misStep++
	}
}

// flyTo moves straight towards (e, n) at the given speed; true when arrived.
func (s *Sim) flyTo(e, n, speed, dt float64) bool {
	st := &s.st
	de, dn := e-st.East, n-st.North
	dist := math.Hypot(de, dn)
	if dist <= arriveTolM {
		st.East, st.North = e, n
		st.SpeedMps, st.RollDeg, st.PitchDeg = 0, 0, 0
		return true
	}
	step := math.Min(dist, speed*dt)
	st.East += de / dist * step
	st.North += dn / dist * step
	st.SpeedMps = step / dt
	st.CourseDeg = geo.HeadingFromVector(de, dn)
	st.YawDeg = st.CourseDeg
	st.RollDeg = 0
	st.PitchDeg = -2 * st.SpeedMps / 4 // nose down in forward flight
	return false
}

// orbit flies anticlockwise around the incident centre.
func (s *Sim) orbit(dt float64) {
	st := &s.st
	r := s.cfg.OrbitRadiusM
	omega := s.cfg.SpeedMps / r // rad/s
	s.orbitAng = math.Mod(s.orbitAng+omega*dt, 2*math.Pi)
	st.East, st.North = r*math.Cos(s.orbitAng), r*math.Sin(s.orbitAng)
	// Velocity is tangent to the circle (anticlockwise).
	ve, vn := -math.Sin(s.orbitAng), math.Cos(s.orbitAng)
	st.CourseDeg = geo.HeadingFromVector(ve, vn)
	st.YawDeg = st.CourseDeg
	st.SpeedMps = s.cfg.SpeedMps
	st.ClimbMps = 0
	roll := math.Atan(st.SpeedMps*omega/gravity) * 180 / math.Pi
	st.RollDeg = -math.Min(roll, maxRollDeg) // left turn = negative roll
	st.PitchDeg = -2 * st.SpeedMps / 4
}

// Fix is a simulated GPS fix.
type Fix struct {
	Point  geo.Point
	AltMSL float64
	HAccM  float64
	AltAGL float64
}

// GPS returns the current position as the (simulated) GPS receiver reports it.
func (s *Sim) GPS(local geo.Local, groundAltMSL float64) Fix {
	e, n, alt := s.ReportedENU()
	return Fix{Point: local.ToPoint(e, n), AltMSL: groundAltMSL + alt, HAccM: s.cfg.GPSNoiseM, AltAGL: alt}
}

// ReportedENU is the GPS position in local metres and the height above ground.
func (s *Sim) ReportedENU() (e, n, altAGL float64) {
	e, n = s.st.East+s.gpsErr[0], s.st.North+s.gpsErr[1]
	altAGL = math.Max(0, s.st.AltAGL+s.gpsErr[2])
	return
}

// ReportedYaw is the heading as the IMU reports it, degrees.
func (s *Sim) ReportedYaw() float64 { return geo.NormDeg(s.st.YawDeg + s.yawErr) }
