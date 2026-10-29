// Package outbox is a drone's store-and-forward queue for detection reports.
//
// Every report is written to disk before it is first sent and removed only
// when the ZBook acknowledges it, so a radio outage or a restart loses
// nothing. Reports leave in priority order through the drone's link budget
// (shared with telemetry, which keeps a small reserve). When the queue falls
// behind the budget, reports are sent with smaller crops, then thermal crops
// only, then metadata only. Unacknowledged reports are re-sent with growing
// back-off; when nothing has been acknowledged for a while the link is
// assumed down and only an occasional probe is sent until an ack returns.
package outbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/diag"
	"rubblesar/internal/sarpb"
)

// Priorities, highest first.
const (
	PrioWarmNew  = iota // new, warm and fairly confident: most likely a living person
	PrioNew             // other new body parts
	PrioUpdate          // changes to reports already sent
	PrioWarmSpot        // thermal-only candidates
)

// Priority of a report.
func Priority(d *sarpb.Detection) int {
	switch {
	case d.GetPart() == sarpb.PartClass_PART_CLASS_WARM_SPOT:
		return PrioWarmSpot
	case d.GetReason() == sarpb.ReportReason_REPORT_REASON_UPDATE:
		return PrioUpdate
	case d.GetThermal().GetWarm() && d.GetFusedConfidence() >= 0.5:
		return PrioWarmNew
	}
	return PrioNew
}

// Backlog thresholds (seconds of budget) for the payload levels 1, 2 and 3.
var DegradeAfter = [3]float64{10, 30, 60}

// Config configures one drone's outbox.
type Config struct {
	Dir        string // this drone's folder
	MaxBytes   int64
	AckTimeout time.Duration
	MaxBackoff time.Duration
	ProbeAfter time.Duration
	// ProbeEvery is the interval between probes while the link seems down.
	ProbeEvery time.Duration
	// Encode returns the wire form of a report at a payload level (0 = full).
	Encode func(d *sarpb.Detection, level int) ([]byte, error)
	// Reserve keeps this many bytes of allowance for telemetry.
	Reserve int
}

// Sender publishes one message (bus.Publisher).
type Sender interface{ Put([]byte) error }

// Budget meters the radio (linkbudget.Bucket).
type Budget interface {
	TryReserve(n, reserve int) bool
	Bps() uint32
}

type item struct {
	id       string
	prio     int
	created  time.Time
	det      *sarpb.Detection
	full     []byte // wire form at full payload (also the file content)
	attempts int
	lastSent time.Time
	nextSend time.Time
	encLevel int    // cached reduced encoding (level > 0)
	enc      []byte //
}

// Stats is a snapshot for telemetry and the admin API.
type Stats struct {
	Queued       int
	QueuedBytes  int64
	Unacked      int
	Sent         uint64 // first transmissions
	Resent       uint64
	Acked        uint64
	Discarded    uint64 // dropped because the queue was full
	Level        int
	Probing      bool
	Outage       bool
	OldestS      float64 // age of the oldest queued report
	SendErrors   uint64
	LastAckAgeS  float64 // -1 if never
	BytesOnWire  uint64
	LoadedOnOpen int
}

// Outbox is safe for concurrent use.
type Outbox struct {
	mu          sync.Mutex
	cfg         Config
	now         func() time.Time
	items       map[string]*item
	bytes       int64
	st          Stats
	lastAck     time.Time
	firstUnack  time.Time // first send not followed by any ack
	lastProbe   time.Time
	outageUntil time.Time
}

// Open creates the folder if needed and loads reports left from a previous run.
func Open(cfg Config, now func() time.Time) (*Outbox, error) {
	if now == nil {
		now = time.Now
	}
	if cfg.ProbeEvery == 0 {
		cfg.ProbeEvery = 5 * time.Second
	}
	if cfg.Encode == nil {
		cfg.Encode = func(d *sarpb.Detection, _ int) ([]byte, error) { return proto.Marshal(d) }
	}
	o := &Outbox{cfg: cfg, now: now, items: map[string]*item{}}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, diag.Wrap("E-EDGE-006", err, "creating the detection queue %s", cfg.Dir)
	}
	ents, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, diag.Wrap("E-EDGE-006", err, "reading the detection queue %s", cfg.Dir)
	}
	t := now()
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(cfg.Dir, name)
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(p) // interrupted write
			continue
		}
		if !strings.HasSuffix(name, ".pb") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, diag.Wrap("E-EDGE-006", err, "reading %s", p)
		}
		var d sarpb.Detection
		if err := proto.Unmarshal(b, &d); err != nil || d.GetDetectionId() == "" {
			_ = os.Rename(p, p+".bad") // keep for inspection, do not block the queue
			continue
		}
		created := time.Unix(0, d.GetHeader().GetSentUnixNs())
		o.items[d.GetDetectionId()] = &item{id: d.GetDetectionId(), prio: Priority(&d), created: created, det: &d, full: b, nextSend: t}
		o.bytes += int64(len(b))
		o.st.LoadedOnOpen++
	}
	return o, nil
}

func fileName(id string) string { return id + ".pb" }

// Add queues a report (its header must be set) and writes it to disk first.
func (o *Outbox) Add(d *sarpb.Detection) error {
	full, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	id := d.GetDetectionId()
	if id == "" || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("bad detection id %q", id)
	}
	p := filepath.Join(o.cfg.Dir, fileName(id))
	if err := writeAtomic(p, full); err != nil {
		return diag.Wrap("E-EDGE-006", err, "writing %s", p)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.now()
	if old, ok := o.items[id]; ok {
		o.bytes -= int64(len(old.full))
	}
	o.items[id] = &item{id: id, prio: Priority(d), created: t, det: d, full: full, nextSend: t}
	o.bytes += int64(len(full))
	o.trimLocked()
	return nil
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// trimLocked discards the lowest-priority, oldest reports beyond MaxBytes.
func (o *Outbox) trimLocked() {
	for o.bytes > o.cfg.MaxBytes && len(o.items) > 0 {
		var worst *item
		for _, it := range o.items {
			if worst == nil || it.prio > worst.prio || (it.prio == worst.prio && it.created.Before(worst.created)) {
				worst = it
			}
		}
		o.removeLocked(worst)
		o.st.Discarded++
	}
}

func (o *Outbox) removeLocked(it *item) {
	delete(o.items, it.id)
	o.bytes -= int64(len(it.full))
	if err := os.Remove(filepath.Join(o.cfg.Dir, fileName(it.id))); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.st.SendErrors++
	}
}

// Ack removes acknowledged reports. Acks arriving during a simulated outage
// are ignored (in a real outage they would not arrive either).
func (o *Outbox) Ack(ids []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.now()
	if t.Before(o.outageUntil) {
		return
	}
	o.lastAck = t
	o.firstUnack = time.Time{}
	for _, id := range ids {
		if it, ok := o.items[id]; ok {
			o.removeLocked(it)
			o.st.Acked++
		}
	}
}

// SetOutage simulates a radio outage for d (0 ends it).
func (o *Outbox) SetOutage(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outageUntil = o.now().Add(d)
}

// InOutage reports whether a simulated outage is in progress.
func (o *Outbox) InOutage() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.now().Before(o.outageUntil)
}

// Clear discards every queued report.
func (o *Outbox) Clear() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		o.removeLocked(it)
	}
	o.firstUnack = time.Time{}
}

// levelLocked picks the payload level from the backlog in seconds of budget.
func (o *Outbox) levelLocked(bps uint32, t time.Time) int {
	if bps == 0 {
		return 0
	}
	var backlog int64
	for _, it := range o.items {
		if it.attempts == 0 || !t.Before(it.nextSend) {
			backlog += int64(len(it.full))
		}
	}
	secs := float64(backlog) / (float64(bps) / 8)
	lvl := 0
	for i, th := range DegradeAfter {
		if secs >= th {
			lvl = i + 1
		}
	}
	return lvl
}

// Pump sends what the budget allows now. Call it every 50-100 ms.
func (o *Outbox) Pump(s Sender, b Budget) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.now()
	o.st.Outage = t.Before(o.outageUntil)
	if len(o.items) == 0 {
		o.st.Level, o.st.Probing = 0, false
		return 0
	}
	if o.st.Outage {
		return 0
	}
	o.st.Level = o.levelLocked(b.Bps(), t)
	// Link presumed down: something was sent and nothing acknowledged for ProbeAfter.
	o.st.Probing = !o.firstUnack.IsZero() && t.Sub(o.firstUnack) >= o.cfg.ProbeAfter
	var due []*item
	for _, it := range o.items {
		if o.st.Probing || !t.Before(it.nextSend) {
			due = append(due, it)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].prio != due[j].prio {
			return due[i].prio < due[j].prio
		}
		return due[i].created.Before(due[j].created)
	})
	if o.st.Probing {
		if t.Sub(o.lastProbe) < o.cfg.ProbeEvery || len(due) == 0 {
			return 0
		}
		due = due[:1]
		o.lastProbe = t
	}
	n := 0
	for _, it := range due {
		if n >= 20 {
			break
		}
		wire := it.full
		if o.st.Level > 0 {
			if it.enc == nil || it.encLevel != o.st.Level {
				e, err := o.cfg.Encode(it.det, o.st.Level)
				if err != nil {
					o.st.SendErrors++
					continue
				}
				it.enc, it.encLevel = e, o.st.Level
			}
			wire = it.enc
		}
		if !b.TryReserve(len(wire), o.cfg.Reserve) {
			break
		}
		if err := s.Put(wire); err != nil {
			o.st.SendErrors++
		}
		if it.attempts == 0 {
			o.st.Sent++
		} else {
			o.st.Resent++
		}
		o.st.BytesOnWire += uint64(len(wire))
		it.attempts++
		it.lastSent = t
		back := o.cfg.AckTimeout << (it.attempts - 1)
		if back > o.cfg.MaxBackoff || back <= 0 {
			back = o.cfg.MaxBackoff
		}
		it.nextSend = t.Add(back)
		if o.firstUnack.IsZero() {
			o.firstUnack = t
		}
		n++
	}
	return n
}

// Stats returns a snapshot.
func (o *Outbox) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.now()
	s := o.st
	s.Queued = len(o.items)
	s.QueuedBytes = o.bytes
	s.Outage = t.Before(o.outageUntil)
	s.LastAckAgeS = -1
	if !o.lastAck.IsZero() {
		s.LastAckAgeS = t.Sub(o.lastAck).Seconds()
	}
	for _, it := range o.items {
		if it.attempts > 0 {
			s.Unacked++
		}
		if a := t.Sub(it.created).Seconds(); a > s.OldestS {
			s.OldestS = a
		}
	}
	return s
}
