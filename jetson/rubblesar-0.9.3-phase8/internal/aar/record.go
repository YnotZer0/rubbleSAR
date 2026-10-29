// Package aar records a run (phase 6) and turns the recording into an
// after-action report: what happened when, on a map and a timeline, and how
// it compares with the scenario.
//
// The Recorder listens on the bus while a run goes on (casualty lists and
// merges, investigation tasks, thinned drone telemetry, rescue assignments
// and team positions) and polls rescue-dispatch for its chat, writing one
// JSON event per line to events.jsonl in the run's folder. Build reads the
// folder back; the report can be rebuilt at any time from it.
//
// Phase 8 records enough to replay a run in the operator view: each
// casualty's detail, every detection report with its crops (crops/), the
// truth of the frames they came from (to render them again for the demo
// video), the drones' link and pipeline figures, and the director's beats.
package aar

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Event is one line of events.jsonl.
type Event struct {
	T     int64           `json:"t"` // unix milliseconds
	K     string          `json:"k"` // cas, casd, merge, task, tel, asg, teams, chat, det, frame, beat, start, end, note
	Cas   []CasSnap       `json:"cas,omitempty"`
	Merge []string        `json:"merge,omitempty"` // from, into
	Task  *TaskSnap       `json:"task,omitempty"`
	Tel   *TelSnap        `json:"tel,omitempty"`
	Asg   json.RawMessage `json:"asg,omitempty"` // sar.v1.Assignment (protojson)
	Teams []TeamSnap      `json:"teams,omitempty"`
	Chat  *ChatLine       `json:"chat,omitempty"`
	Note  string          `json:"note,omitempty"`
	// Phase 8 (for replays and the demo video).
	CasD  *CasDetail `json:"casd,omitempty"`
	Det   *DetSnap   `json:"det,omitempty"`
	Frame *FrameSnap `json:"frame,omitempty"`
	// Beat: a moment the demo director marks ("first-red", "outage-start"), Ref what it is about.
	Beat string `json:"beat,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// Waiting: casualties waiting for a team (with "teams").
	Waiting []string `json:"waiting,omitempty"`
}

// CasSnap is one casualty in a list snapshot.
type CasSnap struct {
	ID        string   `json:"id"`
	Triage    string   `json:"tri"`
	Prov      bool     `json:"prov,omitempty"`
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	CEP       float64  `json:"cep"`
	UAVs      []string `json:"uavs,omitempty"`
	First     int64    `json:"first,omitempty"` // unix ms, first seen
	SeenClose bool     `json:"close,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	// Phase 8.
	Status string `json:"st,omitempty"`
}

// CasDetail is what a casualty record holds besides its list entry (phase 8),
// recorded whenever it changes: temperature, movement, looks, the parts.
type CasDetail struct {
	ID        string     `json:"id"`
	Conf      float64    `json:"conf,omitempty"`
	PAlive    float64    `json:"pa,omitempty"`
	Thermal   *ThermSnap `json:"th,omitempty"`
	Moved     bool       `json:"mv,omitempty"`
	Watched   float64    `json:"w,omitempty"`
	Looks     uint32     `json:"looks,omitempty"`
	NeedsLook bool       `json:"nl,omitempty"`
	LookWhy   string     `json:"lw,omitempty"`
	Best      string     `json:"best,omitempty"`
	Last      int64      `json:"last,omitempty"`
	Parts     []PartSnap `json:"parts,omitempty"`
}

// ThermSnap is a temperature reading.
type ThermSnap struct {
	Readable bool    `json:"r"`
	Warm     bool    `json:"w,omitempty"`
	Skin     float64 `json:"s,omitempty"`
	Ambient  float64 `json:"a,omitempty"`
	Delta    float64 `json:"d,omitempty"`
	Readings uint32  `json:"n,omitempty"`
	Close    uint32  `json:"c,omitempty"`
	Trend    float64 `json:"tr,omitempty"`
}

// PartSnap is one drone's body-part track in a casualty record.
type PartSnap struct {
	UAV   string  `json:"u"`
	Track string  `json:"tk"`
	Det   string  `json:"d"`
	Class string  `json:"c"`
	Conf  float64 `json:"p"`
	Obs   uint32  `json:"o,omitempty"`
}

// DetSnap is a detection report as it arrived (phase 8); its crops are kept in crops/<id>.jpg and .png.
type DetSnap struct {
	ID      string     `json:"id"`
	UAV     string     `json:"uav"`
	Track   string     `json:"track"`
	Rev     uint32     `json:"rev"`
	Reason  string     `json:"reason"`
	Part    string     `json:"part"`
	Conf    float64    `json:"conf"`
	Fused   float64    `json:"fused"`
	Lat     float64    `json:"lat"`
	Lon     float64    `json:"lon"`
	CEP     float64    `json:"cep"`
	Thermal *ThermSnap `json:"th,omitempty"`
	Payload string     `json:"payload"`
	Bytes   int        `json:"bytes"`
	Crop    bool       `json:"crop,omitempty"`
	Therm   bool       `json:"therm,omitempty"`
	Capture int64      `json:"capture"`
	Frame   string     `json:"frame"`
	SimS    float64    `json:"sim_s,omitempty"`
	RangeM  float64    `json:"range_m,omitempty"`
	Obs     uint32     `json:"obs,omitempty"`
	Summary string     `json:"summary,omitempty"`
	Outline []float64  `json:"outline,omitempty"`
	Box     []float64  `json:"box,omitempty"`    // the visible crop's region of the frame (x0, y0, x1, y1)
	ThBox   []float64  `json:"th_box,omitempty"` // the thermal crop's region of the thermal frame
	Pose    *PoseSnap  `json:"pose,omitempty"`   // as the drone reported it
	CropW   uint32     `json:"crop_w,omitempty"` // the colour crop as sent
	CropH   uint32     `json:"crop_h,omitempty"`
}

// PoseSnap is a camera pose.
type PoseSnap struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Alt   float64 `json:"alt"` // above ground
	Yaw   float64 `json:"yaw"`
	Pitch float64 `json:"pitch"`
	Roll  float64 `json:"roll,omitempty"`
}

// FrameSnap is the simulation's truth of a camera frame some detection came
// from (phase 8): with the true pose and the scenario clock, the frame can be
// rendered again exactly as the drone saw it.
type FrameSnap struct {
	ID       string    `json:"id"`
	UAV      string    `json:"uav"`
	Capture  int64     `json:"capture"`
	SimS     float64   `json:"sim_s"`
	True     *PoseSnap `json:"true"`
	Reported *PoseSnap `json:"reported,omitempty"`
	// Visible: the scenario victims in view (id:part).
	Visible []string `json:"visible,omitempty"`
}

// TaskSnap is an investigation task when its state changes.
type TaskSnap struct {
	ID     string  `json:"id"`
	Cas    string  `json:"cas"`
	UAV    string  `json:"uav,omitempty"`
	State  string  `json:"state"`
	Tier   uint32  `json:"tier"`
	Reason string  `json:"reason,omitempty"`
	Found  bool    `json:"found,omitempty"`
	Moved  bool    `json:"moved,omitempty"`
	Lat    float64 `json:"lat,omitempty"`
	Lon    float64 `json:"lon,omitempty"`
}

// TelSnap is a drone's telemetry, thinned.
type TelSnap struct {
	UAV      string  `json:"uav"`
	Role     string  `json:"role"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	AltAGL   float64 `json:"alt"`
	Battery  float64 `json:"bat"`
	State    string  `json:"state"`
	Task     string  `json:"task,omitempty"`
	Step     uint32  `json:"step,omitempty"`
	Steps    uint32  `json:"steps,omitempty"`
	Done     bool    `json:"done,omitempty"`
	Reports  uint64  `json:"reports,omitempty"`
	Software string  `json:"sw,omitempty"`
	// Phase 8 (replays).
	Yaw     float64   `json:"yaw,omitempty"`
	Speed   float64   `json:"spd,omitempty"`
	FlightS uint32    `json:"fl,omitempty"`
	SimS    float64   `json:"sim,omitempty"`
	TaskSt  string    `json:"tst,omitempty"`
	TaskCas string    `json:"tcas,omitempty"`
	TaskLat float64   `json:"tlat,omitempty"`
	TaskLon float64   `json:"tlon,omitempty"`
	HoverS  uint32    `json:"hov,omitempty"`
	Elapsed float64   `json:"el,omitempty"`
	Link    *LinkSnap `json:"link,omitempty"`
	Perc    *PercSnap `json:"perc,omitempty"`
	// 0.9.3: the drone's home, and a survey drone's sites in the order flown.
	HomeLat float64  `json:"hlat,omitempty"`
	HomeLon float64  `json:"hlon,omitempty"`
	Survey  []string `json:"sv,omitempty"`
}

// LinkSnap is a drone's radio link as it reports it (phase 8).
type LinkSnap struct {
	Budget    uint32 `json:"b"`
	Used      uint32 `json:"u"`
	Queued    uint32 `json:"q,omitempty"`
	QBytes    uint64 `json:"qb,omitempty"`
	Unacked   uint32 `json:"un,omitempty"`
	Degrade   uint32 `json:"dg,omitempty"`
	Probing   bool   `json:"pr,omitempty"`
	Acked     uint64 `json:"ak,omitempty"`
	Resent    uint64 `json:"rs,omitempty"`
	Discarded uint64 `json:"dc,omitempty"`
	Dropped   uint64 `json:"dr,omitempty"`
	Sent      uint64 `json:"sb,omitempty"`
}

// PercSnap is the onboard pipeline (phase 8).
type PercSnap struct {
	Backend string  `json:"be,omitempty"`
	Model   string  `json:"m,omitempty"`
	Hz      float64 `json:"hz,omitempty"`
	Frames  uint64  `json:"fr,omitempty"`
	Seg     float64 `json:"seg,omitempty"`
	Therm   float64 `json:"th,omitempty"`
	Track   float64 `json:"tk,omitempty"`
	Pack    float64 `json:"pk,omitempty"`
	Render  float64 `json:"rn,omitempty"`
	Tracks  uint32  `json:"trk,omitempty"`
	Warm    uint64  `json:"warm,omitempty"`
}

// TeamSnap is a rescue team's position and state.
type TeamSnap struct {
	ID       string  `json:"id"`
	Callsign string  `json:"cs"`
	Mode     string  `json:"mode"`
	Source   string  `json:"src"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	State    string  `json:"state"`
	// Phase 8.
	Casualty string  `json:"cas,omitempty"`
	Course   float64 `json:"crs,omitempty"`
	Done     uint32  `json:"done,omitempty"`
}

// ChatLine is a chat message (rescue-dispatch's chat with the teams).
type ChatLine struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Text string    `json:"text"`
}

// Meta describes the run (meta.json).
type Meta struct {
	Incident string    `json:"incident"`
	Run      string    `json:"run"`
	Suite    string    `json:"suite"` // "mission" (rubblesar mission) or "phase6" (acceptance test)
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitempty"`
	Host     string    `json:"host"`
	Software []string  `json:"software"`
	// Simulated teams move TimeScale and rescue RescueTimeScale times faster than real time.
	TimeScale       float64  `json:"time_scale"`
	RescueTimeScale float64  `json:"rescue_time_scale"`
	Teams           []string `json:"teams"`
	Scenario        string   `json:"scenario"`
	// Staging is the command post (lat, lon).
	Staging [2]float64 `json:"staging"`
	Notes   []string   `json:"notes,omitempty"`
}

// Options configure a recorder.
type Options struct {
	Bus      bus.Bus
	Incident string
	Dir      string
	Meta     Meta
	// Chats returns rescue-dispatch's chat lines (nil: chat not recorded).
	Chats func() ([]ChatLine, error)
	// TelEvery and TeamsEvery thin telemetry and team positions (defaults 2 s and 5 s).
	TelEvery, TeamsEvery time.Duration
	// MaxCropMB: the detections' crops kept in crops/ (default 80 MB; then metadata only).
	MaxCropMB int
}

// Recorder writes a run's events.
type Recorder struct {
	o       Options
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	subs    []bus.Subscription
	stop    chan struct{}
	done    chan struct{}
	lastCas string
	tasks   map[string]string // task id -> state recorded
	asgs    map[string]string // assignment id -> state|casualty|route length recorded
	telAt   map[string]time.Time
	teamAt  time.Time
	chats   map[string]bool
	n       int
	closed  bool
	// Phase 8.
	casd      map[string]string // casualty id -> detail recorded
	dets      map[string]bool
	cropBytes int64
	truths    map[string]*sarpb.SimFrameTruth // recent frames' truth, by frame id
	truthQ    []string
	frames    map[string]bool // frames referenced by a detection (pending until their truth arrives)
	framesOut map[string]bool
}

// Start creates the folder, writes meta.json and starts recording.
func Start(o Options) (*Recorder, error) {
	if o.TelEvery <= 0 {
		o.TelEvery = 2 * time.Second
	}
	if o.TeamsEvery <= 0 {
		o.TeamsEvery = 5 * time.Second
	}
	if o.MaxCropMB <= 0 {
		o.MaxCropMB = 80
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("E-AAR-001: creating %s: %w", o.Dir, err)
	}
	if o.Meta.Started.IsZero() {
		o.Meta.Started = time.Now()
	}
	o.Meta.Incident = o.Incident
	if err := writeJSON(filepath.Join(o.Dir, "meta.json"), o.Meta); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(o.Dir, "events.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("E-AAR-001: %w", err)
	}
	r := &Recorder{o: o, f: f, w: bufio.NewWriterSize(f, 64<<10), stop: make(chan struct{}), done: make(chan struct{}),
		tasks: map[string]string{}, asgs: map[string]string{}, telAt: map[string]time.Time{}, chats: map[string]bool{},
		casd: map[string]string{}, dets: map[string]bool{}, truths: map[string]*sarpb.SimFrameTruth{}, frames: map[string]bool{}, framesOut: map[string]bool{}}
	r.write(Event{K: "start", Note: o.Meta.Suite})
	inc := o.Incident
	for _, x := range []struct {
		key string
		h   bus.Handler
	}{{keys.Casualties(inc), r.onList}, {keys.CasualtyAll(inc), r.onCasualty}, {keys.Tasks(inc), r.onTasks},
		{keys.TelemetryAll(inc), r.onTel}, {keys.Dispatch(inc), r.onDispatch}, {keys.DetectionAll(inc), r.onDetection}, {keys.SimTruthAll(inc), r.onTruth}} {
		sub, err := o.Bus.Subscribe(x.key, x.h)
		if err != nil {
			for _, s := range r.subs {
				_ = s.Close()
			}
			_ = f.Close()
			return nil, fmt.Errorf("E-AAR-001: subscribing to %s: %w", x.key, err)
		}
		r.subs = append(r.subs, sub)
	}
	go r.loop()
	return r, nil
}

// Dir is the run's folder.
func (r *Recorder) Dir() string { return r.o.Dir }

// Events is how many events have been written.
func (r *Recorder) Events() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Note records a line of text (e.g. "survey finished").
func (r *Recorder) Note(text string) { r.write(Event{K: "note", Note: text}) }

// Beat marks a moment of the run (the demo director's beats: "first-red", "outage-start"), about ref.
func (r *Recorder) Beat(name, ref, text string) {
	r.write(Event{K: "beat", Beat: name, Ref: ref, Note: text})
}

func (r *Recorder) loop() {
	defer close(r.done)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			r.pollChats()
			return
		case <-t.C:
			r.pollChats()
			r.mu.Lock()
			_ = r.w.Flush()
			r.mu.Unlock()
		}
	}
}

func (r *Recorder) pollChats() {
	if r.o.Chats == nil {
		return
	}
	lines, err := r.o.Chats()
	if err != nil {
		return
	}
	for _, c := range lines {
		k := c.At.UTC().Format(time.RFC3339Nano) + "|" + c.From + "|" + c.Text
		r.mu.Lock()
		seen := r.chats[k]
		r.chats[k] = true
		r.mu.Unlock()
		if !seen {
			cc := c
			r.write(Event{T: c.At.UnixMilli(), K: "chat", Chat: &cc})
		}
	}
}

// Close stops recording, writes the end event and updates meta.json.
func (r *Recorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	for _, s := range r.subs {
		_ = s.Close()
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
	r.write(Event{K: "end"})
	r.mu.Lock()
	r.closed = true
	err := r.w.Flush()
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	r.mu.Unlock()
	r.o.Meta.Ended = time.Now()
	if werr := writeJSON(filepath.Join(r.o.Dir, "meta.json"), r.o.Meta); err == nil {
		err = werr
	}
	return err
}

func (r *Recorder) write(e Event) {
	if e.T == 0 {
		e.T = time.Now().UnixMilli()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	_, _ = r.w.Write(b)
	_ = r.w.WriteByte('\n')
	r.n++
}

func (r *Recorder) onList(x bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(x.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), r.o.Incident) != nil {
		return
	}
	snap := make([]CasSnap, 0, len(l.Casualties))
	for _, c := range l.Casualties {
		snap = append(snap, CasSnap{ID: c.CasualtyId, Triage: casualty.TriageName(c.Triage), Prov: c.Provisional, Lat: round7(c.GetPosition().GetLatDeg()),
			Lon: round7(c.GetPosition().GetLonDeg()), CEP: round1(float64(c.CepM)), UAVs: c.UavIds, First: c.FirstSeenUnixNs / 1e6, SeenClose: c.SeenClose,
			Reason: c.Reason, Summary: c.Summary, Status: strings.ToLower(strings.TrimPrefix(c.Status.String(), "CASUALTY_STATUS_"))})
	}
	sort.Slice(snap, func(i, j int) bool { return snap[i].ID < snap[j].ID })
	b, _ := json.Marshal(snap)
	r.mu.Lock()
	same := string(b) == r.lastCas
	r.lastCas = string(b)
	r.mu.Unlock()
	if !same {
		r.write(Event{K: "cas", Cas: snap})
	}
}

func (r *Recorder) onCasualty(x bus.Sample) {
	var c sarpb.Casualty
	if msg.Unmarshal(x.Payload, &c) != nil || msg.CheckHeader(c.GetHeader(), r.o.Incident) != nil {
		return
	}
	if c.MergedInto != "" {
		r.write(Event{K: "merge", Merge: []string{c.CasualtyId, c.MergedInto}})
		return
	}
	d := &CasDetail{ID: c.CasualtyId, Conf: round3(float64(c.Confidence)), PAlive: round3(float64(c.PAlive)), Looks: c.Investigations,
		NeedsLook: c.NeedsInvestigation, LookWhy: c.InvestigationReason, Best: c.BestDetectionId}
	if t := c.Thermal; t != nil {
		d.Thermal = &ThermSnap{Readable: t.Readable, Warm: t.Warm, Skin: round1(float64(t.SkinC)), Ambient: round1(float64(t.AmbientC)), Delta: round1(float64(t.DeltaC)),
			Readings: t.Readings, Close: t.CloseReadings, Trend: round2(float64(t.TrendCPerH))}
	}
	if m := c.Movement; m != nil {
		d.Moved, d.Watched = m.Moved, round1(float64(m.WatchedS))
	}
	for i, p := range c.Parts {
		if i == 8 {
			break
		}
		d.Parts = append(d.Parts, PartSnap{UAV: p.UavId, Track: p.TrackId, Det: p.DetectionId, Class: partName(p.Part), Conf: round3(float64(p.Confidence)), Obs: p.Observations})
	}
	b, _ := json.Marshal(d)
	d.Last = c.LastSeenUnixNs / 1e6
	r.mu.Lock()
	same := r.casd[c.CasualtyId] == string(b)
	r.casd[c.CasualtyId] = string(b)
	r.mu.Unlock()
	if !same {
		r.write(Event{K: "casd", CasD: d})
	}
}

func partName(p sarpb.PartClass) string {
	if p == sarpb.PartClass_PART_CLASS_WARM_SPOT {
		return "warm spot"
	}
	return strings.ToLower(strings.TrimPrefix(p.String(), "PART_CLASS_"))
}

var payloadName = map[sarpb.Payload]string{sarpb.Payload_PAYLOAD_FULL: "full", sarpb.Payload_PAYLOAD_SMALL: "small",
	sarpb.Payload_PAYLOAD_THERMAL_ONLY: "thermal", sarpb.Payload_PAYLOAD_METADATA: "metadata"}

func poseSnap(p *sarpb.CapturePose) *PoseSnap {
	if p == nil || p.Position == nil {
		return nil
	}
	return &PoseSnap{Lat: round7(p.Position.LatDeg), Lon: round7(p.Position.LonDeg), Alt: round2(float64(p.AltAglM)), Yaw: round2(float64(p.YawDeg)),
		Pitch: round2(float64(p.PitchDeg)), Roll: round2(float64(p.RollDeg))}
}

func floats(v []float32) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = round3(float64(x))
	}
	return out
}

// onDetection records each report once (repeats keep their id) and keeps its crops.
func (r *Recorder) onDetection(x bus.Sample) {
	var d sarpb.Detection
	if msg.Unmarshal(x.Payload, &d) != nil || msg.CheckHeader(d.GetHeader(), r.o.Incident) != nil || d.DetectionId == "" {
		return
	}
	r.mu.Lock()
	seen := r.dets[d.DetectionId]
	r.dets[d.DetectionId] = true
	r.mu.Unlock()
	if seen {
		return
	}
	s := &DetSnap{ID: d.DetectionId, UAV: d.UavId, Track: d.TrackId, Rev: d.Revision, Reason: strings.ToLower(strings.TrimPrefix(d.Reason.String(), "REPORT_REASON_")),
		Part: partName(d.Part), Conf: round3(float64(d.Confidence)), Fused: round3(float64(d.FusedConfidence)), Lat: round7(d.GetPosition().GetLatDeg()),
		Lon: round7(d.GetPosition().GetLonDeg()), CEP: round1(float64(d.CepM)), Payload: payloadName[d.Payload], Bytes: len(x.Payload),
		Capture: d.CaptureUnixNs / 1e6, Frame: d.FrameId, SimS: round2(d.SimTimeS), RangeM: round1(float64(d.RangeM)), Obs: d.Observations, Summary: d.Summary,
		Outline: floats(d.Outline), Pose: poseSnap(d.CapturePose)}
	if t := d.Thermal; t != nil {
		s.Thermal = &ThermSnap{Readable: t.Readable, Warm: t.Warm, Skin: round1(float64(t.SkinC)), Ambient: round1(float64(t.AmbientC)), Delta: round1(float64(t.DeltaC)), Readings: t.Pixels}
	}
	if c := d.VisibleCrop; c != nil && len(c.Data) > 0 {
		s.Box, s.CropW, s.CropH = floats(c.SourceBox), c.Width, c.Height
		s.Crop = r.keepCrop(d.DetectionId+".jpg", c.Data)
	}
	if c := d.ThermalCrop; c != nil && len(c.Data) > 0 {
		s.ThBox = floats(c.SourceBox)
		s.Therm = r.keepCrop(d.DetectionId+".png", c.Data)
	}
	r.write(Event{K: "det", Det: s})
	if d.FrameId != "" {
		r.mu.Lock()
		r.frames[d.FrameId] = true
		t := r.truths[d.FrameId]
		r.mu.Unlock()
		if t != nil {
			r.writeFrame(t)
		}
	}
}

func (r *Recorder) keepCrop(name string, b []byte) bool {
	if !keysafe(name) {
		return false
	}
	r.mu.Lock()
	full := r.cropBytes+int64(len(b)) > int64(r.o.MaxCropMB)<<20
	if !full {
		r.cropBytes += int64(len(b))
	}
	r.mu.Unlock()
	if full {
		return false
	}
	dir := filepath.Join(r.o.Dir, "crops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644) == nil
}

func keysafe(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return false
	}
	return true
}

// onTruth keeps the latest frames' truth; a frame some detection came from is recorded.
func (r *Recorder) onTruth(x bus.Sample) {
	var t sarpb.SimFrameTruth
	if msg.Unmarshal(x.Payload, &t) != nil || msg.CheckHeader(t.GetHeader(), r.o.Incident) != nil || t.FrameId == "" {
		return
	}
	r.mu.Lock()
	r.truths[t.FrameId] = &t
	r.truthQ = append(r.truthQ, t.FrameId)
	if len(r.truthQ) > 900 {
		delete(r.truths, r.truthQ[0])
		r.truthQ = r.truthQ[1:]
	}
	want := r.frames[t.FrameId]
	r.mu.Unlock()
	if want {
		r.writeFrame(&t)
	}
}

func (r *Recorder) writeFrame(t *sarpb.SimFrameTruth) {
	r.mu.Lock()
	done := r.framesOut[t.FrameId]
	r.framesOut[t.FrameId] = true
	r.mu.Unlock()
	if done {
		return
	}
	f := &FrameSnap{ID: t.FrameId, UAV: t.UavId, Capture: t.CaptureUnixNs / 1e6, SimS: t.SimTimeS, True: poseSnap(t.TruePose), Reported: poseSnap(t.ReportedPose)}
	for _, v := range t.Visible {
		f.Visible = append(f.Visible, v.VictimId+":"+partName(v.Part))
	}
	r.write(Event{K: "frame", Frame: f})
}

func (r *Recorder) onTasks(x bus.Sample) {
	var l sarpb.TaskList
	if msg.Unmarshal(x.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), r.o.Incident) != nil {
		return
	}
	for _, t := range l.Tasks {
		st := strings.ToLower(strings.TrimPrefix(t.State.String(), "TASK_STATE_"))
		r.mu.Lock()
		same := r.tasks[t.TaskId] == st+"|"+t.UavId
		r.tasks[t.TaskId] = st + "|" + t.UavId
		r.mu.Unlock()
		if same {
			continue
		}
		ts := &TaskSnap{ID: t.TaskId, Cas: t.CasualtyId, UAV: t.UavId, State: st, Tier: t.Tier, Reason: t.Reason,
			Found: t.GetResult().GetFound(), Moved: t.GetResult().GetMoved(), Lat: t.GetTarget().GetLatDeg(), Lon: t.GetTarget().GetLonDeg()}
		r.write(Event{K: "task", Task: ts})
	}
}

func (r *Recorder) onTel(x bus.Sample) {
	var t sarpb.Telemetry
	if msg.Unmarshal(x.Payload, &t) != nil || msg.CheckHeader(t.GetHeader(), r.o.Incident) != nil || t.UavId == "" || t.Position == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	due := now.Sub(r.telAt[t.UavId]) >= r.o.TelEvery
	if due {
		r.telAt[t.UavId] = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	p := t.GetPerception()
	ts := &TelSnap{UAV: t.UavId, Role: strings.ToLower(strings.TrimPrefix(t.Role.String(), "UAV_ROLE_")),
		Lat: round7(t.Position.LatDeg), Lon: round7(t.Position.LonDeg), AltAGL: round1(float64(t.AltAglM)), Battery: round1(float64(t.BatteryPct)),
		State: strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_")), Task: t.GetTask().GetTaskId(),
		Step: p.GetMissionStep(), Steps: p.GetMissionSteps(), Done: p.GetMissionDone(), Reports: p.GetReports(), Software: t.Software,
		Yaw: round1(float64(t.YawDeg)), Speed: round1(float64(t.GroundSpeedMps)), FlightS: t.FlightTimeLeftS, SimS: round1(p.GetSimTimeS()),
		HomeLat: round7(t.GetHome().GetLatDeg()), HomeLon: round7(t.GetHome().GetLonDeg()), Survey: t.SurveySites}
	if k := t.GetTask(); k.GetTaskId() != "" {
		ts.TaskSt, ts.TaskCas = strings.ToLower(strings.TrimPrefix(k.State.String(), "TASK_STATE_")), k.CasualtyId
		ts.TaskLat, ts.TaskLon, ts.HoverS, ts.Elapsed = round7(k.GetTarget().GetLatDeg()), round7(k.GetTarget().GetLonDeg()), k.HoverS, round1(float64(k.HoverElapsedS))
	}
	if l := t.Link; l != nil {
		ts.Link = &LinkSnap{Budget: l.BudgetBps, Used: l.UsedBps, Queued: l.QueuedMsgs, QBytes: l.QueuedBytes, Unacked: l.UnackedMsgs, Degrade: l.DegradeLevel,
			Probing: l.Probing, Acked: l.AckedMsgs, Resent: l.ResentMsgs, Discarded: l.DiscardedMsgs, Dropped: l.DroppedMsgs, Sent: l.SentBytes}
	}
	if p != nil && p.Enabled {
		ts.Perc = &PercSnap{Backend: p.Backend, Model: p.Model, Hz: round2(float64(p.FrameHzActual)), Frames: p.Frames, Seg: round1(float64(p.SegmentMs)),
			Therm: round1(float64(p.ThermalMs)), Track: round1(float64(p.TrackMs)), Pack: round1(float64(p.PackageMs)), Render: round1(float64(p.RenderMs)),
			Tracks: p.Tracks, Warm: p.WarmSpotReports}
	}
	r.write(Event{K: "tel", Tel: ts})
}

func (r *Recorder) onDispatch(x bus.Sample) {
	var st sarpb.DispatchState
	if msg.Unmarshal(x.Payload, &st) != nil || msg.CheckHeader(st.GetHeader(), r.o.Incident) != nil {
		return
	}
	for _, a := range st.Assignments {
		k := fmt.Sprintf("%v|%s|%.0f|%d", a.State, a.CasualtyId, a.GetRoute().GetLengthM(), a.FreeUnixNs)
		r.mu.Lock()
		same := r.asgs[a.AssignmentId] == k
		r.asgs[a.AssignmentId] = k
		r.mu.Unlock()
		if same {
			continue
		}
		b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(a)
		if err == nil {
			r.write(Event{K: "asg", Asg: b})
		}
	}
	now := time.Now()
	r.mu.Lock()
	due := now.Sub(r.teamAt) >= r.o.TeamsEvery
	if due {
		r.teamAt = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	var teams []TeamSnap
	for _, t := range st.Teams {
		teams = append(teams, TeamSnap{ID: t.TeamId, Callsign: t.Callsign, Mode: t.Mode, Source: t.Source, Lat: round7(t.GetPosition().GetLatDeg()),
			Lon: round7(t.GetPosition().GetLonDeg()), State: strings.ToLower(strings.TrimPrefix(t.State.String(), "TEAM_STATE_")),
			Casualty: t.CasualtyId, Course: round1(float64(t.CourseDeg)), Done: t.AssignmentsDone})
	}
	r.write(Event{K: "teams", Teams: teams, Waiting: st.Waiting})
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("E-AAR-001: writing %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

func round7(v float64) float64 { return float64(int64(v*1e7+copysign(0.5, v))) / 1e7 }
func round1(v float64) float64 { return float64(int64(v*10+copysign(0.5, v))) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+copysign(0.5, v))) / 100 }
func round3(v float64) float64 { return float64(int64(v*1000+copysign(0.5, v))) / 1000 }

func copysign(a, b float64) float64 {
	if b < 0 {
		return -a
	}
	return a
}
