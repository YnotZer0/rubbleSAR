// Package live is the operator view's picture of a run (phase 7): the drones,
// casualties, close looks, rescue teams, assignments, detections, chat and a
// timeline of what happened, kept up to date from the bus while a mission
// runs (Live) or rebuilt from a recorded run (Player, phase 8). sar-web
// serves it to browsers as JSON and as a stream of server-sent events.
//
// The Hub is the one place where messages become the picture and changes
// become timeline entries, so a live run and its replay read the same.
package live

// Picture is what the operator view shows at one moment.
type Picture struct {
	Incident string `json:"incident"`
	// Mode: "live" (from the bus), "replay" (a recorded run) or "off".
	Mode string `json:"mode"`
	// Bus: the zenoh connection in words ("connected", "waiting for zenohd at tcp/127.0.0.1:7447").
	Bus string `json:"bus"`
	// Now: the picture's time (unix ms); in a replay, the recorded time.
	Now int64 `json:"now"`
	// Start: when the run started (unix ms), 0 when not known (live, mid-run).
	Start int64  `json:"start,omitempty"`
	Seq   uint64 `json:"seq"`
	// Run: the run's id (a replay's folder name, or rescue-dispatch's run token).
	Run string `json:"run,omitempty"`

	Drones      []*Drone      `json:"drones"`
	Casualties  []*Casualty   `json:"casualties"`
	Tasks       []*Task       `json:"tasks"`
	Teams       []*Team       `json:"teams"`
	Assignments []*Assignment `json:"assignments"`
	Waiting     []string      `json:"waiting,omitempty"`
	// Detections: the latest reports received, newest first.
	Detections []*Det  `json:"detections"`
	Timeline   []*Item `json:"timeline"`
	Chats      []*Chat `json:"chats"`
	KPI        KPI     `json:"kpi"`
	TAK        *TAK    `json:"tak,omitempty"`
	Replay     *Replay `json:"replay,omitempty"`
}

// Drone is one drone's latest telemetry.
type Drone struct {
	ID       string  `json:"id"`
	Role     string  `json:"role"`
	State    string  `json:"state"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Alt      float64 `json:"alt"` // above ground, m
	Yaw      float64 `json:"yaw"`
	Course   float64 `json:"course,omitempty"`
	Speed    float64 `json:"speed"`
	Battery  float64 `json:"battery"`
	FlightS  uint32  `json:"flight_left_s,omitempty"`
	GPSm     float64 `json:"gps_m,omitempty"`
	HomeLat  float64 `json:"home_lat,omitempty"`
	HomeLon  float64 `json:"home_lon,omitempty"`
	Seen     int64   `json:"seen"` // unix ms of the last telemetry
	Stale    bool    `json:"stale,omitempty"`
	Step     uint32  `json:"step,omitempty"`
	Steps    uint32  `json:"steps,omitempty"`
	Surveyed bool    `json:"survey_done,omitempty"`
	Software string  `json:"software,omitempty"`
	// Survey: a survey drone's collapse sites in the order it flies them (edge-eye 0.9.3 on).
	Survey []string `json:"survey,omitempty"`
	// SimS: the scenario clock on the drone, seconds.
	SimS float64    `json:"sim_s,omitempty"`
	Task *DroneTask `json:"task,omitempty"`
	Link *Link      `json:"link,omitempty"`
	Perc *Perc      `json:"perc,omitempty"`
	// Stored: this drone's reports received at the ZBook (distinct ids seen here); Bytes their size.
	Stored uint64 `json:"stored"`
	Bytes  uint64 `json:"bytes"`
	// Trail: the last positions (lat, lon), oldest first.
	Trail [][2]float64 `json:"trail,omitempty"`
	// LinkHist: the radio over the last minutes, oldest first: unix ms, bits/s used,
	// budget (bits/s), reports waiting (queued + unacknowledged), payload level.
	LinkHist [][5]float64 `json:"link_hist,omitempty"`
}

// DroneTask is the investigation a drone is flying.
type DroneTask struct {
	ID       string  `json:"id"`
	Casualty string  `json:"casualty"`
	State    string  `json:"state"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	HoverS   uint32  `json:"hover_s,omitempty"`
	Elapsed  float64 `json:"elapsed_s,omitempty"`
	Moved    bool    `json:"moved,omitempty"`
}

// Link is a drone's radio as the drone sees it.
type Link struct {
	BudgetBps   uint32 `json:"budget_bps"`
	UsedBps     uint32 `json:"used_bps"`
	QueuedMsgs  uint32 `json:"queued"`
	QueuedBytes uint64 `json:"queued_bytes"`
	Unacked     uint32 `json:"unacked"`
	// Degrade: 0 full reports, 1 small crops, 2 thermal crops only, 3 metadata only.
	Degrade   uint32 `json:"degrade"`
	Outage    bool   `json:"outage,omitempty"`
	Probing   bool   `json:"probing,omitempty"`
	Acked     uint64 `json:"acked"`
	Resent    uint64 `json:"resent"`
	Discarded uint64 `json:"discarded"`
	Dropped   uint64 `json:"dropped"`
	SentBytes uint64 `json:"sent_bytes"`
}

// Perc is the onboard pipeline (on the Jetson).
type Perc struct {
	Backend  string  `json:"backend,omitempty"`
	Model    string  `json:"model,omitempty"`
	HzTarget float64 `json:"hz_target,omitempty"`
	Hz       float64 `json:"hz"`
	Frames   uint64  `json:"frames"`
	SegMS    float64 `json:"seg_ms"`
	ThermMS  float64 `json:"therm_ms"`
	TrackMS  float64 `json:"track_ms"`
	PackMS   float64 `json:"pack_ms"`
	RenderMS float64 `json:"render_ms,omitempty"`
	Tracks   uint32  `json:"tracks"`
	Reports  uint64  `json:"reports"`
	Warm     uint64  `json:"warm_spots,omitempty"`
}

// Casualty is one casualty record.
type Casualty struct {
	ID      string   `json:"id"`
	Triage  string   `json:"triage"` // Red, Yellow, Green, Black, Grey
	Prov    bool     `json:"prov,omitempty"`
	Status  string   `json:"status,omitempty"`
	Lat     float64  `json:"lat"`
	Lon     float64  `json:"lon"`
	CEP     float64  `json:"cep"`
	UAVs    []string `json:"uavs,omitempty"`
	First   int64    `json:"first,omitempty"`
	Last    int64    `json:"last,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Conf    float64  `json:"conf,omitempty"`
	PAlive  float64  `json:"p_alive,omitempty"`
	Thermal *Thermal `json:"thermal,omitempty"`
	Moved   bool     `json:"moved,omitempty"`
	Watched float64  `json:"watched_s,omitempty"`
	Close   bool     `json:"close,omitempty"` // seen close up
	Looks   uint32   `json:"looks,omitempty"`
	// NeedsLook: casualty-analyst wants a close look (LookWhy).
	NeedsLook bool    `json:"needs_look,omitempty"`
	LookWhy   string  `json:"look_why,omitempty"`
	Best      string  `json:"best,omitempty"` // the detection whose crops show it best
	Parts     []*Part `json:"parts,omitempty"`
	// From rescue-dispatch: the team on it and how far it is.
	Team     string  `json:"team,omitempty"`
	AsgState string  `json:"asg_state,omitempty"`
	ETAs     float64 `json:"eta_s,omitempty"`
	Rescued  bool    `json:"rescued,omitempty"`
	NeedTeam bool    `json:"need_team,omitempty"`
}

// Thermal is a casualty's temperature reading.
type Thermal struct {
	Readable bool    `json:"readable"`
	Warm     bool    `json:"warm,omitempty"`
	SkinC    float64 `json:"skin_c,omitempty"`
	AmbientC float64 `json:"ambient_c,omitempty"`
	DeltaC   float64 `json:"delta_c,omitempty"`
	Readings uint32  `json:"readings,omitempty"`
	Close    uint32  `json:"close_readings,omitempty"`
	TrendCH  float64 `json:"trend_c_per_h,omitempty"`
}

// Part is one drone's body-part track merged into a casualty.
type Part struct {
	UAV   string  `json:"uav"`
	Track string  `json:"track"`
	Det   string  `json:"det"`
	Class string  `json:"class"`
	Conf  float64 `json:"conf"`
	Obs   uint32  `json:"obs,omitempty"`
}

// Task is a close look (investigation) by an investigator drone.
type Task struct {
	ID       string  `json:"id"`
	Casualty string  `json:"casualty"`
	State    string  `json:"state"`
	Tier     uint32  `json:"tier,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	UAV      string  `json:"uav,omitempty"`
	Lat      float64 `json:"lat,omitempty"`
	Lon      float64 `json:"lon,omitempty"`
	Found    bool    `json:"found,omitempty"`
	Moved    bool    `json:"moved,omitempty"`
	Note     string  `json:"note,omitempty"`
	Created  int64   `json:"created,omitempty"`
	Finished int64   `json:"finished,omitempty"`
}

// Team is a rescue team.
type Team struct {
	ID       string  `json:"id"`
	Callsign string  `json:"callsign"`
	Mode     string  `json:"mode"`
	Source   string  `json:"source"` // sim or tak
	State    string  `json:"state"`  // idle, en_route, on_scene, offline
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Course   float64 `json:"course,omitempty"`
	Casualty string  `json:"casualty,omitempty"`
	Done     uint32  `json:"done,omitempty"`
	Seen     int64   `json:"seen,omitempty"`
}

// Assignment is a rescue team sent to a casualty.
type Assignment struct {
	ID       string  `json:"id"`
	Casualty string  `json:"casualty"`
	Team     string  `json:"team"`
	State    string  `json:"state"` // en_route, on_scene, done, cancelled
	Triage   string  `json:"triage"`
	Prov     bool    `json:"prov,omitempty"`
	Priority uint32  `json:"priority,omitempty"`
	Created  int64   `json:"created,omitempty"`
	OnScene  int64   `json:"on_scene,omitempty"`
	Done     int64   `json:"done,omitempty"`
	Needed   int64   `json:"needed,omitempty"`
	LengthM  float64 `json:"length_m,omitempty"`
	RubbleM  float64 `json:"rubble_m,omitempty"`
	ETAs     float64 `json:"eta_s,omitempty"`
	// Extrication: why freeing takes as long as it does; ExtricationS in real seconds.
	Extrication  string   `json:"extrication,omitempty"`
	ExtricationS float64  `json:"extrication_s,omitempty"`
	Streets      []string `json:"streets,omitempty"`
	Note         string   `json:"note,omitempty"`
	// Route: (lat, lon) points, only while the team is on the way or on scene.
	Route  [][2]float64 `json:"route,omitempty"`
	Access int          `json:"access,omitempty"` // Route[Access] is where the streets end
}

// Det is one detection report received from a drone (metadata; the crops are served separately).
type Det struct {
	ID       string   `json:"id"`
	UAV      string   `json:"uav"`
	Track    string   `json:"track"`
	Rev      uint32   `json:"rev"`
	Reason   string   `json:"reason"`
	Part     string   `json:"part"`
	Conf     float64  `json:"conf"`
	Fused    float64  `json:"fused"`
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	CEP      float64  `json:"cep"`
	Thermal  *Thermal `json:"thermal,omitempty"`
	Payload  string   `json:"payload"` // full, small, thermal, metadata
	Bytes    int      `json:"bytes"`
	Crop     bool     `json:"crop,omitempty"`
	Therm    bool     `json:"therm,omitempty"`
	At       int64    `json:"at"`      // received (unix ms)
	Capture  int64    `json:"capture"` // the frame's capture time
	Frame    string   `json:"frame,omitempty"`
	RangeM   float64  `json:"range_m,omitempty"`
	Obs      uint32   `json:"obs,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Casualty string   `json:"casualty,omitempty"`
	// Outline: the part's outline in the visible crop, 0..1 (x0, y0, x1, y1, ...).
	Outline []float64 `json:"outline,omitempty"`
}

// Item is one line of the timeline.
type Item struct {
	T    int64  `json:"t"` // unix ms
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Ref: the casualty (or drone, or team) it is about; Tone: red, yellow, green, black, grey, blue, info, warn.
	Ref  string `json:"ref,omitempty"`
	Tone string `json:"tone,omitempty"`
}

// Chat is a chat line between SAR-OPS and the teams.
type Chat struct {
	At   int64  `json:"at"`
	From string `json:"from"`
	To   string `json:"to"`
	Text string `json:"text"`
}

// KPI are the headline numbers.
type KPI struct {
	Records     int            `json:"records"`
	ByTriage    map[string]int `json:"by_triage"`
	Provisional int            `json:"provisional"`
	NeedTeam    int            `json:"need_team"`
	Waiting     int            `json:"waiting"`
	Rescued     int            `json:"rescued"`
	OnTheWay    int            `json:"teams_en_route"`
	OnScene     int            `json:"teams_on_scene"`
	TeamsFree   int            `json:"teams_free"`
	TeamsOff    int            `json:"teams_offline"`
	LooksOpen   int            `json:"looks_open"`
	LooksDone   int            `json:"looks_done"`
	Detections  uint64         `json:"detections"`
	Bytes       uint64         `json:"bytes"`
	Airborne    int            `json:"airborne"`
	FirstSeen   int64          `json:"first_seen,omitempty"`
	FirstFreed  int64          `json:"first_freed,omitempty"`
	// Lost: reports a drone had to discard (its queue full): 0 is the point of store-and-forward.
	Lost uint64 `json:"lost"`
}

// TAK is the TAK server link (rescue-dispatch's).
type TAK struct {
	Server    string   `json:"server,omitempty"`
	Connected bool     `json:"connected"`
	Users     []string `json:"users,omitempty"`
	Sent      uint64   `json:"sent,omitempty"`
	Received  uint64   `json:"received,omitempty"`
}

// Replay is where a replay stands.
type Replay struct {
	Run      string  `json:"run"`
	Title    string  `json:"title,omitempty"`
	Start    int64   `json:"start"`
	End      int64   `json:"end"`
	At       int64   `json:"at"` // the replay's position (unix ms)
	Speed    float64 `json:"speed"`
	Playing  bool    `json:"playing"`
	Finished bool    `json:"finished,omitempty"`
}
