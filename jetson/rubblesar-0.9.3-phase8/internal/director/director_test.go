package director

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/aar"
	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/live"
	"rubblesar/internal/msg"
	"rubblesar/internal/obs"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/web"
)

func TestBuiltInStoryboard(t *testing.T) {
	sb, err := LoadStoryboard("")
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Chapters) < 10 || sb.Chapters[0].Card != "title" || sb.Chapters[len(sb.Chapters)-1].Card != "end" {
		t.Fatalf("chapters %d", len(sb.Chapters))
	}
	f := &Facts{Place: "central Antakya, in southern Türkiye", Sites: 5, Victims: 23, VictimsSentence: "eight conscious", Drones: 3, ScoutAltM: "40",
		GSDcm: "3", SegMS: "70", FirstRedSkin: "31", FirstRedAmbient: "9", AvgReportKB: "4.2", FrameMB: "1.2", FrameSize: "about a megabyte", BudgetKbps: "256", Blackout: true,
		BlackoutS: "60", HoverS: "30", Found: 21, Rescued: 20, RedYellow: 20, RealFreeing: "1 hour 40 minutes", FalseRecords: 7}
	total := 0.0
	for _, c := range sb.Chapters {
		n, err := Fill(c.Narration, f)
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		if strings.Contains(n, "{{") || strings.Contains(n, "<no value>") || strings.Contains(n, "  ") {
			t.Errorf("%s: %q", c.ID, n)
		}
		d := sb.Duration(c, n)
		total += d.Seconds()
		for _, s := range Segments(n, d) {
			if Words(s.Text) > maxSegmentWords {
				t.Errorf("%s: caption of %d words: %q", c.ID, Words(s.Text), s.Text)
			}
		}
	}
	// A few minutes, as asked.
	if total < 4*60 || total > 7*60 {
		t.Errorf("the built-in storyboard lasts %.0f s", total)
	}
	n, _ := Fill(sb.Chapters[len(sb.Chapters)-2].Narration, f)
	if !strings.Contains(n, "all 20 Red and Yellow") || !strings.Contains(n, "7 records were false alarms") {
		t.Errorf("aar narration: %s", n)
	}
	byID := map[string]string{}
	for _, c := range sb.Chapters {
		byID[c.ID] = c.Narration
	}
	f.BlackoutUAV, f.PlaceNote = "scout-1", ", a city heavily damaged in the February 2023 earthquakes"
	if n, _ := Fill(byID["radio"], f); !strings.Contains(n, "the link of scout-1 is cut completely for 60 seconds") || !strings.Contains(n, "none is lost") {
		t.Errorf("radio narration: %s", n)
	}
	f.Lost = 1
	if n, _ := Fill(byID["radio"], f); !strings.Contains(n, "1 report was lost") {
		t.Errorf("radio narration with a loss: %s", n)
	}
	f.Sites = 1
	if n, _ := Fill(byID["place"], f); !strings.Contains(n, "one building has collapsed") || !strings.Contains(n, "Türkiye, a city heavily damaged") {
		t.Errorf("place narration: %s", n)
	}
	// The video names the command post's computer by the storyboard's
	// command_post, never by its make.
	if sb.CommandPost != DefaultCommandPost {
		t.Errorf("command_post %q", sb.CommandPost)
	}
	f.CommandPost = "a GPU-equipped laptop"
	for _, c := range sb.Chapters {
		for _, x := range append([]string{c.Narration, c.Title, c.CardTitle, c.CardSubtitle}, c.CardLines...) {
			n, _ := Fill(x, f)
			if strings.Contains(strings.ToLower(n), "zbook") || strings.Contains(n, "HP ") {
				t.Errorf("%s names the laptop: %q", c.ID, n)
			}
		}
	}
	if n, _ := Fill(byID["zbook"], f); !strings.Contains(n, "At the command post, a GPU-equipped laptop receives") {
		t.Errorf("command post narration: %s", n)
	}
	f.Rescued, f.FalseRecords = 18, 0
	n, _ = Fill(sb.Chapters[len(sb.Chapters)-2].Narration, f)
	if !strings.Contains(n, "18 of 20 Red and Yellow") || strings.Contains(n, "false alarms") {
		t.Errorf("aar narration: %s", n)
	}
}

func TestStoryboardErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sb.toml")
	os.WriteFile(p, []byte(`
[[chapter]]
id = "a"
panel = "nope"
map = "casualty:"
insert = "x"
narration = "Hello {{.Nope"
[[chapter]]
id = "a"
min_seconds = 3
colour = "red"
`), 0o644)
	_, err := LoadStoryboard(p)
	if err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("unknown key: %v", err)
	}
	os.WriteFile(p, []byte(`
[[chapter]]
id = "a"
panel = "nope"
map = "casualty:"
insert = "x"
narration = "Hello {{.Nope"
[[chapter]]
id = "a"
min_seconds = 3
`), 0o644)
	_, err = LoadStoryboard(p)
	for _, want := range []string{"unknown panel", "map \"casualty:\"", "insert \"x\"", "unclosed action", "used twice"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

func TestSegments(t *testing.T) {
	s := Segments("One two three. Four five six seven, eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twentyone twentytwo twentythree twentyfour twentyfive twentysix twentyseven twentyeight. End.", 10*time.Second)
	if len(s) != 4 || s[0].Text != "One two three." || !strings.HasSuffix(s[1].Text, ",") || s[3].Text != "End." {
		t.Fatalf("%+v", s)
	}
	if s[0].Start != 0 || s[3].End < 9.9 || s[3].End > 10.01 {
		t.Errorf("timing %+v", s)
	}
	if wrapCaption("a b c d e f", 46) != "a b c d e f" || !strings.Contains(wrapCaption(strings.Repeat("word ", 20), 46), "\n") {
		t.Error("wrapCaption")
	}
}

func TestConfig(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Recorders) != 3 || !c.Recorders[0].On("replay") || c.Recorders[1].On("live") || len(c.Live.Events) != 2 {
		t.Fatalf("defaults %+v", c)
	}
	p := filepath.Join(t.TempDir(), "demo.toml")
	os.WriteFile(p, []byte(`
[[recorder]]
name = "x"
kind = "obs"
url = "http://nope"
[[live.event]]
at = "first-red"
uav = "scout-1"
`), 0o600)
	_, err = LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "ws://host:4455") || !strings.Contains(err.Error(), "either budget_kbps") {
		t.Fatalf("%v", err)
	}
	dir := t.TempDir()
	wrote, err := Init(dir)
	if err != nil || len(wrote) != 2 {
		t.Fatalf("init %v %v", wrote, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "demo.toml")); fi.Mode().Perm() != 0o600 {
		t.Errorf("demo.toml mode %v", fi.Mode())
	}
	c, err = LoadConfig(filepath.Join(dir, "demo.toml"))
	if err != nil || c.Demo.Storyboard != filepath.Join(dir, "storyboard.toml") {
		t.Fatalf("init's config: %v %+v", err, c.Demo)
	}
	if _, err := LoadStoryboard(c.Demo.Storyboard); err != nil {
		t.Fatal(err)
	}
}

// recordRun makes a small recorded run on the in-memory bus: a scout surveying with
// a blackout, a Red casualty with its report, crops and frame, a close look, a
// TAK team sent and freeing it, chat.
func recordRun(t *testing.T, dir string) {
	t.Helper()
	m := bus.NewMemory()
	defer m.Close()
	chats := []aar.ChatLine{}
	r, err := aar.Start(aar.Options{Bus: m, Incident: "inc", Dir: dir, TelEvery: time.Millisecond, TeamsEvery: time.Millisecond,
		Meta:  aar.Meta{Run: "r1", Suite: "test", Scenario: "antakya-demo", Staging: [2]float64{36.2016, 36.1585}},
		Chats: func() ([]aar.ChatLine, error) { return chats, nil }})
	if err != nil {
		t.Fatal(err)
	}
	var seq msg.Seq
	put := func(key string, v proto.Message, set func(h *sarpb.Header)) {
		set(msg.NewHeader("inc", "test", seq.Next()))
		b, _ := msg.Marshal(v)
		if err := m.Put(key, b, bus.PubOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	pos := func(lat, lon float64) *sarpb.GeoPoint { return &sarpb.GeoPoint{LatDeg: lat, LonDeg: lon} }
	tel := func(i int, queued uint32) {
		x := &sarpb.Telemetry{UavId: "scout-1", Role: sarpb.UavRole_UAV_ROLE_SCOUT, Position: pos(36.2010+float64(i)*1e-5, 36.1580), AltAglM: 40,
			State: sarpb.FlightState_FLIGHT_STATE_SURVEY, BatteryPct: 90, Link: &sarpb.LinkStats{BudgetBps: 256000, UsedBps: 40000, QueuedMsgs: queued},
			Perception: &sarpb.PerceptionStats{Enabled: true, Backend: "tensorrt 10.3.0 on Orin", SegmentMs: 68, MissionSteps: 12, MissionStep: uint32(i)}}
		put(keys.Telemetry("inc", "scout-1"), x, func(h *sarpb.Header) { x.Header = h })
	}
	tel(1, 0)
	truth := &sarpb.SimFrameTruth{UavId: "scout-1", FrameId: "scout-1-000042", SimTimeS: 612, CaptureUnixNs: time.Now().UnixNano(),
		TruePose: &sarpb.CapturePose{Position: pos(36.20101, 36.15801), AltAglM: 40, YawDeg: 10, PitchDeg: -90}}
	put(keys.SimTruth("inc", "scout-1"), truth, func(h *sarpb.Header) { truth.Header = h })
	det := &sarpb.Detection{UavId: "scout-1", DetectionId: "scout-1-k-0001.1", TrackId: "scout-1-k-0001", Revision: 1, Part: sarpb.PartClass_PART_CLASS_HAND,
		Confidence: 0.82, Position: pos(36.2041, 36.1612), CepM: 2.1, FrameId: "scout-1-000042", Payload: sarpb.Payload_PAYLOAD_FULL,
		Thermal:     &sarpb.ThermalReading{Readable: true, Warm: true, SkinC: 31.4, AmbientC: 8.8},
		VisibleCrop: &sarpb.Crop{Format: "jpeg", Data: []byte{0xff, 0xd8, 0xff, 0xd9}}, ThermalCrop: &sarpb.Crop{Format: "png16-dk", Data: []byte{1, 2}}}
	put(keys.Detection("inc", "scout-1"), det, func(h *sarpb.Header) { det.Header = h })
	red := &sarpb.Casualty{CasualtyId: "C-001", Triage: sarpb.Triage_TRIAGE_RED, Provisional: true, Position: pos(36.2041, 36.1612), CepM: 2.1,
		UavIds: []string{"scout-1"}, Reason: "warm; not yet watched close up", BestDetectionId: "scout-1-k-0001.1",
		Thermal: &sarpb.CasualtyThermal{Readable: true, Warm: true, SkinC: 31.4, AmbientC: 8.8}, Parts: []*sarpb.CasualtyPart{{UavId: "scout-1", DetectionId: "scout-1-k-0001.1", Part: sarpb.PartClass_PART_CLASS_HAND}}}
	put(keys.Casualty("inc", "C-001"), red, func(h *sarpb.Header) { red.Header = h })
	l := &sarpb.CasualtyList{Casualties: []*sarpb.Casualty{red}}
	put(keys.Casualties("inc"), l, func(h *sarpb.Header) { l.Header = h })
	tel(2, 3)
	// The blackout: no telemetry for 7 s.
	time.Sleep(7 * time.Second)
	tel(3, 5)
	tel(4, 0)
	tasks := &sarpb.TaskList{Tasks: []*sarpb.Task{{TaskId: "T-1", CasualtyId: "C-001", UavId: "inv-1", State: sarpb.TaskState_TASK_STATE_ON_STATION}}}
	put(keys.Tasks("inc"), tasks, func(h *sarpb.Header) { tasks.Header = h })
	teams := []*sarpb.Team{{TeamId: "vader", Callsign: "VADER", Mode: "foot", Source: "tak", Position: pos(36.2016, 36.1585)}}
	asg := &sarpb.Assignment{AssignmentId: "A-1", CasualtyId: "C-001", TeamId: "vader", Triage: sarpb.Triage_TRIAGE_RED, State: sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE,
		Route: &sarpb.RoutePlan{LengthM: 400, EtaS: 300, Points: []*sarpb.GeoPoint{pos(36.2016, 36.1585), pos(36.2041, 36.1612)}}, CreatedUnixNs: time.Now().UnixNano()}
	st := &sarpb.DispatchState{Teams: teams, Assignments: []*sarpb.Assignment{asg}}
	put(keys.Dispatch("inc"), st, func(h *sarpb.Header) { st.Header = h })
	chats = append(chats, aar.ChatLine{At: time.Now(), From: "SAR-OPS", To: "VADER", Text: "Go to C-001"}, aar.ChatLine{At: time.Now().Add(time.Second), From: "VADER", To: "SAR-OPS", Text: "done"})
	time.Sleep(3200 * time.Millisecond) // the recorder polls the chat every 3 s
	asg.State, asg.DoneUnixNs = sarpb.AssignmentState_ASSIGNMENT_STATE_DONE, time.Now().UnixNano()
	put(keys.Dispatch("inc"), st, func(h *sarpb.Header) { st.Header = h })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// A replay take against sar-web (the real server) with two fake OBS recorders.
func TestReplayTake(t *testing.T) {
	if testing.Short() {
		t.Skip("records a run (about 12 s)")
	}
	root := t.TempDir()
	runDir := filepath.Join(root, "reports", "inc", "20261021T090000Z")
	recordRun(t, runDir)
	p, err := live.LoadRun(runDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, b := range p.Beats() {
		names[b.Name] = true
	}
	for _, b := range []string{"first-casualty", "first-red", "first-hover", "first-team", "first-tak-team", "outage", "outage-end", "first-freed", "first-tak-reply"} {
		if !names[b] {
			t.Errorf("beat %s missing (have %v)", b, names)
		}
	}
	f := RunFacts(p, nil, nil)
	if f.FirstRedSkin != "31" || f.FirstRedAmbient != "9" || f.BudgetKbps != "256" || f.SegMS != "68" || !f.Blackout || f.TakTeams != "VADER" {
		t.Errorf("facts %+v", f)
	}
	// sar-web with the run, and its presenter.
	present := web.NewPresenter()
	srv := &web.Server{Root: root, Version: "test", Replays: &web.Replays{Dir: filepath.Join(root, "reports", "inc")}, Present: present}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// A presenter page listening (as OBS's browser source would).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/stream?src=present", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	zb, wt := obs.NewFake("pw"), obs.NewFake("")
	defer zb.Close()
	defer wt.Close()
	sb := &Storyboard{WordsPerMinute: 600, Chapters: []Chapter{
		{ID: "title", Card: "title", CardTitle: "RubbleSAR", Narration: "This is a test. Everything is simulated.", MinSeconds: 0.5, Map: "sites", Replay: "start", Speed: ptr(0.0)},
		{ID: "radio", Title: "Radio", Panel: "radio", Narration: "The link of {{.BlackoutUAV}} was lost for {{.BlackoutS}} seconds.", Replay: "outage-2s", Speed: ptr(4.0), Map: "follow:scout-1", Clock: "mission"},
		{ID: "tak", Title: "TAK", Panel: "tak", Narration: "A team on WinTAK.", Replay: "first-tak-team", Speed: ptr(2.0), Insert: "wintak@first-tak-team-1s~4", Scenes: map[string]string{"wintak": "Scene"}},
		{ID: "nope", Title: "Missing", Narration: "Gone.", Replay: "no-such-beat", Focus: "first-red"},
	}}
	pl, err := MakePlan(sb, p, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "no-such-beat") || pl.Chapters[3].Focus != "C-001" || pl.Chapters[2].Insert == nil {
		t.Fatalf("plan warnings %v, chapters %+v", pl.Warnings, pl.Chapters[3])
	}
	cfg := &Config{}
	cfg.Demo.TakesDir, cfg.Demo.SlateS = t.TempDir(), 0.2
	rs := []Recorder{NewRecorder(RecorderConfig{Name: "zbook", Kind: "obs", URL: zb.URL, Password: "pw"}), NewRecorder(RecorderConfig{Name: "wintak", Kind: "obs", URL: wt.URL})}
	var out strings.Builder
	cs, dir, err := ReplayTake(context.Background(), ReplayOptions{Cfg: cfg, Plan: pl, Web: NewWeb(ts.URL), Recorders: rs, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	t.Log(out.String())
	if len(cs.Chapters) != 4 || len(cs.Recorders) != 2 || cs.Recorders[0].Output == "" || zb.Recording() || len(zb.Chapters) != 4 {
		t.Fatalf("cue sheet %+v; zbook chapters %v", cs, zb.Chapters)
	}
	if c, _ := present.Cue(); c.Source != "replay" || c.Chapter != "nope" {
		t.Errorf("last cue %+v", c)
	}
	if info := srv.Replays.Current().Info(); info.Playing {
		t.Error("the replay should be paused after the take")
	}
	for _, f := range []string{"cues.json", "plan.json", "narration.txt", "narration.srt", "shotlist.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	srt, _ := os.ReadFile(filepath.Join(dir, "narration.srt"))
	if !strings.Contains(string(srt), "1\n00:00:0") || !strings.Contains(string(srt), "lost for") {
		t.Errorf("srt:\n%s", srt)
	}
	// The rough cut: the main recording is OBS's file on this machine; stand in with files.
	main := filepath.Join(t.TempDir(), "zbook.mkv")
	os.WriteFile(main, []byte("x"), 0o644)
	wintak := filepath.Join(t.TempDir(), "wintak.mkv")
	os.WriteFile(wintak, []byte("x"), 0o644)
	// A live take of the same run, whose wintak recording started 10 s before the run's beats.
	liveDir := filepath.Join(cfg.Demo.TakesDir, "20261021T085959Z-live")
	os.MkdirAll(liveDir, 0o755)
	live := &CueSheet{Take: "20261021T085959Z-live", Kind: "live", Run: p.Run, Recorders: []Recording{{Name: "wintak", Kind: "obs", Started: p.Start().Add(-10 * time.Second), Output: `C:\Users\x\wintak.mkv`}}}
	live.Save(liveDir)
	os.MkdirAll(filepath.Join(dir, "voice"), 0o755)
	writeWAV(t, filepath.Join(dir, "voice", "01-title.wav"), 1.5)
	cp, err := PlanCut(CutOptions{Take: dir, Live: liveDir, Main: main, Clips: map[string]string{"wintak": wintak}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cp.Args, " ")
	if len(cp.Inserts) != 1 || cp.Voice != 1 || !strings.Contains(args, wintak) || !strings.Contains(args, "overlay=x=0:y=56") || !strings.Contains(args, "adelay=") {
		t.Fatalf("cut %+v\n%s", cp, args)
	}
	// Without the clip there is no insert, with a warning.
	cp, _ = PlanCut(CutOptions{Take: dir, Live: liveDir, Main: main})
	if len(cp.Inserts) != 0 || len(cp.Warnings) == 0 {
		t.Errorf("cut without the clip: %+v", cp)
	}
	// "wintak|atak": WinTAK's file is on the laptop (no clip), so the ATAK phone's, on this ZBook, is used.
	atak := filepath.Join(t.TempDir(), "atak.mkv")
	os.WriteFile(atak, []byte("x"), 0o644)
	live.Recorders = append(live.Recorders, Recording{Name: "atak", Kind: "command", Started: p.Start().Add(-5 * time.Second), Output: atak})
	live.Save(liveDir)
	both, err := LoadCueSheet(dir)
	if err != nil {
		t.Fatal(err)
	}
	both.Chapters[2].Insert.Recorder = "wintak|atak"
	both.Save(dir)
	cp, _ = PlanCut(CutOptions{Take: dir, Live: liveDir, Main: main})
	font := firstFile("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf")
	if a := strings.Join(cp.Args, " "); len(cp.Inserts) != 1 || !strings.Contains(a, atak) || (font != "" && !strings.Contains(a, "ATAK, recorded live")) {
		t.Errorf("cut falling back to ATAK: %+v", cp)
	}
	cp, _ = PlanCut(CutOptions{Take: dir, Live: liveDir, Main: main, Clips: map[string]string{"wintak": wintak}})
	if len(cp.Inserts) != 1 || !strings.Contains(strings.Join(cp.Args, " "), wintak) {
		t.Errorf("cut with the WinTAK clip: %+v", cp)
	}
	if d, ok := wavSeconds(filepath.Join(dir, "voice", "01-title.wav")); !ok || d < 1.49 || d > 1.51 {
		t.Errorf("wav length %v %v", d, ok)
	}
	b, _ := json.Marshal(cs)
	if !strings.Contains(string(b), `"insert"`) {
		t.Error("the insert is not in the cue sheet")
	}
}

func ptr(v float64) *float64 { return &v }

// writeWAV writes a silent 16-bit mono WAV of d seconds.
func writeWAV(t *testing.T, path string, d float64) {
	rate := 16000
	n := int(d * float64(rate))
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(2*rate))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*n))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The live take's beats and staged events, from a timeline.
func TestWatchBeats(t *testing.T) {
	h := live.NewHub(live.HubOptions{Incident: "inc"})
	cfg := &Config{}
	kb := 24.0
	cfg.Live.Events = []LiveEvent{{At: "first-red+1s", UAV: "scout-1", BudgetKbps: &kb, For: "1s"}, {At: "first-red+2s", UAV: "scout-1", Blackout: "1s"}}
	var calls []string
	var beats []string
	var out strings.Builder
	o := LiveOptions{Cfg: cfg, Out: &out}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchBeats(o, h, time.Now(), func(name, ref, text string) { beats = append(beats, name+":"+ref) },
			func(uav string, bps uint32) error {
				calls = append(calls, "budget "+uav+" "+itoa(int(bps)))
				return nil
			},
			func(uav string, s uint32) error { calls = append(calls, "blackout "+uav+" "+itoa(int(s))); return nil }, stop)
	}()
	h.SetDrone(&live.Drone{ID: "scout-1", Role: "scout", State: "takeoff", Lat: 36.2, Lon: 36.16, Link: &live.Link{BudgetBps: 256000}}, time.Now())
	h.SetDrone(&live.Drone{ID: "scout-1", Role: "scout", State: "survey", Steps: 12, Lat: 36.2, Lon: 36.16, Link: &live.Link{BudgetBps: 256000}}, time.Now())
	h.SetCasualties([]*live.Casualty{{ID: "C-001", Triage: "Red", Prov: true, UAVs: []string{"scout-1"}}}, time.Now())
	h.SetDispatch(live.Dispatch{Teams: []*live.Team{{ID: "vader", Callsign: "VADER", Source: "tak", Mode: "foot"}},
		Assignments: []*live.Assignment{{ID: "A-1", Casualty: "C-001", Team: "vader", State: "en_route", Triage: "Red"}}}, time.Now())
	time.Sleep(4500 * time.Millisecond)
	close(stop)
	<-done
	got := strings.Join(beats, " ")
	for _, want := range []string{"survey:scout-1", "first-casualty:C-001", "first-red:C-001", "first-tak-team:C-001", "budget-cut:scout-1", "budget-restore:scout-1", "blackout:scout-1", "blackout-end:scout-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("beat %s missing: %s", want, got)
		}
	}
	if strings.Contains(got, "first-team:") {
		t.Errorf("a TAK team's assignment is first-tak-team, not also first-team: %s", got)
	}
	sort.Strings(calls)
	if strings.Join(calls, "; ") != "blackout scout-1 1; budget scout-1 24000; budget scout-1 256000" {
		t.Errorf("calls %q", calls)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The phase 8 acceptance against sar-web (the real server) and a fake OBS.
func TestPhase8(t *testing.T) {
	if testing.Short() {
		t.Skip("records a run (about 12 s)")
	}
	root := t.TempDir()
	runDir := filepath.Join(root, "reports", "inc", "20261021T090000Z")
	recordRun(t, runDir)
	// The media as `rubblesar demo media` writes it (rendering needs the scenario).
	os.MkdirAll(filepath.Join(runDir, "media"), 0o755)
	b, _ := json.Marshal(Media{Run: "r1", Frame: "scout-1/42", UAV: "scout-1", Exact: true, AltM: 40, GSDcm: 2.9, Model: "ONNX Runtime on the ZBook", InferMS: 61,
		Detections: []MediaDet{{Class: "leg", Conf: 0.8}}, Truth: 1})
	os.WriteFile(filepath.Join(runDir, "media", "media.json"), b, 0o644)
	srv := &web.Server{Root: root, Version: "test", Replays: &web.Replays{Dir: filepath.Join(root, "reports", "inc")}, Present: web.NewPresenter()}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/stream?src=present", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	zb := obs.NewFake("pw")
	defer zb.Close()
	cfg := &Config{Path: "demo.toml", Recorders: []RecorderConfig{{Name: "zbook", Kind: "obs", URL: zb.URL, Password: "pw", Takes: []string{"live", "replay"}}}}
	cfg.Demo.TakesDir, cfg.Demo.Web, cfg.Demo.EdgeSeed = t.TempDir(), ts.URL, 1
	var out strings.Builder
	r := Phase8(context.Background(), Phase8Options{Demo: cfg, Web: NewWeb(ts.URL), RunDir: runDir, RecordFor: 300 * time.Millisecond}, &out)
	txt := out.String()
	t.Log("\n" + txt)
	for _, want := range []string{"[PASS] P8-01", "P8-02", "[PASS] P8-03", "P8-04", "[PASS] P8-05", "[PASS] P8-06", "P8-07", "[SKIP] P8-08"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(txt, "[FAIL]") || !r.Passed() {
		t.Error("phase 8 should pass")
	}
	if zb.Recording() {
		t.Error("the test recording was not stopped")
	}
	if c, _ := srv.Present.Cue(); c.Source != "live" {
		t.Errorf("presenter left on %+v", c)
	}
	// No run: P8-02 fails, the rest that need it are skipped.
	out.Reset()
	r = Phase8(context.Background(), Phase8Options{Demo: cfg, Web: NewWeb(ts.URL), RecordFor: 100 * time.Millisecond}, &out)
	if txt := out.String(); !strings.Contains(txt, "[FAIL] P8-02") || !strings.Contains(txt, "[SKIP] P8-05") || r.Passed() {
		t.Errorf("without a run:\n%s", txt)
	}
}

func TestStoryboardBeats(t *testing.T) {
	sb, err := LoadStoryboard("")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(sb.Beats(), " ")
	if got != "survey first-casualty first-red outage first-team first-hover first-tak-team" {
		t.Errorf("beats %q", got)
	}
}

// A replay's picture read while it seeks and plays (a lock-order deadlock
// between the player and its hub once stopped TestReplayTake).
func TestReplaySnapshotWhileSeeking(t *testing.T) {
	if testing.Short() {
		t.Skip("records a run (about 12 s)")
	}
	runDir := filepath.Join(t.TempDir(), "20261021T090000Z")
	recordRun(t, runDir)
	p, err := live.LoadRun(runDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	done := make(chan struct{})
	stop := time.Now().Add(1500 * time.Millisecond)
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(stop); i++ {
				if i%2 == 0 {
					p.Seek(p.End())
				} else {
					p.Seek(p.Start())
				}
				p.Play(50)
			}
		}()
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				_ = p.Snapshot()
				_ = p.Info()
			}
		}()
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: Snapshot and Seek did not finish")
	}
}

func TestOBSConnectionHints(t *testing.T) {
	r := NewRecorder(RecorderConfig{Name: "wintak", Kind: "obs", URL: "ws://127.0.0.1:1"})
	defer r.Close()
	_, err := r.Check(context.Background(), "live")
	if err == nil || !strings.Contains(err.Error(), "OBS is not running there") {
		t.Errorf("refused: %v", err)
	}
}

func TestTakTeamsLine(t *testing.T) {
	sim := &live.Team{Callsign: "TEAM-1", Source: "sim", State: "idle"}
	vader := &live.Team{Callsign: "VADER", Source: "tak", State: "offline"}
	check1 := &live.Team{Callsign: "CHECK-1", Source: "tak", State: "offline"}
	atak := &live.Team{Callsign: "ATAK-1", Source: "tak", State: "idle"}

	l := TakTeamsLine([]*live.Team{sim})
	if !l.Warn || !strings.Contains(l.Detail, "none in rescue-dispatch") {
		t.Fatalf("no TAK teams: %+v", l)
	}
	l = TakTeamsLine([]*live.Team{sim, vader, check1})
	if !l.Warn || !strings.Contains(l.Detail, "VADER, CHECK-1 offline") {
		t.Fatalf("none online: %+v", l)
	}
	l = TakTeamsLine([]*live.Team{sim, vader, check1, atak})
	if l.Warn || !strings.HasPrefix(l.Detail, "ATAK-1 online") || !strings.Contains(l.Detail, "offline: VADER, CHECK-1") {
		t.Fatalf("one online: %+v", l)
	}
	on := *vader
	on.State = "en_route"
	l = TakTeamsLine([]*live.Team{&on, atak})
	if !l.Warn || !strings.HasPrefix(l.Detail, "VADER, ATAK-1 online") {
		t.Fatalf("two online: %+v", l)
	}
}
