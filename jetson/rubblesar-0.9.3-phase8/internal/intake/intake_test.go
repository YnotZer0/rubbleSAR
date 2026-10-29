package intake

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func det(inc, uav, id string, seq uint64) []byte {
	b, _ := msg.Marshal(&sarpb.Detection{Header: msg.NewHeader(inc, "uav/"+uav, seq), UavId: uav, DetectionId: id, TrackId: id,
		Reason: sarpb.ReportReason_REPORT_REASON_NEW, Part: sarpb.PartClass_PART_CLASS_HAND, Position: &sarpb.GeoPoint{LatDeg: 36.2, LonDeg: 36.16},
		CaptureUnixNs: time.Now().Add(-time.Second).UnixNano(), Summary: "Hand exposed",
		VisibleCrop: &sarpb.Crop{Format: "jpeg", Data: []byte{0xff, 0xd8, 1, 2}}, ThermalCrop: &sarpb.Crop{Format: "png16-dk", Data: []byte{0x89, 'P'}}})
	return b
}

func TestStoreDedupeAckAndReload(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	dir := t.TempDir()
	var mu sync.Mutex
	acked := map[string]int{}
	m.Subscribe(keys.AckAll("inc"), func(s bus.Sample) {
		var a sarpb.DetectionAck
		if msg.Unmarshal(s.Payload, &a) != nil {
			t.Error("bad ack")
			return
		}
		mu.Lock()
		for _, id := range a.DetectionIds {
			acked[id]++
		}
		mu.Unlock()
	})
	in, err := Open(Config{Incident: "inc", Dir: dir, SaveCrops: true}, m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, b []byte) { m.Put(key, b, bus.QoSDetection) }
	r1, r2 := det("inc", "scout-1", "scout-1-0001.1", 1), det("inc", "scout-1", "scout-1-0002.1", 2)
	put(keys.Detection("inc", "scout-1"), r1)
	put(keys.Detection("inc", "scout-1"), r2)
	put(keys.Detection("inc", "scout-1"), r1)                                     // repeat: the same report sent again
	put(keys.Detection("inc", "inv-1"), det("other", "inv-1", "inv-1-0001.1", 1)) // wrong incident
	put(keys.Detection("inc", "inv-1"), det("inc", "inv-2", "inv-2-0001.1", 1))   // wrong key
	time.Sleep(200 * time.Millisecond)
	in.Flush()
	time.Sleep(200 * time.Millisecond)
	st := in.Stats()
	if st.UAVs["scout-1"].Stored != 2 || st.UAVs["scout-1"].Duplicates != 1 || st.Invalid != 2 {
		t.Fatalf("stats %+v %+v", st, st.UAVs["scout-1"])
	}
	mu.Lock()
	if acked["scout-1-0001.1"] != 2 || acked["scout-1-0002.1"] != 1 || len(acked) != 2 {
		t.Fatalf("acks %v", acked)
	}
	mu.Unlock()
	in.Close()

	f, _ := os.Open(filepath.Join(dir, fileName))
	sc := bufio.NewScanner(f)
	var recs []Record
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	f.Close()
	if len(recs) != 2 || recs[0].Part != "hand" || recs[0].Reason != "new" || recs[0].LatencyMS < 900 || recs[0].VisibleCrop == "" {
		t.Fatalf("records %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(dir, recs[0].VisibleCrop)); err != nil {
		t.Fatal("crop not written")
	}
	// Reopen: the stored ids are known, a repeat is not stored again.
	in2, err := Open(Config{Incident: "inc", Dir: dir}, m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	put(keys.Detection("inc", "scout-1"), r2)
	time.Sleep(200 * time.Millisecond)
	if st := in2.Stats(); st.Loaded != 2 || st.UAVs["scout-1"].Duplicates != 1 || st.UAVs["scout-1"].Stored != 0 {
		t.Fatalf("after reopen %+v %+v", st, st.UAVs["scout-1"])
	}
}

// An edge-eye before 0.4.2 numbered its tracks from 1 again after a restart:
// a report with an id already stored but made at another time is a different
// report. It is stored (under <id>~<time>, with its own crops) and
// acknowledged, not dropped as a repeat; after a reload, sending it again is
// a repeat.
func TestReusedIDIsStored(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	dir := t.TempDir()
	in, err := Open(Config{Incident: "inc", Dir: dir, SaveCrops: true}, m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	key := keys.Detection("inc", "inv-1")
	first := det("inc", "inv-1", "inv-1-0001.1", 1)
	time.Sleep(2 * time.Millisecond)
	second := det("inc", "inv-1", "inv-1-0001.1", 1) // after a restart: same id, new report
	m.Put(key, first, bus.QoSDetection)
	m.Put(key, second, bus.QoSDetection)
	m.Put(key, second, bus.QoSDetection) // and that one sent again
	time.Sleep(200 * time.Millisecond)
	in.Flush()
	u := in.Stats().UAVs["inv-1"]
	if u.Stored != 2 || u.Conflicts != 1 || u.Duplicates != 1 {
		t.Fatalf("stats %+v", u)
	}
	if !strings.Contains(in.Stats().Text(), "reused an id") {
		t.Fatalf("status does not mention the reused id:\n%s", in.Stats().Text())
	}
	in.Close()
	crops, _ := filepath.Glob(filepath.Join(dir, "crops", "inv-1", "*.jpg"))
	if len(crops) != 2 {
		t.Fatalf("crops %v (the second report must not overwrite the first)", crops)
	}
	in2, err := Open(Config{Incident: "inc", Dir: dir}, m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	m.Put(key, second, bus.QoSDetection)
	m.Put(key, first, bus.QoSDetection)
	time.Sleep(200 * time.Millisecond)
	if u := in2.Stats().UAVs["inv-1"]; u.Stored != 0 || u.Duplicates != 2 {
		t.Fatalf("after reload %+v", u)
	}
}
