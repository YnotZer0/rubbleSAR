// Package intake receives detection reports on the ZBook, stores them and
// acknowledges them to the drones (which then free their on-disk queues).
//
// Storage, per incident (default /var/lib/rubblesar/detections/<incident>/):
//
//	detections.jsonl          one line per stored report (metadata and file names)
//	crops/<uav>/<id>.jpg      visible crop
//	crops/<uav>/<id>.png      thermal crop (16-bit grey, kelvin x 10)
//
// A report that arrives again (its acknowledgement was lost) is acknowledged
// again but stored only once. Phase 4 builds casualty records on top of this.
package intake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/diag"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Config configures the intake.
type Config struct {
	Incident    string
	Dir         string // this incident's folder
	AckInterval time.Duration
	SaveCrops   bool
	Source      string // header source id, e.g. "zbook/casualty-analyst"
	// OnRecord, if set, is called for every newly stored report (with the
	// intake's lock held: it must not call back into the intake).
	OnRecord func(Record, *sarpb.Detection)
}

// Thermal is the stored temperature reading.
type Thermal struct {
	Readable bool    `json:"readable"`
	Reason   string  `json:"reason,omitempty"`
	SkinC    float32 `json:"skin_c,omitempty"`
	AmbientC float32 `json:"ambient_c,omitempty"`
	DeltaC   float32 `json:"delta_c,omitempty"`
	Pixels   uint32  `json:"pixels,omitempty"`
	Warm     bool    `json:"warm"`
}

// Record is one stored report (one line of detections.jsonl).
type Record struct {
	DetectionID  string    `json:"detection_id"`
	UAV          string    `json:"uav"`
	Seq          uint64    `json:"seq"`
	TrackID      string    `json:"track_id"`
	Revision     uint32    `json:"revision"`
	Reason       string    `json:"reason"`
	Part         string    `json:"part"`
	Confidence   float32   `json:"confidence"`
	Fused        float32   `json:"fused_confidence"`
	Lat          float64   `json:"lat"`
	Lon          float64   `json:"lon"`
	CEPm         float32   `json:"cep_m"`
	AreaM2       float32   `json:"area_m2"`
	Observations uint32    `json:"observations"`
	Thermal      Thermal   `json:"thermal"`
	Summary      string    `json:"summary"`
	Payload      string    `json:"payload"`
	Bytes        int       `json:"bytes"`
	FrameID      string    `json:"frame_id"`
	RangeM       float32   `json:"range_m"`
	SimTimeS     float64   `json:"sim_time_s"`
	CaptureNs    int64     `json:"capture_unix_ns"`
	CreatedNs    int64     `json:"created_unix_ns"`
	ReceivedNs   int64     `json:"received_unix_ns"`
	LatencyMS    float64   `json:"capture_to_arrival_ms"`
	ClassVotes   []float32 `json:"class_votes,omitempty"`
	Outline      []float32 `json:"outline,omitempty"`
	VisibleCrop  string    `json:"visible_crop,omitempty"`
	ThermalCrop  string    `json:"thermal_crop,omitempty"`
	// Phase 4.
	FirstSeenNs int64     `json:"first_seen_unix_ns,omitempty"`
	LastSeenNs  int64     `json:"last_seen_unix_ns,omitempty"`
	MinRangeM   float32   `json:"min_range_m,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	Movement    *Movement `json:"movement,omitempty"`
}

// Movement is the stored hover verdict of a report (phase 4).
type Movement struct {
	HoverFrames uint32  `json:"hover_frames"`
	MovedFrames uint32  `json:"moved_frames"`
	MaxChange   float32 `json:"max_change"`
	Moved       bool    `json:"moved"`
	WatchedS    float32 `json:"watched_s"`
}

// UAVStats counts one drone's reports.
type UAVStats struct {
	Stored     uint64    `json:"stored"`
	Duplicates uint64    `json:"duplicates"`
	Conflicts  uint64    `json:"id_conflicts"` // an id reused for a different report (stored under <id>~<time>)
	Bytes      uint64    `json:"bytes"`
	Acked      uint64    `json:"acks_sent"`
	LastAt     time.Time `json:"last_at"`
	LastSeq    uint64    `json:"last_seq"`
}

// Stats is the intake's state for its status endpoint.
type Stats struct {
	Incident string               `json:"incident"`
	Dir      string               `json:"dir"`
	Started  time.Time            `json:"started"`
	Loaded   int                  `json:"loaded_from_disk"`
	Invalid  uint64               `json:"invalid"`
	LastErr  string               `json:"last_error,omitempty"`
	UAVs     map[string]*UAVStats `json:"uavs"`
}

// Intake is safe for concurrent use.
type Intake struct {
	cfg  Config
	b    bus.Bus
	log  *slog.Logger
	sub  bus.Subscription
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	seen map[string]bool     // reportKey(id, created) of every stored report
	ids  map[string]bool     // the drone's ids stored so far
	acks map[string][]string // uav -> ids to acknowledge
	pubs map[string]bus.Publisher
	seq  msg.Seq
	st   Stats
}

const fileName = "detections.jsonl"

// reportKey identifies one report: its id and when the drone created it.
func reportKey(id string, created int64) string { return id + "@" + strconv.FormatInt(created, 10) }

// Open creates the folder, reloads the ids stored so far and subscribes.
func Open(cfg Config, b bus.Bus, log *slog.Logger) (*Intake, error) {
	if cfg.AckInterval <= 0 {
		cfg.AckInterval = 200 * time.Millisecond
	}
	if cfg.Source == "" {
		cfg.Source = "zbook/intake"
	}
	in := &Intake{cfg: cfg, b: b, log: log, seen: map[string]bool{}, ids: map[string]bool{}, acks: map[string][]string{}, pubs: map[string]bus.Publisher{},
		st: Stats{Incident: cfg.Incident, Dir: cfg.Dir, Started: time.Now(), UAVs: map[string]*UAVStats{}}}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "crops"), 0o755); err != nil {
		return nil, diag.Wrap("E-ANL-001", err, "creating %s", cfg.Dir)
	}
	p := filepath.Join(cfg.Dir, fileName)
	if f, err := os.Open(p); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var r struct {
				ID      string `json:"detection_id"`
				Created int64  `json:"created_unix_ns"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
				orig, _, _ := strings.Cut(r.ID, "~")
				in.ids[orig] = true
				in.seen[reportKey(orig, r.Created)] = true
			}
		}
		f.Close()
		in.st.Loaded = len(in.seen)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, diag.Wrap("E-ANL-001", err, "reading %s", p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, diag.Wrap("E-ANL-001", err, "opening %s", p)
	}
	in.f, in.w = f, bufio.NewWriter(f)
	if in.sub, err = b.Subscribe(keys.DetectionAll(cfg.Incident), in.handle); err != nil {
		f.Close()
		return nil, err
	}
	return in, nil
}

func (in *Intake) fail(err error) {
	in.st.Invalid++
	in.st.LastErr = err.Error()
}

func (in *Intake) handle(s bus.Sample) {
	var d sarpb.Detection
	err := msg.Unmarshal(s.Payload, &d)
	in.mu.Lock()
	defer in.mu.Unlock()
	if err != nil {
		in.fail(err)
		return
	}
	if err := msg.CheckHeader(d.GetHeader(), in.cfg.Incident); err != nil {
		in.fail(err)
		return
	}
	k, ok := keys.ParseUAV(s.Key)
	if !ok || k.UAV != d.GetUavId() || d.GetDetectionId() == "" || strings.ContainsAny(d.GetDetectionId(), "/\\") {
		in.fail(diag.New("E-MSG-004", "detection %q from %q arrived on %q", d.GetDetectionId(), d.GetUavId(), s.Key))
		return
	}
	u := in.st.UAVs[d.UavId]
	if u == nil {
		u = &UAVStats{}
		in.st.UAVs[d.UavId] = u
	}
	u.Bytes += uint64(len(s.Payload))
	in.acks[d.UavId] = append(in.acks[d.UavId], d.DetectionId)
	// A repeat is the same report sent again (same id and creation time). The
	// same id with another creation time is a different report: an edge-eye
	// that numbers its tracks from 1 again after a restart (before 0.4.2).
	key := reportKey(d.DetectionId, d.GetHeader().GetSentUnixNs())
	if in.seen[key] {
		u.Duplicates++
		return
	}
	rec := in.record(&d, s, len(s.Payload))
	if in.ids[d.DetectionId] {
		rec.DetectionID = d.DetectionId + "~" + strconv.FormatInt(d.GetHeader().GetSentUnixNs()/int64(time.Millisecond), 36)
		u.Conflicts++
		in.log.Warn("detection id reused for a different report; stored under a new id", "uav", d.UavId, "id", d.DetectionId, "stored_as", rec.DetectionID)
	}
	if err := in.store(&rec, &d); err != nil {
		in.fail(err)
		// Not acknowledged: the drone keeps it and sends it again.
		in.acks[d.UavId] = in.acks[d.UavId][:len(in.acks[d.UavId])-1]
		return
	}
	in.seen[key] = true
	in.ids[d.DetectionId] = true
	u.Stored++
	u.LastAt = s.Received
	if seq := d.GetHeader().GetSeq(); seq > u.LastSeq {
		u.LastSeq = seq
	}
	if d.Reason == sarpb.ReportReason_REPORT_REASON_NEW {
		in.log.Info("detection", "uav", d.UavId, "id", d.DetectionId, "part", rec.Part, "fused", rec.Fused, "summary", d.Summary)
	} else {
		in.log.Debug("detection update", "uav", d.UavId, "id", d.DetectionId, "summary", d.Summary)
	}
	if in.cfg.OnRecord != nil {
		in.cfg.OnRecord(rec, &d)
	}
}

func lower(s, prefix string) string { return strings.ToLower(strings.TrimPrefix(s, prefix)) }

func (in *Intake) record(d *sarpb.Detection, s bus.Sample, n int) Record {
	return RecordOf(d, s.Received, n)
}

// RecordOf is the stored record of a detection packet received at received
// (n bytes on the wire).
func RecordOf(d *sarpb.Detection, received time.Time, n int) Record {
	r := Record{DetectionID: d.DetectionId, UAV: d.UavId, Seq: d.GetHeader().GetSeq(), TrackID: d.TrackId, Revision: d.Revision,
		Reason: lower(d.Reason.String(), "REPORT_REASON_"), Part: strings.ReplaceAll(lower(d.Part.String(), "PART_CLASS_"), "_", " "),
		Confidence: d.Confidence, Fused: d.FusedConfidence, Lat: d.GetPosition().GetLatDeg(), Lon: d.GetPosition().GetLonDeg(),
		CEPm: d.CepM, AreaM2: d.AreaM2, Observations: d.Observations, Summary: d.Summary, Payload: lower(d.Payload.String(), "PAYLOAD_"),
		Bytes: n, FrameID: d.FrameId, RangeM: d.RangeM, SimTimeS: d.SimTimeS, CaptureNs: d.CaptureUnixNs, CreatedNs: d.GetHeader().GetSentUnixNs(),
		ReceivedNs: received.UnixNano(), ClassVotes: d.ClassVotes, Outline: d.Outline,
		FirstSeenNs: d.FirstSeenUnixNs, LastSeenNs: d.LastSeenUnixNs, MinRangeM: d.MinRangeM, TaskID: d.TaskId}
	if m := d.Movement; m != nil {
		r.Movement = &Movement{HoverFrames: m.HoverFrames, MovedFrames: m.MovedFrames, MaxChange: m.MaxChange, Moved: m.Moved, WatchedS: m.WatchedS}
	}
	if d.CaptureUnixNs > 0 {
		r.LatencyMS = float64(received.UnixNano()-d.CaptureUnixNs) / 1e6
	}
	if t := d.Thermal; t != nil {
		r.Thermal = Thermal{Readable: t.Readable, Reason: t.NotReadableReason, SkinC: t.SkinC, AmbientC: t.AmbientC, DeltaC: t.DeltaC, Pixels: t.Pixels, Warm: t.Warm}
	}
	return r
}

func (in *Intake) store(r *Record, d *sarpb.Detection) error {
	if in.cfg.SaveCrops {
		dir := filepath.Join(in.cfg.Dir, "crops", d.UavId)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return diag.Wrap("E-ANL-001", err, "creating %s", dir)
		}
		if c := d.VisibleCrop; c != nil && len(c.Data) > 0 {
			r.VisibleCrop = filepath.Join("crops", d.UavId, r.DetectionID+".jpg")
			if err := os.WriteFile(filepath.Join(in.cfg.Dir, r.VisibleCrop), c.Data, 0o644); err != nil {
				return diag.Wrap("E-ANL-001", err, "writing a crop")
			}
		}
		if c := d.ThermalCrop; c != nil && len(c.Data) > 0 {
			r.ThermalCrop = filepath.Join("crops", d.UavId, r.DetectionID+".png")
			if err := os.WriteFile(filepath.Join(in.cfg.Dir, r.ThermalCrop), c.Data, 0o644); err != nil {
				return diag.Wrap("E-ANL-001", err, "writing a crop")
			}
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := in.w.Write(append(b, '\n')); err != nil {
		return diag.Wrap("E-ANL-001", err, "writing %s", fileName)
	}
	return nil
}

// Run sends the acknowledgements and flushes the log every AckInterval until ctx ends.
func (in *Intake) Run(ctx context.Context) {
	t := time.NewTicker(in.cfg.AckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			in.Flush()
			return
		case <-t.C:
			in.Flush()
		}
	}
}

// Flush writes the stored records to disk and sends the pending acknowledgements.
func (in *Intake) Flush() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if err := in.w.Flush(); err != nil {
		in.fail(diag.Wrap("E-ANL-001", err, "writing %s", fileName))
		return // keep the acks: those reports are not safely stored yet
	}
	_ = in.f.Sync()
	for uav, ids := range in.acks {
		if len(ids) == 0 {
			continue
		}
		pub := in.pubs[uav]
		if pub == nil {
			p, err := in.b.DeclarePublisher(keys.Ack(in.cfg.Incident, uav), bus.QoSAck)
			if err != nil {
				in.fail(err)
				continue
			}
			pub, in.pubs[uav] = p, p
		}
		for len(ids) > 0 {
			n := min(len(ids), 200)
			b, err := msg.Marshal(&sarpb.DetectionAck{Header: msg.NewHeader(in.cfg.Incident, in.cfg.Source, in.seq.Next()), UavId: uav, DetectionIds: ids[:n]})
			if err == nil {
				err = pub.Put(b)
			}
			if err != nil {
				in.fail(err)
				break
			}
			in.st.UAVs[uav].Acked += uint64(n)
			ids = ids[n:]
		}
		in.acks[uav] = ids
	}
}

// Stats returns a snapshot.
func (in *Intake) Stats() Stats {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.st
	s.UAVs = map[string]*UAVStats{}
	for k, v := range in.st.UAVs {
		c := *v
		s.UAVs[k] = &c
	}
	return s
}

// Text renders the stats.
func (s Stats) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "incident %s, store %s, up since %s, %d reports loaded from disk, %d invalid\n", s.Incident, s.Dir, s.Started.UTC().Format(time.RFC3339), s.Loaded, s.Invalid)
	ids := make([]string, 0, len(s.UAVs))
	for k := range s.UAVs {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	fmt.Fprintf(&b, "%-8s %8s %10s %10s %10s %8s  %s\n", "UAV", "STORED", "REPEATS", "ACKS SENT", "KBYTES", "LAST SEQ", "LAST")
	for _, id := range ids {
		u := s.UAVs[id]
		last := "-"
		if !u.LastAt.IsZero() {
			last = time.Since(u.LastAt).Truncate(time.Second).String() + " ago"
		}
		fmt.Fprintf(&b, "%-8s %8d %10d %10d %10.1f %8d  %s\n", id, u.Stored, u.Duplicates, u.Acked, float64(u.Bytes)/1024, u.LastSeq, last)
	}
	for _, id := range ids {
		if c := s.UAVs[id].Conflicts; c > 0 {
			fmt.Fprintf(&b, "%s: %d reports reused an id already stored (stored as <id>~<time>; update edge-eye on the Jetson to 0.4.2 or later)\n", id, c)
		}
	}
	if s.LastErr != "" {
		fmt.Fprintf(&b, "last error: %s\n", s.LastErr)
	}
	return b.String()
}

// Close stops receiving and flushes.
func (in *Intake) Close() {
	if in.sub != nil {
		_ = in.sub.Close()
	}
	in.Flush()
	in.mu.Lock()
	defer in.mu.Unlock()
	_ = in.f.Close()
}

// ReadRecords reads the stored reports of an incident folder in the order they
// were stored (for rebuilding casualty records after a restart).
func ReadRecords(dir string) ([]Record, error) {
	f, err := os.Open(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, diag.Wrap("E-ANL-001", err, "reading %s", fileName)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.DetectionID != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// ArchiveDir is where Reset moves a finished run: <dir>/archive/<stamp>.
const ArchiveDir = "archive"

// Reset starts the incident folder afresh: everything stored so far
// (detections.jsonl, crops, and any other file of the run) moves to
// archive/<stamp>/ and the intake forgets the stored ids. Used by the
// acceptance run (sar-check phase4) so that each run is scored on its own.
// also, if set, runs under the intake's lock once the folder is empty (to
// reset what OnRecord feeds without losing a report in between).
func (in *Intake) Reset(stamp string, also func()) (string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if err := in.w.Flush(); err != nil {
		return "", diag.Wrap("E-ANL-001", err, "writing %s", fileName)
	}
	_ = in.f.Close()
	dst := filepath.Join(in.cfg.Dir, ArchiveDir, stamp)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", diag.Wrap("E-ANL-001", err, "creating %s", dst)
	}
	ents, err := os.ReadDir(in.cfg.Dir)
	if err != nil {
		return "", diag.Wrap("E-ANL-001", err, "reading %s", in.cfg.Dir)
	}
	for _, e := range ents {
		if e.Name() == ArchiveDir {
			continue
		}
		if err := os.Rename(filepath.Join(in.cfg.Dir, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return "", diag.Wrap("E-ANL-001", err, "archiving %s", e.Name())
		}
	}
	if err := os.MkdirAll(filepath.Join(in.cfg.Dir, "crops"), 0o755); err != nil {
		return "", diag.Wrap("E-ANL-001", err, "creating crops")
	}
	f, err := os.OpenFile(filepath.Join(in.cfg.Dir, fileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", diag.Wrap("E-ANL-001", err, "opening %s", fileName)
	}
	in.f, in.w = f, bufio.NewWriter(f)
	in.seen, in.ids, in.acks = map[string]bool{}, map[string]bool{}, map[string][]string{}
	in.st.UAVs, in.st.Loaded, in.st.Invalid, in.st.LastErr = map[string]*UAVStats{}, 0, 0, ""
	if also != nil {
		also()
	}
	return dst, nil
}
