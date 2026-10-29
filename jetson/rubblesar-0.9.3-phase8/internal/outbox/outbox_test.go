package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/linkbudget"
	"rubblesar/internal/sarpb"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time       { return c.t }
func (c *clock) add(d time.Duration)  { c.t = c.t.Add(d) }
func newClock() *clock                { return &clock{t: time.Unix(1_000_000, 0)} }
func (c *clock) fn() func() time.Time { return c.now }

type sink struct {
	got   []*sarpb.Detection
	bytes int
}

func (s *sink) Put(b []byte) error {
	var d sarpb.Detection
	if err := proto.Unmarshal(b, &d); err != nil {
		return err
	}
	s.got = append(s.got, &d)
	s.bytes += len(b)
	return nil
}

func det(id string, reason sarpb.ReportReason, part sarpb.PartClass, warm bool, fused float32, pad int, t time.Time) *sarpb.Detection {
	return &sarpb.Detection{Header: &sarpb.Header{SentUnixNs: t.UnixNano()}, DetectionId: id, Reason: reason, Part: part,
		FusedConfidence: fused, Thermal: &sarpb.ThermalReading{Warm: warm}, Summary: string(make([]byte, pad))}
}

func open(t *testing.T, dir string, c *clock, maxBytes int64, enc func(*sarpb.Detection, int) ([]byte, error)) *Outbox {
	t.Helper()
	o, err := Open(Config{Dir: dir, MaxBytes: maxBytes, AckTimeout: 8 * time.Second, MaxBackoff: 60 * time.Second,
		ProbeAfter: 20 * time.Second, ProbeEvery: 5 * time.Second, Reserve: 200, Encode: enc}, c.fn())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPriorityDiskAckAndReload(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	o := open(t, dir, c, 1<<20, nil)
	N, U := sarpb.ReportReason_REPORT_REASON_NEW, sarpb.ReportReason_REPORT_REASON_UPDATE
	arm, warm := sarpb.PartClass_PART_CLASS_ARM, sarpb.PartClass_PART_CLASS_WARM_SPOT
	for _, d := range []*sarpb.Detection{
		det("u-1.2", U, arm, false, 0.9, 10, c.t),
		det("u-2.1", N, warm, true, 0.3, 10, c.t),
		det("u-3.1", N, arm, false, 0.6, 10, c.t),
		det("u-4.1", N, arm, true, 0.8, 10, c.t),
	} {
		if err := o.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.pb"))
	if len(files) != 4 {
		t.Fatalf("%d files on disk", len(files))
	}
	var s sink
	b := linkbudget.New(0, c.fn())
	if n := o.Pump(&s, b); n != 4 {
		t.Fatalf("sent %d", n)
	}
	order := ""
	for _, d := range s.got {
		order += d.DetectionId + " "
	}
	if order != "u-4.1 u-3.1 u-1.2 u-2.1 " {
		t.Fatalf("send order %q", order)
	}
	o.Ack([]string{"u-4.1", "u-3.1", "unknown"})
	if st := o.Stats(); st.Queued != 2 || st.Acked != 2 || st.Unacked != 2 {
		t.Fatalf("after ack %+v", st)
	}
	files, _ = filepath.Glob(filepath.Join(dir, "*.pb"))
	if len(files) != 2 {
		t.Fatalf("%d files after ack", len(files))
	}
	// Restart: the unacknowledged reports come back and are sent again.
	o2 := open(t, dir, c, 1<<20, nil)
	if st := o2.Stats(); st.Queued != 2 || st.LoadedOnOpen != 2 {
		t.Fatalf("reload %+v", st)
	}
	var s2 sink
	if n := o2.Pump(&s2, b); n != 2 {
		t.Fatalf("resent after restart %d", n)
	}
}

func TestRetransmitBackoffAndProbe(t *testing.T) {
	c := newClock()
	o := open(t, t.TempDir(), c, 1<<20, nil)
	for i := 0; i < 3; i++ {
		o.Add(det(fmt.Sprintf("u-%d.1", i), sarpb.ReportReason_REPORT_REASON_NEW, sarpb.PartClass_PART_CLASS_HAND, false, 0.5, 10, c.t))
	}
	var s sink
	b := linkbudget.New(0, c.fn())
	o.Pump(&s, b)
	if len(s.got) != 3 {
		t.Fatal("first send")
	}
	c.add(7 * time.Second)
	if o.Pump(&s, b) != 0 {
		t.Fatal("re-sent before the ack timeout")
	}
	c.add(1 * time.Second) // 8 s: first retry
	if o.Pump(&s, b) != 3 {
		t.Fatal("no retry at the ack timeout")
	}
	c.add(15 * time.Second) // 23 s since first send: probing (>= 20 s without ack)
	n := o.Pump(&s, b)
	if st := o.Stats(); !st.Probing || n != 1 {
		t.Fatalf("expected one probe, sent %d, %+v", n, st)
	}
	c.add(2 * time.Second)
	if o.Pump(&s, b) != 0 {
		t.Fatal("probes must be spaced")
	}
	c.add(3 * time.Second)
	if o.Pump(&s, b) != 1 {
		t.Fatal("second probe missing")
	}
	o.Ack([]string{"u-0.1"})
	c.add(100 * time.Millisecond)
	o.Pump(&s, b)
	if st := o.Stats(); st.Probing {
		t.Fatalf("still probing after an ack: %+v", st)
	}
}

func TestOutageHoldsAndIgnoresAcks(t *testing.T) {
	c := newClock()
	o := open(t, t.TempDir(), c, 1<<20, nil)
	var s sink
	b := linkbudget.New(0, c.fn())
	o.SetOutage(60 * time.Second)
	o.Add(det("u-1.1", sarpb.ReportReason_REPORT_REASON_NEW, sarpb.PartClass_PART_CLASS_HEAD, true, 0.9, 10, c.t))
	for i := 0; i < 50; i++ {
		c.add(time.Second)
		if o.Pump(&s, b) != 0 {
			t.Fatal("sent during the outage")
		}
	}
	o.Ack([]string{"u-1.1"})
	if o.Stats().Queued != 1 {
		t.Fatal("ack during the outage was applied")
	}
	c.add(11 * time.Second)
	if o.Pump(&s, b) != 1 {
		t.Fatal("not sent after the outage")
	}
	o.Ack([]string{"u-1.1"})
	if st := o.Stats(); st.Queued != 0 || st.Outage {
		t.Fatalf("after outage %+v", st)
	}
}

// Under a small budget the queue degrades the payload and never exceeds the
// average rate (plus the one-second burst allowance).
func TestBudgetAndDegrade(t *testing.T) {
	c := newClock()
	levels := map[int]int{}
	enc := func(d *sarpb.Detection, lvl int) ([]byte, error) {
		levels[lvl]++
		dd := proto.Clone(d).(*sarpb.Detection)
		dd.Summary = dd.Summary[:len(dd.Summary)/(lvl+1)]
		return proto.Marshal(dd)
	}
	o := open(t, t.TempDir(), c, 1<<30, enc)
	const bps = 32000 // 4000 B/s
	b := linkbudget.New(bps, c.fn())
	for i := 0; i < 100; i++ { // 100 x ~4 KB = 100 s of budget at full size
		o.Add(det(fmt.Sprintf("u-%03d.1", i), sarpb.ReportReason_REPORT_REASON_NEW, sarpb.PartClass_PART_CLASS_ARM, false, 0.5, 4000, c.t))
	}
	var s sink
	const secs = 20
	maxLevel := 0
	for i := 0; i < secs*10; i++ {
		o.Pump(&s, b)
		if l := o.Stats().Level; l > maxLevel {
			maxLevel = l
		}
		c.add(100 * time.Millisecond)
	}
	limit := secs*bps/8 + bps/8 + 4100 // average rate + one second burst + one message of debt
	if s.bytes > limit {
		t.Fatalf("sent %d bytes in %d s at %d bit/s (limit %d)", s.bytes, secs, bps, limit)
	}
	if maxLevel < 2 || levels[0] != 0 {
		t.Fatalf("expected degraded payloads with a 100 s backlog: max level %d, encodings %v", maxLevel, levels)
	}
	t.Logf("sent %d reports (%d bytes) in %d s; levels used %v", len(s.got), s.bytes, secs, levels)
}

func TestQueueLimitDiscardsLowestPriority(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	o := open(t, dir, c, 3*1100, nil)
	N := sarpb.ReportReason_REPORT_REASON_NEW
	o.Add(det("spot.1", N, sarpb.PartClass_PART_CLASS_WARM_SPOT, true, 0.3, 1000, c.t))
	c.add(time.Second)
	o.Add(det("hand.1", N, sarpb.PartClass_PART_CLASS_HAND, false, 0.5, 1000, c.t))
	c.add(time.Second)
	o.Add(det("head.1", N, sarpb.PartClass_PART_CLASS_HEAD, true, 0.9, 1000, c.t))
	c.add(time.Second)
	o.Add(det("arm.1", N, sarpb.PartClass_PART_CLASS_ARM, false, 0.5, 1000, c.t))
	st := o.Stats()
	if st.Discarded != 1 || st.Queued != 3 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "spot.1.pb")); !os.IsNotExist(err) {
		t.Fatal("the warm spot should have been discarded first")
	}
}

func TestBadFilesAreSetAside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.pb"), []byte("not a protobuf \xff\xff"), 0o644)
	os.WriteFile(filepath.Join(dir, "y.pb.tmp"), []byte("half written"), 0o644)
	o := open(t, dir, newClock(), 1<<20, nil)
	if o.Stats().Queued != 0 {
		t.Fatal("bad file loaded")
	}
	if _, err := os.Stat(filepath.Join(dir, "x.pb.bad")); err != nil {
		t.Fatal("bad file not set aside")
	}
	if _, err := os.Stat(filepath.Join(dir, "y.pb.tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary file not removed")
	}
}
