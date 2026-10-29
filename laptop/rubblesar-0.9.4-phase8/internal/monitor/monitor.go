// Package monitor tracks telemetry arriving on the ZBook: per-drone rate,
// sequence gaps, decode errors and latency. sar-monitor displays it and
// sar-check judges it.
package monitor

import (
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// UAVStats is the tracked state of one drone.
type UAVStats struct {
	ID         string
	Count      uint64 // messages accepted since the last Reset
	Bytes      uint64
	FirstSeen  time.Time
	LastSeen   time.Time
	LastSeq    uint64
	Gaps       uint64 // missing sequence numbers
	OutOfOrder uint64 // older or duplicate sequence numbers
	Restarts   uint64 // sequence went back to 1 (edge-eye restarted)
	Latest     *sarpb.Telemetry
	LatencyMs  []float64 // recv - sent, most recent first-in order (bounded)
	arrivals   []time.Time
}

// RateHz returns messages per second over the last window.
func (u *UAVStats) RateHz(now time.Time, window time.Duration) float64 {
	cut := now.Add(-window)
	n := 0
	for _, t := range u.arrivals {
		if t.After(cut) {
			n++
		}
	}
	return float64(n) / window.Seconds()
}

// MedianLatencyMs returns the median of recorded latencies (NaN-free: 0 if none).
func (u *UAVStats) MedianLatencyMs() float64 {
	if len(u.LatencyMs) == 0 {
		return 0
	}
	s := append([]float64(nil), u.LatencyMs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// Tracker accumulates telemetry. Safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	incident  string
	center    geo.Point
	uavs      map[string]*UAVStats
	decodeErr uint64
	mismatch  uint64 // incident or key/payload mismatches
	lastErr   error
	since     time.Time
}

const (
	maxLatency  = 600
	maxArrivals = 3600
)

// NewTracker creates a tracker for one incident.
func NewTracker(incident string, center geo.Point) *Tracker {
	return &Tracker{incident: incident, center: center, uavs: map[string]*UAVStats{}, since: time.Now()}
}

// Handle is a bus.Handler for telemetry samples.
func (t *Tracker) Handle(s bus.Sample) {
	var m sarpb.Telemetry
	err := msg.Unmarshal(s.Payload, &m)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.decodeErr++
		t.lastErr = err
		return
	}
	if err := msg.CheckHeader(m.GetHeader(), t.incident); err != nil {
		t.mismatch++
		t.lastErr = err
		return
	}
	k, ok := keys.ParseUAV(s.Key)
	if !ok || k.UAV != m.GetUavId() || k.Incident != t.incident {
		t.mismatch++
		t.lastErr = diag.New("E-MSG-004", "key %q carries telemetry for %q", s.Key, m.GetUavId())
		return
	}
	u := t.uavs[m.GetUavId()]
	if u == nil {
		u = &UAVStats{ID: m.GetUavId(), FirstSeen: s.Received}
		t.uavs[u.ID] = u
	}
	seq := m.GetHeader().GetSeq()
	switch {
	case u.LastSeq == 0:
	case seq == u.LastSeq+1:
	case seq == 1 && u.LastSeq > 1:
		u.Restarts++
	case seq > u.LastSeq+1:
		u.Gaps += seq - u.LastSeq - 1
	default:
		u.OutOfOrder++
	}
	if seq > u.LastSeq || seq == 1 {
		u.LastSeq = seq
	}
	u.Count++
	u.Bytes += uint64(len(s.Payload))
	u.LastSeen = s.Received
	u.Latest = &m
	lat := float64(s.Received.Sub(msg.SentTime(m.GetHeader()))) / float64(time.Millisecond)
	u.LatencyMs = appendBounded(u.LatencyMs, lat, maxLatency)
	u.arrivals = appendBoundedT(u.arrivals, s.Received, maxArrivals)
}

func appendBounded(s []float64, v float64, max int) []float64 {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

func appendBoundedT(s []time.Time, v time.Time, max int) []time.Time {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// Reset clears counters (but keeps the latest message and sequence position).
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range t.uavs {
		u.Count, u.Bytes, u.Gaps, u.OutOfOrder, u.Restarts = 0, 0, 0, 0, 0
		u.LatencyMs = nil
		u.arrivals = nil
		u.FirstSeen = time.Time{}
	}
	t.decodeErr, t.mismatch, t.lastErr = 0, 0, nil
	t.since = time.Now()
}

// Snapshot is a consistent copy of everything tracked.
type Snapshot struct {
	Taken     time.Time
	Since     time.Time
	UAVs      []UAVStats // sorted by id
	DecodeErr uint64
	Mismatch  uint64
	LastErr   error
}

// Snapshot returns a deep-enough copy for display and judgement.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Snapshot{Taken: time.Now(), Since: t.since, DecodeErr: t.decodeErr, Mismatch: t.mismatch, LastErr: t.lastErr}
	for _, u := range t.uavs {
		c := *u
		c.LatencyMs = append([]float64(nil), u.LatencyMs...)
		c.arrivals = append([]time.Time(nil), u.arrivals...)
		s.UAVs = append(s.UAVs, c)
	}
	sort.Slice(s.UAVs, func(i, j int) bool { return s.UAVs[i].ID < s.UAVs[j].ID })
	return s
}

// Get returns one drone's stats.
func (s Snapshot) Get(id string) (UAVStats, bool) {
	for _, u := range s.UAVs {
		if u.ID == id {
			return u, true
		}
	}
	return UAVStats{}, false
}

// DistanceFromCenter returns how far a drone's latest fix is from the incident centre.
func (t *Tracker) DistanceFromCenter(u UAVStats) float64 {
	if u.Latest == nil || u.Latest.Position == nil {
		return -1
	}
	return geo.Distance(t.center, geo.Point{Lat: u.Latest.Position.LatDeg, Lon: u.Latest.Position.LonDeg})
}

// DetectionEntry is one received detection report.
type DetectionEntry struct {
	At      time.Time
	UAV     string
	ID      string
	Part    string
	Fused   float32
	Payload string
	Summary string
}

// DetectionLog keeps per-drone counts and the latest detection reports.
// Safe for concurrent use.
type DetectionLog struct {
	mu       sync.Mutex
	incident string
	counts   map[string]*DetCounts
	latest   []DetectionEntry
	keep     int
}

// DetCounts counts one drone's reports.
type DetCounts struct {
	Packets, New, Updates, Bytes uint64
	Reduced                      uint64 // sent with reduced payload
}

// NewDetectionLog keeps the last keep reports.
func NewDetectionLog(incident string, keep int) *DetectionLog {
	return &DetectionLog{incident: incident, counts: map[string]*DetCounts{}, keep: keep}
}

// Handle is a bus.Handler for detection samples.
func (l *DetectionLog) Handle(s bus.Sample) {
	var d sarpb.Detection
	if msg.Unmarshal(s.Payload, &d) != nil || msg.CheckHeader(d.GetHeader(), l.incident) != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.counts[d.UavId]
	if c == nil {
		c = &DetCounts{}
		l.counts[d.UavId] = c
	}
	c.Packets++
	c.Bytes += uint64(len(s.Payload))
	if d.Reason == sarpb.ReportReason_REPORT_REASON_UPDATE {
		c.Updates++
	} else {
		c.New++
	}
	if d.Payload != sarpb.Payload_PAYLOAD_FULL {
		c.Reduced++
	}
	part := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(d.Part.String(), "PART_CLASS_")), "_", " ")
	pay := strings.ToLower(strings.TrimPrefix(d.Payload.String(), "PAYLOAD_"))
	l.latest = append(l.latest, DetectionEntry{At: s.Received, UAV: d.UavId, ID: d.DetectionId, Part: part, Fused: d.FusedConfidence, Payload: pay, Summary: d.Summary})
	if len(l.latest) > l.keep {
		l.latest = l.latest[len(l.latest)-l.keep:]
	}
}

// Snapshot returns copies of the counts and the latest reports (oldest first).
func (l *DetectionLog) Snapshot() (map[string]DetCounts, []DetectionEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := map[string]DetCounts{}
	for k, v := range l.counts {
		c[k] = *v
	}
	return c, append([]DetectionEntry(nil), l.latest...)
}

// Board keeps the latest casualty list (casualty-analyst) and task list
// (mission-planner), phase 4. Safe for concurrent use.
type Board struct {
	mu       sync.Mutex
	incident string
	cas      *sarpb.CasualtyList
	casAt    time.Time
	tasks    *sarpb.TaskList
	tasksAt  time.Time
	disp     *sarpb.DispatchState
	dispAt   time.Time
}

// NewBoard makes an empty board for one incident.
func NewBoard(incident string) *Board { return &Board{incident: incident} }

// HandleCasualties is a bus.Handler for the casualty list.
func (b *Board) HandleCasualties(s bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(s.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), b.incident) != nil {
		return
	}
	b.mu.Lock()
	b.cas, b.casAt = &l, s.Received
	b.mu.Unlock()
}

// HandleTasks is a bus.Handler for the task list.
func (b *Board) HandleTasks(s bus.Sample) {
	var l sarpb.TaskList
	if msg.Unmarshal(s.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), b.incident) != nil {
		return
	}
	b.mu.Lock()
	b.tasks, b.tasksAt = &l, s.Received
	b.mu.Unlock()
}

// Snapshot returns the latest lists (nil if none yet) and when they arrived.
func (b *Board) Snapshot() (*sarpb.CasualtyList, time.Time, *sarpb.TaskList, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cas, b.casAt, b.tasks, b.tasksAt
}

// HandleDispatch is a bus.Handler for rescue-dispatch's state (phase 5).
func (b *Board) HandleDispatch(s bus.Sample) {
	var d sarpb.DispatchState
	if msg.Unmarshal(s.Payload, &d) != nil || msg.CheckHeader(d.GetHeader(), b.incident) != nil {
		return
	}
	b.mu.Lock()
	b.disp, b.dispAt = &d, s.Received
	b.mu.Unlock()
}

// Dispatch returns the latest dispatch state (nil if none yet) and when it arrived.
func (b *Board) Dispatch() (*sarpb.DispatchState, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.disp, b.dispAt
}
