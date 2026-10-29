// Package config loads and validates RubbleSAR TOML configuration files.
//
// Every program reads one file. Shared sections ([site], [zenoh], [log]) have
// the same meaning everywhere. Environment variables override a few fields so
// that systemd units can change them without editing the file:
//
//	SAR_ZENOH_CONNECT  comma-separated endpoints, e.g. tcp/192.168.1.50:7447
//	SAR_INCIDENT_ID    incident id
//	SAR_LOG_LEVEL      debug|info|warn|error (applied by logx)
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"rubblesar/internal/diag"
	"rubblesar/internal/keys"
)

// Duration parses Go duration strings such as "10s" or "1m30s".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Site describes the incident.
type Site struct {
	IncidentID    string  `toml:"incident_id"`
	CenterLat     float64 `toml:"center_lat"`
	CenterLon     float64 `toml:"center_lon"`
	GroundAltMSLm float64 `toml:"ground_alt_msl_m"`
	RadiusM       float64 `toml:"radius_m"`
	// ScenarioDir holds scenario.json and the rendered layers (phase 1+).
	// Empty = /var/lib/rubblesar/scenarios/<incident_id>.
	ScenarioDir string `toml:"scenario_dir"`
}

// Scenario returns the scenario folder.
func (s Site) Scenario() string {
	if s.ScenarioDir != "" {
		return s.ScenarioDir
	}
	return "/var/lib/rubblesar/scenarios/" + s.IncidentID
}

// Zenoh configures the bus session.
type Zenoh struct {
	Mode        string   `toml:"mode"`    // client | peer
	Connect     []string `toml:"connect"` // e.g. ["tcp/192.168.1.50:7447"]
	Listen      []string `toml:"listen"`  // peer mode only
	OpenTimeout Duration `toml:"open_timeout"`
	// RetryForever keeps retrying the first connection (edge-eye); tools give up after OpenTimeout.
	RetryForever bool `toml:"retry_forever"`
	// ConfigFile, if set, is a full zenoh JSON5 config used instead of the fields above.
	ConfigFile string `toml:"config_file"`
}

// Log configures logging.
type Log struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// Common holds the sections every program shares.
type Common struct {
	Site  Site  `toml:"site"`
	Zenoh Zenoh `toml:"zenoh"`
	Log   Log   `toml:"log"`
}

// UAV is one simulated drone hosted by edge-eye.
type UAV struct {
	ID               string  `toml:"id"`
	Role             string  `toml:"role"`      // scout | investigator
	Behaviour        string  `toml:"behaviour"` // survey | standby | orbit | hover | landed
	HomeOffsetEastM  float64 `toml:"home_offset_east_m"`
	HomeOffsetNorthM float64 `toml:"home_offset_north_m"`
	AltAGLm          float64 `toml:"alt_agl_m"`
	SpeedMps         float64 `toml:"speed_mps"`
	OrbitRadiusM     float64 `toml:"orbit_radius_m"`
	TelemetryHz      float64 `toml:"telemetry_hz"`
	EnduranceMin     float64 `toml:"endurance_min"`
	GPSNoiseM        float64 `toml:"gps_noise_m"`
	BudgetKbps       *uint32 `toml:"budget_kbps"` // nil = use [link] default
	// Heading error of the IMU (1 sigma, degrees; slowly drifting).
	YawNoiseDeg float64 `toml:"yaw_noise_deg"`
	// Phase 3 "survey" behaviour: sites to survey in order (empty = all, in scenario order).
	SurveySites    []string `toml:"survey_sites"`
	SideOverlap    float64  `toml:"side_overlap"`     // share of the swath neighbouring lines share
	SurveySpeedMps float64  `toml:"survey_speed_mps"` // along survey lines (speed_mps in transit)
	// Camera frames per second (0 = [perception] frame_hz).
	FrameHz float64 `toml:"frame_hz"`
	// Phase 4: after a task, wait this long over the last target for the next
	// one before flying home ("standby" drones).
	TaskLoiterS float64 `toml:"task_loiter_s"`
	// Simulated drift of the airframe while holding over a target (1 sigma, metres).
	HoverWanderM float64 `toml:"hover_wander_m"`
}

// Admin configures edge-eye's local admin HTTP API.
type Admin struct {
	Listen string `toml:"listen"`
}

// Link configures the simulated radio link.
type Link struct {
	DefaultBudgetKbps uint32 `toml:"default_budget_kbps"`
}

// Perception configures edge-eye's onboard pipeline (phase 3).
type Perception struct {
	Enabled  bool   `toml:"enabled"`
	ModelDir string `toml:"model_dir"`
	// "model" (the installed model) or "truth-oracle" (detections made from the
	// simulated ground truth; for testing the pipeline only - sar-check phase3
	// does not accept a run made with it).
	Detector string  `toml:"detector"`
	Conf     float64 `toml:"conf"`     // detection confidence threshold
	FrameHz  float64 `toml:"frame_hz"` // camera frames per second per drone (per-drone frame_hz overrides)
	// Human skin band and contrast for the thermal cue, degrees C.
	SkinMinC   float64 `toml:"skin_min_c"`
	SkinMaxC   float64 `toml:"skin_max_c"`
	WarmDeltaC float64 `toml:"warm_delta_c"`
	// A temperature reading needs at least this many thermal pixels on the part.
	MinThermalPx int `toml:"min_thermal_px"`
	// Warm spots with no body part seen in colour become low-confidence candidates.
	WarmSpots     bool `toml:"warm_spots"`
	WarmSpotMinPx int  `toml:"warm_spot_min_px"`
	// Report a track once it was seen in this many frames.
	ConfirmHits int `toml:"confirm_hits"`
	// Updates of a reported track at most this often.
	UpdateMinInterval Duration `toml:"update_min_interval"`
	// Simulation: publish the ground truth of every frame for scoring
	// (sar/{incident}/sim/uav/{uav}/truth; not counted against the radio budget).
	PublishTruth bool `toml:"publish_truth"`
	// Scenario clock when edge-eye starts, seconds after the scenario start.
	SimStartS      float64 `toml:"sim_start_s"`
	CameraPitchDeg float64 `toml:"camera_pitch_deg"`
	// Phase 4. Position error of a detection the pose errors do not explain
	// (the model's outline, the part's extent), 1 sigma per axis, metres;
	// added to every reported CEP. 1.0 calibrated from the phase 3 runs.
	DetectionSigmaM float64 `toml:"detection_sigma_m"`
	// Phase 4. Height of the debris surface above the ground that body parts
	// are assumed to lie on when they are geolocated, metres (0 = flat ground).
	DebrisHeightM float64 `toml:"debris_height_m"`
	// Movement while hovering: a frame "moved" when more than movement_change
	// of the part's outline changed by more than movement_tolerance_px (after
	// correcting for the drone's drift); movement needs movement_min_frames such frames.
	MovementChange      float64 `toml:"movement_change"`
	MovementTolerancePx int     `toml:"movement_tolerance_px"`
	MovementMinFrames   int     `toml:"movement_min_frames"`
}

// Outbox configures the detection queue (store-and-forward).
type Outbox struct {
	Dir        string   `toml:"dir"`
	MaxQueueMB int      `toml:"max_queue_mb"`
	AckTimeout Duration `toml:"ack_timeout"`
	MaxBackoff Duration `toml:"max_backoff"`
	// With nothing acknowledged for this long, send only an occasional probe.
	ProbeAfter Duration `toml:"probe_after"`
}

// Edge is edge-eye's configuration (Jetson).
type Edge struct {
	Common
	Admin      Admin      `toml:"admin"`
	Link       Link       `toml:"link"`
	Perception Perception `toml:"perception"`
	Outbox     Outbox     `toml:"outbox"`
	UAVs       []UAV      `toml:"uav"`
	Seed       int64      `toml:"seed"`
}

// FrameHz is the camera rate of one drone.
func (c *Edge) FrameHz(u UAV) float64 {
	if u.FrameHz > 0 {
		return u.FrameHz
	}
	return c.Perception.FrameHz
}

// Check configures sar-check acceptance tests (ZBook).
type Check struct {
	ExpectUAVs       []string `toml:"expect_uavs"`
	ExpectRateHz     float64  `toml:"expect_rate_hz"`
	DiscoverTimeout  Duration `toml:"discover_timeout"`
	RateWindow       Duration `toml:"rate_window"`
	RateTolerancePct float64  `toml:"rate_tolerance_pct"`
	MaxClockOffset   Duration `toml:"max_clock_offset"`
	PingTimeout      Duration `toml:"ping_timeout"`
	ReportDir        string   `toml:"report_dir"`
}

// Monitor configures sar-monitor.
type Monitor struct {
	Refresh    Duration `toml:"refresh"`
	StaleAfter Duration `toml:"stale_after"`
}

// Analyst configures casualty-analyst (phase 3: receiving, storing and
// acknowledging detections; phase 4 adds casualty records).
type Analyst struct {
	// Detections are stored in <store_dir>/<incident_id>/.
	StoreDir    string   `toml:"store_dir"`
	SaveCrops   bool     `toml:"save_crops"`
	AckInterval Duration `toml:"ack_interval"`
	// Local status API ('casualty-analyst status').
	Listen string `toml:"listen"`
	Fusion Fusion `toml:"fusion"`
	Triage Triage `toml:"triage"`
}

// Fusion configures how detection reports become casualties (phase 4).
type Fusion struct {
	// Parts one drone saw within link_m of each other (at about the same time)
	// belong to one person; a person spans at most span_m.
	LinkM  float64  `toml:"link_m"`
	SpanM  float64  `toml:"span_m"`
	Window Duration `toml:"window"`
	// A person shows at most two hands, arms and legs and one head and torso;
	// parts of one class within duplicate_m of each other count once (a real
	// model reports one leg as two or three pieces). 0: every part counts.
	DuplicateM float64 `toml:"duplicate_m"`
	// Records from different drones are the same casualty within
	// max(match_min_m, match_ceps x their combined CEP), one-to-one per drone.
	MatchMinM float64 `toml:"match_min_m"`
	MatchCEPs float64 `toml:"match_ceps"`
	// Full casualty list publication interval.
	PublishInterval Duration `toml:"publish_interval"`
}

// Triage configures the provisional status and START colour (plan decision D9:
// every threshold is subject to medical sign-off).
type Triage struct {
	// Warm: bare skin (hand, head, warm spot) within skin_min_c..skin_max_c
	// and at least warm_delta_c above ambient. When only parts usually clothed
	// are read (arm, leg, torso): within clothed_min_c..skin_max_c and at least
	// clothed_delta_c above ambient (sun-warmed clothing on a body that has
	// cooled reads several degrees above ambient).
	SkinMinC      float64 `toml:"skin_min_c"`
	SkinMaxC      float64 `toml:"skin_max_c"`
	WarmDeltaC    float64 `toml:"warm_delta_c"`
	ClothedMinC   float64 `toml:"clothed_min_c"`
	ClothedDeltaC float64 `toml:"clothed_delta_c"`
	// Readings from closer than this count as a close look, metres.
	CloseRangeM float64 `toml:"close_range_m"`
	// Seconds of close watching needed before "no movement" counts.
	MinWatchS float64 `toml:"min_watch_s"`
	// Moved: a hover aimed at the casualty saw its parts move in at least
	// this many frames in all (two parts moving in one frame count twice).
	MovedFrames int `toml:"moved_frames"`
	// Deteriorating when skin cools faster than this (degrees C per hour), over
	// readings spanning at least trend_min_span.
	CoolingCPerH float64  `toml:"cooling_c_per_h"`
	TrendMinSpan Duration `toml:"trend_min_span"`
	// Investigate when the position is less certain than this (CEP, metres).
	MaxCEPm float64 `toml:"max_cep_m"`
	// After this many close looks that found nothing, the casualty is "not found".
	NotFoundLooks int `toml:"not_found_looks"`
}

// Planner configures mission-planner (phase 4): investigation tasks.
type Planner struct {
	Listen string `toml:"listen"`
	// Investigation: height, hover time and the radius around the casualty.
	AltAGLm float64 `toml:"alt_agl_m"`
	HoverS  int     `toml:"hover_s"`
	RadiusM float64 `toml:"radius_m"`
	// A drone takes a task only if it keeps this much battery after it (and the flight home).
	ReservePct float64 `toml:"reserve_pct"`
	// Investigations per casualty at most.
	MaxAttempts int `toml:"max_attempts"`
	// A task with no progress for this long fails and is offered again.
	TaskTimeout Duration `toml:"task_timeout"`
	// Waiting for the drone to answer a command.
	CommandTimeout Duration `toml:"command_timeout"`
	// Drones that take investigations (empty = every drone with role investigator).
	Investigators []string `toml:"investigators"`
	// Task list publication interval.
	PublishInterval Duration `toml:"publish_interval"`
	// The task list is kept in <state_dir>/<incident>.json across restarts.
	StateDir string `toml:"state_dir"`
}

// Dispatch configures rescue-dispatch (phase 5): rescue teams, routes and the TAK map.
type Dispatch struct {
	Listen string `toml:"listen"`
	// Streets around the incident (Overpass JSON from 'scenario-tool fetch-roads';
	// "" = /var/lib/rubblesar/roads/<incident>.json).
	Roads string `toml:"roads"`
	// Streets passing within this distance of a rubble spill area are closed.
	BlockMarginM float64 `toml:"block_margin_m"`
	// TAK server, plain TCP host:port ("" = no TAK map).
	TakServer string `toml:"tak_server"`
	// How rescue-dispatch appears on the TAK map and in chat.
	Callsign string `toml:"callsign"`
	Group    string `toml:"group"`
	// Casualty markers are sent again this often and expire this long after the last.
	MarkerRefresh Duration `toml:"marker_refresh"`
	MarkerStale   Duration `toml:"marker_stale"`
	// Drone positions on the map at most this often.
	DroneEvery Duration `toml:"drone_every"`
	// Show the investigating drones' lines to their casualties.
	ShowInvestigations bool `toml:"show_investigations"`
	// Show the scenario's rubble areas as shapes on the TAK map (phase 6).
	ShowCollapses bool `toml:"show_collapses"`
	// Triage colours that get a rescue team, in priority order.
	Triage []string `toml:"triage"`
	// A team within this distance of its casualty has reached it.
	ArriveM float64 `toml:"arrive_m"`
	// Time a team spends with a casualty once it is free of the rubble
	// (treatment and handover) before the team is free again (a TAK team can
	// say "done" sooner).
	OnScene Duration `toml:"on_scene"`
	// Extrication: before that, the time to dig the casualty out, from the
	// scenario (how the building collapsed, how deep, what shows) and the
	// team's tools; false = none.
	Extrication bool `toml:"extrication"`
	// Simulated teams move this many times faster than real people (1 = real time).
	TimeScale float64 `toml:"time_scale"`
	// Simulated teams free casualties and hand them over this many times
	// faster than real people would; the times shown are real ones.
	RescueTimeScale float64 `toml:"rescue_time_scale"`
	// A TAK team that neither answers nor moves towards its casualty for this
	// long loses it to another team (0 = never).
	TakAckTimeout Duration `toml:"tak_ack_timeout"`
	// A TAK team with no position report for this long is offline (ATAK
	// reports less often while standing still).
	TeamOffline Duration `toml:"team_offline"`
	// Where the teams start (the command post). 0, 0 = the incident centre.
	StagingLat float64 `toml:"staging_lat"`
	StagingLon float64 `toml:"staging_lon"`
	// A casualty gets a team once it has been listed this long.
	MinAge          Duration `toml:"min_age"`
	PublishInterval Duration `toml:"publish_interval"`
	StateDir        string   `toml:"state_dir"`
	Teams           []Team   `toml:"team"`
}

// Team is one rescue team.
type Team struct {
	ID       string `toml:"id"`
	Callsign string `toml:"callsign"`
	// "foot" or "vehicle".
	Mode string `toml:"mode"`
	// "sim": moved by rescue-dispatch; "tak": a TAK user (ATAK, WinTAK) with this callsign.
	Source string `toml:"source"`
}

// DefaultTeams are used when the config lists none.
func DefaultTeams() []Team {
	return []Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"}, {ID: "team-2", Callsign: "TEAM-2", Mode: "foot", Source: "sim"},
		{ID: "team-3", Callsign: "TEAM-3", Mode: "vehicle", Source: "sim"}, {ID: "check-1", Callsign: "CHECK-1", Mode: "foot", Source: "tak"}}
}

// ZBook is the configuration shared by the ZBook tools.
type ZBook struct {
	Common
	Check    Check    `toml:"check"`
	Monitor  Monitor  `toml:"monitor"`
	Analyst  Analyst  `toml:"analyst"`
	Planner  Planner  `toml:"planner"`
	Dispatch Dispatch `toml:"dispatch"`
	Web      Web      `toml:"web"`
	Ops      Ops      `toml:"ops"`
}

// Web configures sar-web (phase 6), which serves the offline map and the
// after-action reports to the network.
type Web struct {
	Listen string `toml:"listen"`
	// Root holds map/ (tiles, MBTiles, map source XML) and reports/.
	Root string `toml:"root"`
}

// Ops configures the rubblesar command (phase 6).
type Ops struct {
	// Jetson is user@address for SSH (scripts/zbook/38-jetson-link.sh sets
	// it); "" = the Jetson's edge-eye is not started or stopped from here.
	Jetson string `toml:"jetson"`
	// The Jetson's systemd service.
	JetsonService string `toml:"jetson_service"`
	// A mission run ends when everything is done, or after this long.
	MaxMission Duration `toml:"max_mission"`
}

// LoadEdge reads and validates an edge-eye config.
func LoadEdge(path string) (*Edge, error) {
	c := &Edge{}
	c.applyDefaults()
	if err := load(path, c); err != nil {
		return nil, err
	}
	c.Common.applyEnv()
	for i := range c.UAVs {
		c.UAVs[i].applyDefaults(c.Link.DefaultBudgetKbps)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadZBook reads and validates a ZBook tools config.
func LoadZBook(path string) (*ZBook, error) {
	c := &ZBook{}
	c.applyDefaults()
	if err := load(path, c); err != nil {
		return nil, err
	}
	c.Common.applyEnv()
	if len(c.Dispatch.Teams) == 0 {
		c.Dispatch.Teams = DefaultTeams()
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func load(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return diag.Wrap("E-CFG-001", err, "config %s", path)
		}
		return diag.Wrap("E-CFG-001", err, "reading %s", path)
	}
	md, err := toml.Decode(string(b), v)
	if err != nil {
		return diag.Wrap("E-CFG-002", err, "%s", path)
	}
	if und := md.Undecoded(); len(und) > 0 {
		names := make([]string, len(und))
		for i, k := range und {
			names[i] = k.String()
		}
		return diag.New("E-CFG-003", "%s: unknown setting(s) %s (typo?)", path, strings.Join(names, ", "))
	}
	return nil
}

// Antakya, Hatay, Türkiye: heavily affected by the 6 February 2023 earthquakes.
const (
	defaultLat = 36.20250
	defaultLon = 36.16056
)

func (c *Common) applyDefaults() {
	c.Site = Site{IncidentID: "antakya-demo", CenterLat: defaultLat, CenterLon: defaultLon, GroundAltMSLm: 80, RadiusM: 805}
	c.Zenoh = Zenoh{Mode: "client", Connect: []string{"tcp/127.0.0.1:7447"}, OpenTimeout: Duration{10 * time.Second}}
	c.Log = Log{Level: "info", Format: "text"}
}

func (c *Common) applyEnv() {
	if v := strings.TrimSpace(os.Getenv("SAR_ZENOH_CONNECT")); v != "" {
		var eps []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				eps = append(eps, p)
			}
		}
		c.Zenoh.Connect = eps
	}
	if v := strings.TrimSpace(os.Getenv("SAR_INCIDENT_ID")); v != "" {
		c.Site.IncidentID = v
	}
}

func (c *Edge) applyDefaults() {
	c.Common.applyDefaults()
	c.Zenoh.RetryForever = true
	c.Admin.Listen = "127.0.0.1:9101"
	c.Link.DefaultBudgetKbps = 256
	c.Seed = 1
	c.Perception = Perception{Enabled: false, ModelDir: "/var/lib/rubblesar/models/current", Detector: "model", Conf: 0.25, FrameHz: 1,
		SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinThermalPx: 9, WarmSpots: true, WarmSpotMinPx: 3, ConfirmHits: 2,
		UpdateMinInterval: Duration{5 * time.Second}, PublishTruth: true, CameraPitchDeg: -90,
		DetectionSigmaM: 1.0, MovementChange: 0.12, MovementTolerancePx: 2, MovementMinFrames: 2}
	c.Outbox = Outbox{Dir: "/var/lib/rubblesar/edge/queue", MaxQueueMB: 200, AckTimeout: Duration{8 * time.Second},
		MaxBackoff: Duration{60 * time.Second}, ProbeAfter: Duration{20 * time.Second}}
}

func (u *UAV) applyDefaults(defBudget uint32) {
	if u.Behaviour == "" {
		u.Behaviour = "hover"
	}
	if u.SpeedMps == 0 {
		u.SpeedMps = 8
	}
	if u.TelemetryHz == 0 {
		u.TelemetryHz = 1
	}
	if u.EnduranceMin == 0 {
		u.EnduranceMin = 30
	}
	if u.GPSNoiseM == 0 {
		u.GPSNoiseM = 1.5
	}
	if u.Behaviour == "orbit" && u.OrbitRadiusM == 0 {
		u.OrbitRadiusM = 150
	}
	if u.YawNoiseDeg == 0 {
		u.YawNoiseDeg = 1
	}
	if u.SideOverlap == 0 {
		u.SideOverlap = 0.7
	}
	if u.SurveySpeedMps == 0 {
		u.SurveySpeedMps = u.SpeedMps
	}
	if u.BudgetKbps == nil {
		b := defBudget
		u.BudgetKbps = &b
	}
	if u.TaskLoiterS == 0 {
		u.TaskLoiterS = 20
	}
	if u.HoverWanderM == 0 {
		u.HoverWanderM = 0.15
	}
}

func (c *ZBook) applyDefaults() {
	c.Common.applyDefaults()
	c.Check = Check{
		ExpectUAVs:       []string{"scout-1", "inv-1", "inv-2"},
		ExpectRateHz:     1,
		DiscoverTimeout:  Duration{20 * time.Second},
		RateWindow:       Duration{30 * time.Second},
		RateTolerancePct: 10,
		MaxClockOffset:   Duration{250 * time.Millisecond},
		PingTimeout:      Duration{2 * time.Second},
		ReportDir:        "reports",
	}
	c.Monitor = Monitor{Refresh: Duration{time.Second}, StaleAfter: Duration{5 * time.Second}}
	c.Analyst = Analyst{StoreDir: "/var/lib/rubblesar/detections", SaveCrops: true, AckInterval: Duration{200 * time.Millisecond}, Listen: "127.0.0.1:9102",
		Fusion: Fusion{LinkM: 2.0, SpanM: 2.5, DuplicateM: 1.5, Window: Duration{10 * time.Second}, MatchMinM: 5, MatchCEPs: 3, PublishInterval: Duration{10 * time.Second}},
		Triage: Triage{SkinMinC: 20, SkinMaxC: 40, ClothedMinC: 15, ClothedDeltaC: 8, WarmDeltaC: 3, CloseRangeM: 20, MinWatchS: 20, MovedFrames: 2, CoolingCPerH: 0.5,
			TrendMinSpan: Duration{10 * time.Minute}, MaxCEPm: 5, NotFoundLooks: 2}}
	c.Planner = Planner{Listen: "127.0.0.1:9103", AltAGLm: 15, HoverS: 30, RadiusM: 6, ReservePct: 25, MaxAttempts: 2,
		TaskTimeout: Duration{4 * time.Minute}, CommandTimeout: Duration{3 * time.Second}, PublishInterval: Duration{5 * time.Second},
		StateDir: "/var/lib/rubblesar/planner"}
	c.Dispatch = Dispatch{Listen: "127.0.0.1:9104", BlockMarginM: 1,
		TakServer: "127.0.0.1:8087", Callsign: "SAR-OPS", Group: "Cyan", MarkerRefresh: Duration{30 * time.Second}, MarkerStale: Duration{5 * time.Minute},
		DroneEvery: Duration{2 * time.Second}, ShowInvestigations: true, ShowCollapses: true, Triage: []string{"red", "yellow"}, ArriveM: 15, OnScene: Duration{10 * time.Minute},
		Extrication: true, TimeScale: 5, RescueTimeScale: 30, TakAckTimeout: Duration{3 * time.Minute}, TeamOffline: Duration{3 * time.Minute}, MinAge: Duration{10 * time.Second}, PublishInterval: Duration{5 * time.Second},
		StateDir: "/var/lib/rubblesar/dispatch"}
	c.Web = Web{Listen: "0.0.0.0:8090", Root: "/var/lib/rubblesar/www"}
	c.Ops = Ops{JetsonService: "edge-eye", MaxMission: Duration{90 * time.Minute}}
}

// ValidationError lists every problem found, so the user can fix them in one go.
type ValidationError struct{ Problems []string }

func (v *ValidationError) Error() string {
	return "E-CFG-003: invalid configuration:\n  - " + strings.Join(v.Problems, "\n  - ")
}

// Unwrap lets diag.CodeOf/HintOf find the code.
func (v *ValidationError) Unwrap() error { return diag.New("E-CFG-003", "invalid configuration") }

type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return &ValidationError{Problems: p}
}

func (c *Common) validate(p *problems) {
	if err := keys.ValidateID("incident", c.Site.IncidentID); err != nil {
		p.add("[site] incident_id: %v", err)
	}
	if c.Site.CenterLat < -90 || c.Site.CenterLat > 90 || math.IsNaN(c.Site.CenterLat) {
		p.add("[site] center_lat %v out of range -90..90", c.Site.CenterLat)
	}
	if c.Site.CenterLon < -180 || c.Site.CenterLon > 180 || math.IsNaN(c.Site.CenterLon) {
		p.add("[site] center_lon %v out of range -180..180", c.Site.CenterLon)
	}
	if c.Site.RadiusM <= 0 || c.Site.RadiusM > 20000 {
		p.add("[site] radius_m %v must be in (0, 20000]", c.Site.RadiusM)
	}
	switch c.Zenoh.Mode {
	case "client", "peer":
	default:
		p.add("[zenoh] mode %q must be \"client\" or \"peer\"", c.Zenoh.Mode)
	}
	if c.Zenoh.ConfigFile == "" {
		if c.Zenoh.Mode == "client" && len(c.Zenoh.Connect) == 0 {
			p.add("[zenoh] connect must list at least one endpoint in client mode (e.g. \"tcp/192.168.1.50:7447\")")
		}
		for _, ep := range append(append([]string{}, c.Zenoh.Connect...), c.Zenoh.Listen...) {
			if err := ValidateEndpoint(ep); err != nil {
				p.add("[zenoh] endpoint %q: %v", ep, err)
			}
		}
	} else if _, err := os.Stat(c.Zenoh.ConfigFile); err != nil {
		p.add("[zenoh] config_file %q: %v", c.Zenoh.ConfigFile, err)
	}
	if c.Zenoh.OpenTimeout.Duration < time.Second {
		p.add("[zenoh] open_timeout %v must be at least 1s", c.Zenoh.OpenTimeout.Duration)
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		p.add("[log] level %q must be debug, info, warn or error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		p.add("[log] format %q must be text or json", c.Log.Format)
	}
}

// ValidateEndpoint checks a zenoh endpoint such as tcp/192.168.1.50:7447.
func ValidateEndpoint(ep string) error {
	proto, addr, ok := strings.Cut(ep, "/")
	if !ok {
		return fmt.Errorf("expected <proto>/<host>:<port>")
	}
	switch proto {
	case "tcp", "tls", "udp", "quic":
	default:
		return fmt.Errorf("unsupported protocol %q (use tcp)", proto)
	}
	// Strip zenoh endpoint metadata/config suffixes (?... or #...).
	if i := strings.IndexAny(addr, "?#"); i >= 0 {
		addr = addr[:i]
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("missing host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("bad port %q", port)
	}
	return nil
}

// Validate checks an edge-eye config.
func (c *Edge) Validate() error {
	var p problems
	c.Common.validate(&p)
	if _, _, err := net.SplitHostPort(c.Admin.Listen); err != nil {
		p.add("[admin] listen %q: %v", c.Admin.Listen, err)
	}
	if len(c.UAVs) == 0 {
		p.add("at least one [[uav]] section is required")
	}
	seen := map[string]bool{}
	for i, u := range c.UAVs {
		where := fmt.Sprintf("[[uav]] #%d (%s)", i+1, u.ID)
		if err := keys.ValidateID("uav", u.ID); err != nil {
			p.add("%s id: %v", where, err)
		}
		if seen[u.ID] {
			p.add("%s: duplicate id", where)
		}
		seen[u.ID] = true
		switch u.Role {
		case "scout", "investigator":
		default:
			p.add("%s role %q must be scout or investigator", where, u.Role)
		}
		switch u.Behaviour {
		case "orbit", "hover", "landed", "survey", "standby":
		default:
			p.add("%s behaviour %q must be survey, standby, orbit, hover or landed", where, u.Behaviour)
		}
		if u.TaskLoiterS < 0 || u.TaskLoiterS > 600 {
			p.add("%s task_loiter_s %v must be in 0..600", where, u.TaskLoiterS)
		}
		if u.HoverWanderM < 0 || u.HoverWanderM > 3 {
			p.add("%s hover_wander_m %v must be in 0..3", where, u.HoverWanderM)
		}
		if u.SideOverlap < 0 || u.SideOverlap > 0.9 {
			p.add("%s side_overlap %v must be in 0..0.9", where, u.SideOverlap)
		}
		if u.SurveySpeedMps <= 0 || u.SurveySpeedMps > 25 {
			p.add("%s survey_speed_mps %v must be in (0, 25]", where, u.SurveySpeedMps)
		}
		if u.YawNoiseDeg < 0 || u.YawNoiseDeg > 10 {
			p.add("%s yaw_noise_deg %v must be in 0..10", where, u.YawNoiseDeg)
		}
		if u.FrameHz < 0 || u.FrameHz > 5 {
			p.add("%s frame_hz %v must be in 0..5 (0 = [perception] frame_hz)", where, u.FrameHz)
		}
		for _, id := range u.SurveySites {
			if err := keys.ValidateID("site", id); err != nil {
				p.add("%s survey_sites: %v", where, err)
			}
		}
		if u.AltAGLm < 0 || u.AltAGLm > 120 {
			p.add("%s alt_agl_m %v must be in 0..120 (UK/EU open-category ceiling)", where, u.AltAGLm)
		}
		if u.Behaviour != "landed" && u.AltAGLm < 5 {
			p.add("%s alt_agl_m %v must be at least 5 when flying", where, u.AltAGLm)
		}
		if u.SpeedMps <= 0 || u.SpeedMps > 25 {
			p.add("%s speed_mps %v must be in (0, 25]", where, u.SpeedMps)
		}
		if u.TelemetryHz <= 0 || u.TelemetryHz > 10 {
			p.add("%s telemetry_hz %v must be in (0, 10]", where, u.TelemetryHz)
		}
		if u.Behaviour == "orbit" && (u.OrbitRadiusM < 10 || u.OrbitRadiusM > c.Site.RadiusM) {
			p.add("%s orbit_radius_m %v must be in [10, site radius %v]", where, u.OrbitRadiusM, c.Site.RadiusM)
		}
		if math.Hypot(u.HomeOffsetEastM, u.HomeOffsetNorthM)+u.OrbitRadiusM > c.Site.RadiusM*1.5 {
			p.add("%s home offset plus orbit radius is far outside the site radius", where)
		}
		if u.EnduranceMin < 1 || u.EnduranceMin > 240 {
			p.add("%s endurance_min %v must be in [1, 240]", where, u.EnduranceMin)
		}
		if u.GPSNoiseM < 0 || u.GPSNoiseM > 50 {
			p.add("%s gps_noise_m %v must be in [0, 50]", where, u.GPSNoiseM)
		}
	}
	c.Perception.validate(&p)
	c.Outbox.validate(&p)
	return p.err()
}

func (pc Perception) validate(p *problems) {
	switch pc.Detector {
	case "model", "truth-oracle":
	default:
		p.add("[perception] detector %q must be \"model\" or \"truth-oracle\"", pc.Detector)
	}
	if pc.Conf <= 0 || pc.Conf >= 1 {
		p.add("[perception] conf %v must be between 0 and 1", pc.Conf)
	}
	if pc.FrameHz <= 0 || pc.FrameHz > 5 {
		p.add("[perception] frame_hz %v must be in (0, 5]", pc.FrameHz)
	}
	if !(pc.SkinMinC >= 15 && pc.SkinMinC < pc.SkinMaxC && pc.SkinMaxC <= 45) {
		p.add("[perception] skin band %v..%v C must lie within 15..45 C", pc.SkinMinC, pc.SkinMaxC)
	}
	if pc.WarmDeltaC < 0.5 || pc.WarmDeltaC > 20 {
		p.add("[perception] warm_delta_c %v must be in 0.5..20", pc.WarmDeltaC)
	}
	if pc.MinThermalPx < 1 || pc.MinThermalPx > 400 {
		p.add("[perception] min_thermal_px %v must be in 1..400", pc.MinThermalPx)
	}
	if pc.WarmSpotMinPx < 1 || pc.WarmSpotMinPx > 400 {
		p.add("[perception] warm_spot_min_px %v must be in 1..400", pc.WarmSpotMinPx)
	}
	if pc.ConfirmHits < 1 || pc.ConfirmHits > 10 {
		p.add("[perception] confirm_hits %v must be in 1..10", pc.ConfirmHits)
	}
	if pc.UpdateMinInterval.Duration < 0 || pc.UpdateMinInterval.Duration > 10*time.Minute {
		p.add("[perception] update_min_interval %v must be in 0..10m", pc.UpdateMinInterval.Duration)
	}
	if pc.SimStartS < 0 || pc.SimStartS > 30*86400 {
		p.add("[perception] sim_start_s %v must be in 0..30 days", pc.SimStartS)
	}
	if pc.CameraPitchDeg < -90 || pc.CameraPitchDeg > -20 {
		p.add("[perception] camera_pitch_deg %v must be in -90..-20", pc.CameraPitchDeg)
	}
	if pc.Enabled && pc.ModelDir == "" && pc.Detector == "model" {
		p.add("[perception] model_dir is required")
	}
	if pc.DetectionSigmaM < 0 || pc.DetectionSigmaM > 10 {
		p.add("[perception] detection_sigma_m %v must be in 0..10", pc.DetectionSigmaM)
	}
	if pc.DebrisHeightM < 0 || pc.DebrisHeightM > 10 {
		p.add("[perception] debris_height_m %v must be in 0..10", pc.DebrisHeightM)
	}
	if pc.MovementChange <= 0 || pc.MovementChange >= 1 {
		p.add("[perception] movement_change %v must be between 0 and 1", pc.MovementChange)
	}
	if pc.MovementTolerancePx < 0 || pc.MovementTolerancePx > 20 {
		p.add("[perception] movement_tolerance_px %v must be in 0..20", pc.MovementTolerancePx)
	}
	if pc.MovementMinFrames < 1 || pc.MovementMinFrames > 30 {
		p.add("[perception] movement_min_frames %v must be in 1..30", pc.MovementMinFrames)
	}
}

func (o Outbox) validate(p *problems) {
	if o.Dir == "" || !strings.HasPrefix(o.Dir, "/") {
		p.add("[outbox] dir %q must be an absolute path", o.Dir)
	}
	if o.MaxQueueMB < 1 || o.MaxQueueMB > 10000 {
		p.add("[outbox] max_queue_mb %v must be in 1..10000", o.MaxQueueMB)
	}
	if o.AckTimeout.Duration < time.Second || o.AckTimeout.Duration > 5*time.Minute {
		p.add("[outbox] ack_timeout %v must be in 1s..5m", o.AckTimeout.Duration)
	}
	if o.MaxBackoff.Duration < o.AckTimeout.Duration || o.MaxBackoff.Duration > 30*time.Minute {
		p.add("[outbox] max_backoff %v must be at least ack_timeout and at most 30m", o.MaxBackoff.Duration)
	}
	if o.ProbeAfter.Duration < time.Second || o.ProbeAfter.Duration > 30*time.Minute {
		p.add("[outbox] probe_after %v must be in 1s..30m", o.ProbeAfter.Duration)
	}
}

// Validate checks a ZBook tools config.
func (c *ZBook) Validate() error {
	var p problems
	c.Common.validate(&p)
	for _, id := range c.Check.ExpectUAVs {
		if err := keys.ValidateID("uav", id); err != nil {
			p.add("[check] expect_uavs: %v", err)
		}
	}
	if len(c.Check.ExpectUAVs) == 0 {
		p.add("[check] expect_uavs must list at least one drone id")
	}
	if c.Check.ExpectRateHz <= 0 || c.Check.ExpectRateHz > 10 {
		p.add("[check] expect_rate_hz must be in (0, 10]")
	}
	if c.Check.RateWindow.Duration < 5*time.Second {
		p.add("[check] rate_window must be at least 5s")
	}
	if c.Check.RateTolerancePct <= 0 || c.Check.RateTolerancePct > 50 {
		p.add("[check] rate_tolerance_pct must be in (0, 50]")
	}
	if c.Monitor.Refresh.Duration < 100*time.Millisecond {
		p.add("[monitor] refresh must be at least 100ms")
	}
	if !strings.HasPrefix(c.Analyst.StoreDir, "/") {
		p.add("[analyst] store_dir %q must be an absolute path", c.Analyst.StoreDir)
	}
	if c.Analyst.AckInterval.Duration < 20*time.Millisecond || c.Analyst.AckInterval.Duration > 5*time.Second {
		p.add("[analyst] ack_interval %v must be in 20ms..5s", c.Analyst.AckInterval.Duration)
	}
	if _, _, err := net.SplitHostPort(c.Analyst.Listen); err != nil {
		p.add("[analyst] listen %q: %v", c.Analyst.Listen, err)
	}
	c.Analyst.Fusion.validate(&p)
	c.Analyst.Triage.validate(&p)
	c.Planner.validate(&p)
	c.Dispatch.validate(&p)
	if _, _, err := net.SplitHostPort(c.Web.Listen); err != nil {
		p.add("[web] listen %q: want host:port (0.0.0.0:8090 for the network)", c.Web.Listen)
	}
	if !strings.HasPrefix(c.Web.Root, "/") {
		p.add("[web] root %q must be an absolute path", c.Web.Root)
	}
	if j := c.Ops.Jetson; j != "" && (!strings.Contains(j, "@") || strings.ContainsAny(j, " ;&|$`'\"")) {
		p.add("[ops] jetson %q: want user@address (e.g. nvidia@192.168.1.60)", j)
	}
	if c.Ops.MaxMission.Duration < time.Minute || c.Ops.MaxMission.Duration > 12*time.Hour {
		p.add("[ops] max_mission %v must be in 1m..12h", c.Ops.MaxMission.Duration)
	}
	return p.err()
}

// ReportsDir is where after-action reports go ([web] root/reports/<incident>).
func (c *ZBook) ReportsDir() string {
	return filepath.Join(c.Web.Root, "reports", c.Site.IncidentID)
}

// MapDir is where the offline map of the incident is installed ([web] root/map/<incident>).
func (c *ZBook) MapDir() string {
	return filepath.Join(c.Web.Root, "map", c.Site.IncidentID)
}

func (f Fusion) validate(p *problems) {
	if f.LinkM <= 0 || f.LinkM > 10 {
		p.add("[analyst.fusion] link_m %v must be in (0, 10]", f.LinkM)
	}
	if f.DuplicateM < 0 || f.DuplicateM > f.SpanM {
		p.add("[analyst.fusion] duplicate_m %v must be in 0..span_m", f.DuplicateM)
	}
	if f.SpanM < f.LinkM || f.SpanM > 20 {
		p.add("[analyst.fusion] span_m %v must be at least link_m and at most 20", f.SpanM)
	}
	if f.Window.Duration < time.Second || f.Window.Duration > 10*time.Minute {
		p.add("[analyst.fusion] window %v must be in 1s..10m", f.Window.Duration)
	}
	if f.MatchMinM < 0 || f.MatchMinM > 50 {
		p.add("[analyst.fusion] match_min_m %v must be in 0..50", f.MatchMinM)
	}
	if f.MatchCEPs <= 0 || f.MatchCEPs > 10 {
		p.add("[analyst.fusion] match_ceps %v must be in (0, 10]", f.MatchCEPs)
	}
	if f.PublishInterval.Duration < time.Second {
		p.add("[analyst.fusion] publish_interval %v must be at least 1s", f.PublishInterval.Duration)
	}
}

func (t Triage) validate(p *problems) {
	if !(t.SkinMinC >= 15 && t.SkinMinC < t.SkinMaxC && t.SkinMaxC <= 45) {
		p.add("[analyst.triage] skin band %v..%v C must lie within 15..45 C", t.SkinMinC, t.SkinMaxC)
	}
	if t.ClothedMinC < 5 || t.ClothedMinC > t.SkinMinC {
		p.add("[analyst.triage] clothed_min_c %v must be in 5..skin_min_c (%v)", t.ClothedMinC, t.SkinMinC)
	}
	if t.ClothedDeltaC < t.WarmDeltaC || t.ClothedDeltaC > 30 {
		p.add("[analyst.triage] clothed_delta_c %v must be in warm_delta_c (%v)..30", t.ClothedDeltaC, t.WarmDeltaC)
	}
	if t.WarmDeltaC < 0.5 || t.WarmDeltaC > 20 {
		p.add("[analyst.triage] warm_delta_c %v must be in 0.5..20", t.WarmDeltaC)
	}
	if t.CloseRangeM < 5 || t.CloseRangeM > 100 {
		p.add("[analyst.triage] close_range_m %v must be in 5..100", t.CloseRangeM)
	}
	if t.MinWatchS < 1 || t.MinWatchS > 600 {
		p.add("[analyst.triage] min_watch_s %v must be in 1..600", t.MinWatchS)
	}
	if t.MovedFrames < 1 || t.MovedFrames > 30 {
		p.add("[analyst.triage] moved_frames %v must be in 1..30", t.MovedFrames)
	}
	if t.CoolingCPerH < 0 || t.CoolingCPerH > 20 {
		p.add("[analyst.triage] cooling_c_per_h %v must be in 0..20", t.CoolingCPerH)
	}
	if t.MaxCEPm < 1 || t.MaxCEPm > 100 {
		p.add("[analyst.triage] max_cep_m %v must be in 1..100", t.MaxCEPm)
	}
	if t.NotFoundLooks < 1 || t.NotFoundLooks > 10 {
		p.add("[analyst.triage] not_found_looks %v must be in 1..10", t.NotFoundLooks)
	}
}

func (pl Planner) validate(p *problems) {
	if _, _, err := net.SplitHostPort(pl.Listen); err != nil {
		p.add("[planner] listen %q: %v", pl.Listen, err)
	}
	if pl.AltAGLm < 5 || pl.AltAGLm > 120 {
		p.add("[planner] alt_agl_m %v must be in 5..120", pl.AltAGLm)
	}
	if pl.HoverS < 5 || pl.HoverS > 600 {
		p.add("[planner] hover_s %v must be in 5..600", pl.HoverS)
	}
	if pl.RadiusM < 1 || pl.RadiusM > 50 {
		p.add("[planner] radius_m %v must be in 1..50", pl.RadiusM)
	}
	if pl.ReservePct < 0 || pl.ReservePct > 90 {
		p.add("[planner] reserve_pct %v must be in 0..90", pl.ReservePct)
	}
	if pl.MaxAttempts < 1 || pl.MaxAttempts > 10 {
		p.add("[planner] max_attempts %v must be in 1..10", pl.MaxAttempts)
	}
	if pl.TaskTimeout.Duration < 30*time.Second {
		p.add("[planner] task_timeout %v must be at least 30s", pl.TaskTimeout.Duration)
	}
	if pl.CommandTimeout.Duration < 500*time.Millisecond || pl.CommandTimeout.Duration > time.Minute {
		p.add("[planner] command_timeout %v must be in 500ms..1m", pl.CommandTimeout.Duration)
	}
	for _, id := range pl.Investigators {
		if err := keys.ValidateID("uav", id); err != nil {
			p.add("[planner] investigators: %v", err)
		}
	}
	if pl.PublishInterval.Duration < time.Second {
		p.add("[planner] publish_interval %v must be at least 1s", pl.PublishInterval.Duration)
	}
	if pl.StateDir == "" {
		p.add("[planner] state_dir must be set")
	}
}

func (d Dispatch) validate(p *problems) {
	if _, _, err := net.SplitHostPort(d.Listen); err != nil {
		p.add("[dispatch] listen %q: %v", d.Listen, err)
	}
	if d.TakServer != "" {
		if _, port, err := net.SplitHostPort(d.TakServer); err != nil || port == "" {
			p.add("[dispatch] tak_server %q must be host:port (taky: \"127.0.0.1:8087\"), or \"\" for no TAK map", d.TakServer)
		}
	}
	if strings.TrimSpace(d.Callsign) == "" {
		p.add("[dispatch] callsign must be set (e.g. \"SAR-OPS\")")
	}
	if d.BlockMarginM < 0 || d.BlockMarginM > 20 {
		p.add("[dispatch] block_margin_m %v must be in 0..20", d.BlockMarginM)
	}
	if d.MarkerRefresh.Duration < 5*time.Second || d.MarkerRefresh.Duration > 10*time.Minute {
		p.add("[dispatch] marker_refresh %v must be in 5s..10m", d.MarkerRefresh.Duration)
	}
	if d.MarkerStale.Duration < 2*d.MarkerRefresh.Duration {
		p.add("[dispatch] marker_stale %v must be at least twice marker_refresh (%v)", d.MarkerStale.Duration, d.MarkerRefresh.Duration)
	}
	if d.DroneEvery.Duration < 500*time.Millisecond || d.DroneEvery.Duration > time.Minute {
		p.add("[dispatch] drone_every %v must be in 500ms..1m", d.DroneEvery.Duration)
	}
	if len(d.Triage) == 0 {
		p.add("[dispatch] triage must list at least one colour (\"red\", \"yellow\", \"green\")")
	}
	for _, t := range d.Triage {
		switch strings.ToLower(t) {
		case "red", "yellow", "green":
		default:
			p.add("[dispatch] triage %q: must be red, yellow or green (black and grey casualties get no team)", t)
		}
	}
	if d.ArriveM < 3 || d.ArriveM > 100 {
		p.add("[dispatch] arrive_m %v must be in 3..100", d.ArriveM)
	}
	if d.OnScene.Duration < 0 || d.OnScene.Duration > 4*time.Hour {
		p.add("[dispatch] on_scene %v must be in 0..4h", d.OnScene.Duration)
	}
	if d.TimeScale < 0.1 || d.TimeScale > 100 {
		p.add("[dispatch] time_scale %v must be in 0.1..100", d.TimeScale)
	}
	if d.RescueTimeScale < 1 || d.RescueTimeScale > 500 {
		p.add("[dispatch] rescue_time_scale %v must be in 1..500", d.RescueTimeScale)
	}
	if d.TakAckTimeout.Duration < 0 || d.TakAckTimeout.Duration > 30*time.Minute {
		p.add("[dispatch] tak_ack_timeout %v must be in 0..30m", d.TakAckTimeout.Duration)
	}
	if d.TeamOffline.Duration < 10*time.Second {
		p.add("[dispatch] team_offline %v must be at least 10s", d.TeamOffline.Duration)
	}
	if d.StagingLat < -90 || d.StagingLat > 90 || d.StagingLon < -180 || d.StagingLon > 180 {
		p.add("[dispatch] staging_lat/staging_lon %v, %v out of range", d.StagingLat, d.StagingLon)
	}
	if d.MinAge.Duration < 0 || d.MinAge.Duration > 10*time.Minute {
		p.add("[dispatch] min_age %v must be in 0..10m", d.MinAge.Duration)
	}
	if d.PublishInterval.Duration < time.Second {
		p.add("[dispatch] publish_interval %v must be at least 1s", d.PublishInterval.Duration)
	}
	if d.StateDir == "" {
		p.add("[dispatch] state_dir must be set")
	}
	ids, calls := map[string]bool{}, map[string]bool{}
	for i, t := range d.Teams {
		where := fmt.Sprintf("[[dispatch.team]] #%d", i+1)
		if err := keys.ValidateID("team", t.ID); err != nil {
			p.add("%s id: %v", where, err)
		}
		if ids[t.ID] {
			p.add("%s id %q is used twice", where, t.ID)
		}
		ids[t.ID] = true
		cs := strings.ToUpper(strings.TrimSpace(t.Callsign))
		if cs == "" {
			p.add("%s (%s) needs a callsign", where, t.ID)
		}
		if calls[cs] || cs == strings.ToUpper(d.Callsign) {
			p.add("%s callsign %q is used twice (or is rescue-dispatch's own)", where, t.Callsign)
		}
		calls[cs] = true
		if t.Mode != "foot" && t.Mode != "vehicle" {
			p.add("%s (%s) mode %q must be \"foot\" or \"vehicle\"", where, t.ID, t.Mode)
		}
		if t.Source != "sim" && t.Source != "tak" {
			p.add("%s (%s) source %q must be \"sim\" (simulated) or \"tak\" (a TAK user with that callsign)", where, t.ID, t.Source)
		}
	}
}

// RoadsPath is the street file ([dispatch] roads, or the incident's file under /var/lib/rubblesar/roads).
func (c *ZBook) RoadsPath() string {
	if c.Dispatch.Roads != "" {
		return c.Dispatch.Roads
	}
	return "/var/lib/rubblesar/roads/" + c.Site.IncidentID + ".json"
}

// Staging is where teams start (the incident centre when not set).
func (c *ZBook) Staging() (lat, lon float64) {
	if c.Dispatch.StagingLat == 0 && c.Dispatch.StagingLon == 0 {
		return c.Site.CenterLat, c.Site.CenterLon
	}
	return c.Dispatch.StagingLat, c.Dispatch.StagingLon
}

// AnalystDir is the detection store of this incident.
func (c *ZBook) AnalystDir() string { return c.Analyst.StoreDir + "/" + c.Site.IncidentID }

// Target describes where the session connects, for messages and reports.
func (z Zenoh) Target() string {
	if z.ConfigFile != "" {
		return "zenoh config " + z.ConfigFile
	}
	if z.Mode == "peer" && len(z.Connect) == 0 {
		return "peer listening on " + strings.Join(z.Listen, ",")
	}
	return strings.Join(z.Connect, ",")
}

// ZenohJSON5 renders the zenoh session configuration for zc_config_from_str.
// The result is plain JSON, which is valid JSON5.
func (z Zenoh) ZenohJSON5() (string, error) {
	if z.ConfigFile != "" {
		b, err := os.ReadFile(z.ConfigFile)
		if err != nil {
			return "", diag.Wrap("E-CFG-001", err, "zenoh config_file")
		}
		return string(b), nil
	}
	cfg := map[string]any{
		"mode": z.Mode,
		"connect": map[string]any{
			"endpoints": nonNil(z.Connect),
		},
		"scouting": map[string]any{
			"multicast": map[string]any{"enabled": false},
		},
		// Shared memory only helps processes on one machine; RubbleSAR traffic
		// crosses Wi-Fi, and some systems restrict /dev/shm. Keep it off.
		"transport": map[string]any{
			"shared_memory": map[string]any{"enabled": false},
		},
	}
	if z.Mode == "peer" {
		cfg["listen"] = map[string]any{"endpoints": nonNil(z.Listen)}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
