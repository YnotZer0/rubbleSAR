package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geom"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/tiles"
	"rubblesar/internal/vcam"
)

// GeneratorVersion changes whenever the same config would give different data.
const GeneratorVersion = "1.0"

// Options control a build.
type Options struct {
	Out      string
	KeepWork bool         // keep each scenario's rendered layers
	Log      func(string) // progress lines
	Limit    int          // > 0: only this many train and val scenarios (smoke test)
	Frames   int          // > 0: override frames per scenario (smoke test)
	Now      func() time.Time
}

// SplitStats counts what went into one split.
type SplitStats struct {
	Scenarios     int            `json:"scenarios"`
	Frames        int            `json:"frames"`
	FramesByKind  map[string]int `json:"frames_by_kind"`
	Tiles         int            `json:"tiles"`
	PositiveTiles int            `json:"positive_tiles"`
	Instances     map[string]int `json:"instances"`
	AreaBuckets   map[string]int `json:"instance_area_px"`
}

func newStats() *SplitStats {
	return &SplitStats{FramesByKind: map[string]int{}, Instances: map[string]int{}, AreaBuckets: map[string]int{}}
}

// Manifest is manifest.json at the dataset root.
type Manifest struct {
	Schema           int                    `json:"schema"`
	Name             string                 `json:"name"`
	Generator        string                 `json:"generator"`
	Created          string                 `json:"created"`
	Config           Config                 `json:"config"`
	Classes          []string               `json:"classes"`
	Splits           map[string]*SplitStats `json:"splits"`
	Scenarios        map[string][]string    `json:"scenarios"` // split -> scenario ids
	TestScenario     string                 `json:"test_scenario"`
	TestScenarioHash string                 `json:"test_scenario_hash"`
	TestFrames       []string               `json:"test_frames"`
	Seconds          float64                `json:"seconds"`
}

func bucket(a int) string {
	switch {
	case a < 16:
		return "<16"
	case a < 64:
		return "16-63"
	case a < 256:
		return "64-255"
	case a < 1024:
		return "256-1023"
	}
	return ">=1024"
}

// Variant returns the generator config of dataset scenario i.
func Variant(c Config, base scenario.Config, split string, i int) scenario.Config {
	rng := rand.New(rand.NewSource(c.Dataset.Seed*1_000_003 + int64(i)*7919 + map[string]int64{"train": 0, "val": 500_009}[split]))
	u := func(r Range) float64 { return r[0] + rng.Float64()*(r[1]-r[0]) }
	ui := func(r Range) int { return int(r[0]) + rng.Intn(int(r[1])-int(r[0])+1) }
	s := base
	s.Scenario.ID = fmt.Sprintf("ds-%s-%03d", split, i)
	s.Scenario.Name = fmt.Sprintf("dataset %s scenario %d", split, i)
	s.Scenario.Seed = rng.Int63()
	h := u(c.Scenarios.StartHour)
	hh, mm := int(h), int((h-math.Floor(h))*60)
	s.Scenario.Start = fmt.Sprintf("2026-%02d-%02dT%02d:%02d:00+03:00", 1+rng.Intn(12), 1+rng.Intn(28), hh, mm)
	s.Sites.Count = ui(c.Scenarios.Sites)
	s.Victims.PerSiteMin = int(c.Scenarios.VictimsPerSite[0])
	s.Victims.PerSiteMax = int(c.Scenarios.VictimsPerSite[1])
	s.Victims.AliveFraction = u(c.Scenarios.AliveFraction)
	s.Environment.AirMinC = math.Round(u(c.Scenarios.AirMinC)*10) / 10
	s.Environment.AirMaxC = s.Environment.AirMinC + math.Round(u(c.Scenarios.AirSpanC)*10)/10
	s.Sites.OSMBuildings = ""
	return s
}

// Build generates the dataset.
func Build(c Config, o Options) (*Manifest, error) {
	t0 := time.Now()
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	base, err := scenario.LoadConfig(c.Resolve(c.Scenarios.BaseConfig))
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"images/train", "images/val", "images/test", "labels/train", "labels/val", "labels/test", "test", "work", "done"} {
		if err := os.MkdirAll(filepath.Join(o.Out, d), 0o755); err != nil {
			return nil, diag.Wrap("E-RND-001", err, "creating %s", d)
		}
	}
	m := &Manifest{Schema: 1, Name: c.Dataset.Name, Generator: GeneratorVersion, Config: c, Classes: ClassNames,
		Splits: map[string]*SplitStats{"train": newStats(), "val": newStats(), "test": newStats()}, Scenarios: map[string][]string{}}
	counts := map[string]int{"train": c.Split.TrainScenarios, "val": c.Split.ValScenarios}
	if o.Limit > 0 {
		counts["train"], counts["val"] = min(o.Limit, counts["train"]), min(o.Limit, counts["val"])
	}
	total := counts["train"] + counts["val"]
	n := 0
	for _, split := range []string{"train", "val"} {
		for i := 0; i < counts[split]; i++ {
			n++
			sc := Variant(c, base, split, i)
			m.Scenarios[split] = append(m.Scenarios[split], sc.Scenario.ID)
			st, err := scenarioTiles(c, o, sc, split)
			if err != nil {
				return nil, err
			}
			merge(m.Splits[split], st)
			el := time.Since(t0).Seconds()
			o.Log(fmt.Sprintf("[%d/%d] %s: %d frames, %d tiles (%d with body parts); %.0f s elapsed, about %.0f s left",
				n, total, sc.Scenario.ID, st.Frames, st.Tiles, st.PositiveTiles, el, el/float64(n)*float64(total-n)))
		}
	}
	if err := testSet(c, o, m); err != nil {
		return nil, err
	}
	if err := writeYAML(o.Out); err != nil {
		return nil, err
	}
	if err := Preview(o.Out, filepath.Join(o.Out, "preview.png"), 36, c.Dataset.Seed); err != nil {
		o.Log("preview not written: " + err.Error())
	}
	if !o.KeepWork {
		os.RemoveAll(filepath.Join(o.Out, "work"))
	}
	m.Created = o.Now().UTC().Format(time.RFC3339)
	m.Seconds = math.Round(time.Since(t0).Seconds())
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(o.Out, "manifest.json"), append(b, '\n'), 0o644); err != nil {
		return nil, diag.Wrap("E-RND-001", err, "writing manifest.json")
	}
	return m, nil
}

func merge(a, b *SplitStats) {
	a.Scenarios += b.Scenarios
	a.Frames += b.Frames
	a.Tiles += b.Tiles
	a.PositiveTiles += b.PositiveTiles
	for k, v := range b.FramesByKind {
		a.FramesByKind[k] += v
	}
	for k, v := range b.Instances {
		a.Instances[k] += v
	}
	for k, v := range b.AreaBuckets {
		a.AreaBuckets[k] += v
	}
}

// scenarioTiles generates, renders and photographs one scenario. A finished
// scenario leaves a marker so an interrupted build resumes where it stopped.
func scenarioTiles(c Config, o Options, sc scenario.Config, split string) (*SplitStats, error) {
	marker := filepath.Join(o.Out, "done", sc.Scenario.ID+".json")
	if b, err := os.ReadFile(marker); err == nil {
		st := newStats()
		if json.Unmarshal(b, st) == nil {
			return st, nil
		}
	}
	s, err := scenario.Generate(sc, nil)
	if err != nil {
		return nil, err
	}
	work := filepath.Join(o.Out, "work", sc.Scenario.ID)
	os.RemoveAll(work)
	if _, err := s.Save(work); err != nil {
		return nil, diag.Wrap("E-RND-001", err, "saving %s", work)
	}
	if _, err := render.RenderAll(work, s, nil); err != nil {
		return nil, err
	}
	w, err := vcam.Open(work, false)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(s.Seed ^ 0x5bd1e995))
	st := newStats()
	st.Scenarios = 1
	frames := c.Frames.PerScenario
	if o.Frames > 0 {
		frames = o.Frames
	}
	for f := 0; f < frames; f++ {
		kind, pose := samplePose(c, w, rng)
		req := vcam.Request{UAV: "dataset", FrameID: fmt.Sprintf("%s-f%03d", sc.Scenario.ID, f), Pose: pose,
			TOffsetS: (c.Frames.TimeOffsetH[0] + rng.Float64()*(c.Frames.TimeOffsetH[1]-c.Frames.TimeOffsetH[0])) * 3600,
			Seed:     rng.Int63(), NoTherm: true,
			NoiseSigma: c.Frames.NoiseSigma[0] + rng.Float64()*(c.Frames.NoiseSigma[1]-c.Frames.NoiseSigma[0])}
		if rng.Float64() < c.Frames.BlurShare {
			req.BlurSigma = c.Frames.BlurSigma[0] + rng.Float64()*(c.Frames.BlurSigma[1]-c.Frames.BlurSigma[0])
		}
		fr, err := w.Render(req)
		if err != nil {
			return nil, err
		}
		st.Frames++
		st.FramesByKind[kind]++
		if err := writeTiles(c, o.Out, split, req.FrameID, fr, rng, st, false); err != nil {
			return nil, err
		}
	}
	if !o.KeepWork {
		os.RemoveAll(work)
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		return nil, diag.Wrap("E-RND-001", err, "writing %s", marker)
	}
	return st, nil
}

// writeTiles cuts a frame into tiles and writes images and labels. Tiles
// without body parts are kept with probability background_keep (all of them
// when keepAll).
func writeTiles(c Config, out, split, frameID string, fr *vcam.Frame, rng *rand.Rand, st *SplitStats, keepAll bool) error {
	for ti, t := range tiles.Plan(fr.RGB.W, fr.RGB.H, c.Dataset.Tile) {
		var ins []Instance
		if c.Dataset.MergeFragments {
			ins = TileInstancesMerged(fr.PartsRGB, fr.InstRGB, t, c.Dataset.MinInstancePx, c.Dataset.MinPiecePx, c.Dataset.SimplifyPx)
		} else {
			ins = TileInstances(fr.PartsRGB, fr.InstRGB, t, c.Dataset.MinInstancePx, c.Dataset.SimplifyPx)
		}
		keep := keepAll || len(ins) > 0 || rng.Float64() < c.Dataset.BackgroundKeep
		q := int(c.Dataset.JPEGQuality[0]) + rng.Intn(int(c.Dataset.JPEGQuality[1]-c.Dataset.JPEGQuality[0])+1)
		if !keep {
			continue
		}
		stem := fmt.Sprintf("%s-t%d", frameID, ti)
		if err := writeJPEG(filepath.Join(out, "images", split, stem+".jpg"), CropRGB(fr.RGB, t), q); err != nil {
			return err
		}
		var lines []string
		for _, in := range ins {
			lines = append(lines, in.YOLOLine(t.Size))
			st.Instances[ClassNames[in.Class]]++
			st.AreaBuckets[bucket(in.Area)]++
		}
		body := strings.Join(lines, "\n")
		if body != "" {
			body += "\n"
		}
		if err := os.WriteFile(filepath.Join(out, "labels", split, stem+".txt"), []byte(body), 0o644); err != nil {
			return diag.Wrap("E-RND-001", err, "writing label %s", stem)
		}
		st.Tiles++
		if len(ins) > 0 {
			st.PositiveTiles++
		}
	}
	return nil
}

func writeJPEG(path string, m *raster.RGB, q int) error {
	f, err := os.Create(path)
	if err != nil {
		return diag.Wrap("E-RND-001", err, "creating %s", path)
	}
	w := bufio.NewWriter(f)
	if err := jpeg.Encode(w, m.ToNRGBA(), &jpeg.Options{Quality: q}); err != nil {
		f.Close()
		return diag.Wrap("E-RND-001", err, "encoding %s", path)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return diag.Wrap("E-RND-001", err, "writing %s", path)
	}
	return f.Close()
}

func disc(rng *rand.Rand, r float64) geom.P {
	a := rng.Float64() * 2 * math.Pi
	d := r * math.Sqrt(rng.Float64())
	return geom.P{X: d * math.Cos(a), Y: d * math.Sin(a)}
}

// lookAt places a camera so its optical axis hits target (local metres).
func lookAt(target geom.P, alt, yaw, pitch float64) vcam.Pose {
	p := vcam.Pose{E: target.X, N: target.Y, AltAGL: alt, YawDeg: yaw, PitchDeg: pitch}
	if pitch > -89.9 {
		back := alt / math.Tan(-pitch*math.Pi/180)
		p.E -= back * math.Sin(yaw*math.Pi/180)
		p.N -= back * math.Cos(yaw*math.Pi/180)
	}
	return p
}

// samplePose draws one capture like the drones fly: investigators close to a
// victim, scouts over a site, and some frames of decoys or open ground.
func samplePose(c Config, w *vcam.World, rng *rand.Rand) (string, vcam.Pose) {
	f := c.Frames
	u := func(r Range) float64 { return r[0] + rng.Float64()*(r[1]-r[0]) }
	yaw := rng.Float64() * 360
	pitch := u(f.PitchDeg)
	s := w.Scn
	x := rng.Float64()
	switch {
	case x < f.InvestigatorShare && len(s.Victims) > 0:
		// Victims showing rarer classes (leg, head, torso) are picked more often.
		weights := make([]float64, len(s.Victims))
		total := 0.0
		for i, v := range s.Victims {
			weights[i] = 1
			for _, cl := range v.Classes() {
				if cl == scenario.PartLeg || cl == scenario.PartHead || cl == scenario.PartTorso {
					weights[i] += 1.5
				}
			}
			total += weights[i]
		}
		r := rng.Float64() * total
		v := s.Victims[len(s.Victims)-1]
		for i, wgt := range weights {
			if r < wgt {
				v = s.Victims[i]
				break
			}
			r -= wgt
		}
		t := geom.P{X: v.E, Y: v.N}.Add(disc(rng, f.InvOffsetM))
		return "investigator", lookAt(t, u(f.InvAltM), yaw, pitch)
	case x < f.InvestigatorShare+f.ScoutShare && len(s.Sites) > 0:
		st := s.Sites[rng.Intn(len(s.Sites))]
		t := geom.P{X: st.CenterE, Y: st.CenterN}.Add(disc(rng, f.ScoutOffsetM))
		return "scout", lookAt(t, u(f.ScoutAltM), yaw, pitch)
	default:
		// Half on a decoy, half over open ground away from the sites.
		if rng.Intn(2) == 0 && len(s.Sites) > 0 {
			st := s.Sites[rng.Intn(len(s.Sites))]
			if m := w.Manifest(st.ID); m != nil && len(m.Decoys) > 0 {
				d := m.Decoys[rng.Intn(len(m.Decoys))]
				t := geom.P{X: d.E, Y: d.N}.Add(disc(rng, 3))
				return "decoy", lookAt(t, u(Range{f.InvAltM[0], f.ScoutAltM[1]}), yaw, pitch)
			}
		}
		for tries := 0; tries < 100; tries++ {
			t := disc(rng, math.Min(600, s.RadiusM-50))
			ok := true
			for _, st := range s.Sites {
				if t.Dist(geom.P{X: st.CenterE, Y: st.CenterN}) < 70 {
					ok = false
				}
			}
			if ok {
				return "open-ground", lookAt(t, u(f.ScoutAltM), yaw, pitch)
			}
		}
		return "open-ground", lookAt(geom.P{X: 0, Y: -700}, u(f.ScoutAltM), yaw, pitch)
	}
}

// TestFrame describes one held-out test capture. Frames are not stored:
// the renderer is deterministic, so evaluation renders them again from the
// installed scenario (on the ZBook or the Jetson) and gets identical pixels.
type TestFrame struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Pose     vcam.Pose `json:"pose"`
	Reported vcam.Pose `json:"reported_pose"`
	TOffsetS float64   `json:"t_offset_s"`
	Seed     int64     `json:"seed"`
}

// Request returns the camera request that renders this frame (colour only).
func (tf TestFrame) Request() vcam.Request {
	return vcam.Request{UAV: "test", FrameID: tf.ID, Pose: tf.Pose, Reported: tf.Reported, TOffsetS: tf.TOffsetS, Seed: tf.Seed, NoTherm: true}
}

// TestSet is test/frames.json.
type TestSet struct {
	Schema       int         `json:"schema"`
	Scenario     string      `json:"scenario"`
	ScenarioHash string      `json:"scenario_hash"`
	Frames       []TestFrame `json:"frames"`
}

// TestFrames lists the held-out captures of the test scenario: every victim
// from an investigator (nadir and oblique) and every site from the scout
// height (nadir and oblique), at several times of day. The reported pose
// carries GPS, altitude and heading errors.
func TestFrames(c Config, s *scenario.Scenario) []TestFrame {
	rng := rand.New(rand.NewSource(c.Dataset.Seed + 77))
	var out []TestFrame
	add := func(kind string, p vcam.Pose, t float64) {
		i := len(out)
		out = append(out, TestFrame{ID: fmt.Sprintf("%s-%03d", s.ID, i), Kind: kind, Pose: p, Reported: drift(c, p, c.Dataset.Seed+int64(i)),
			TOffsetS: t, Seed: int64(1000 + i)})
	}
	for _, v := range s.Victims {
		t := geom.P{X: v.E, Y: v.N}.Add(disc(rng, 2))
		add("investigator-nadir", lookAt(t, c.Test.InvAltM, rng.Float64()*360, -90), 0)
		t = geom.P{X: v.E, Y: v.N}.Add(disc(rng, 2))
		add("investigator-oblique", lookAt(t, c.Test.InvAltM+2, rng.Float64()*360, -70), 2*3600)
	}
	for _, st := range s.Sites {
		for k := 0; k < 4; k++ {
			t := geom.P{X: st.CenterE, Y: st.CenterN}.Add(disc(rng, 10))
			add("scout-nadir", lookAt(t, c.Test.ScoutAltM, float64(k)*90+rng.Float64()*20, -90), float64(k%2)*3*3600)
		}
		t := geom.P{X: st.CenterE, Y: st.CenterN}.Add(disc(rng, 5))
		add("scout-oblique", lookAt(t, c.Test.ScoutAltM, rng.Float64()*360, -60), 3600)
	}
	return out
}

// drift returns the reported pose (GPS/IMU errors).
func drift(c Config, p vcam.Pose, seed int64) vcam.Pose {
	rng := rand.New(rand.NewSource(seed))
	p.E += rng.NormFloat64() * c.Test.GPSNoiseM
	p.N += rng.NormFloat64() * c.Test.GPSNoiseM
	p.AltAGL += rng.NormFloat64() * c.Test.AltNoiseM
	p.YawDeg += rng.NormFloat64() * c.Test.YawNoiseDeg
	return p
}

func testSet(c Config, o Options, m *Manifest) error {
	dir := c.Test.Scenario
	w, err := vcam.Open(dir, false)
	if err != nil {
		return diag.Wrap("E-SCN-001", err, "the test set comes from the installed scenario %s (run 60-scenario.sh first)", dir)
	}
	m.TestScenario, m.TestScenarioHash = w.Scn.ID, w.Scn.Hash()
	st := m.Splits["test"]
	st.Scenarios = 1
	rng := rand.New(rand.NewSource(c.Dataset.Seed + 99))
	ts := TestSet{Schema: 1, Scenario: w.Scn.ID, ScenarioHash: w.Scn.Hash(), Frames: TestFrames(c, w.Scn)}
	for _, tf := range ts.Frames {
		fr, err := w.Render(tf.Request())
		if err != nil {
			return err
		}
		st.Frames++
		st.FramesByKind[tf.Kind]++
		if err := writeTiles(c, o.Out, "test", tf.ID, fr, rng, st, true); err != nil {
			return err
		}
		m.TestFrames = append(m.TestFrames, tf.ID)
	}
	b, _ := json.MarshalIndent(ts, "", "  ")
	if err := os.WriteFile(filepath.Join(o.Out, "test", "frames.json"), append(b, '\n'), 0o644); err != nil {
		return diag.Wrap("E-RND-001", err, "writing test/frames.json")
	}
	o.Log(fmt.Sprintf("test set: %d frames of %s (held out), %d tiles", st.Frames, w.Scn.ID, st.Tiles))
	return nil
}

// LoadTestSet reads test/frames.json of a dataset.
func LoadTestSet(datasetDir string) (*TestSet, error) {
	return LoadTestSetFile(filepath.Join(datasetDir, "test", "frames.json"))
}

// LoadTestSetFile reads a test-frame list (test/frames.json, or testset.json in a model folder).
func LoadTestSetFile(p string) (*TestSet, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, diag.Wrap("E-DS-001", err, "reading %s", p)
	}
	var ts TestSet
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, diag.Wrap("E-DS-001", err, "parsing %s", p)
	}
	return &ts, nil
}

// CheckScenario confirms the test frames belong to this scenario.
func (ts *TestSet) CheckScenario(s *scenario.Scenario) error {
	if ts.ScenarioHash != s.Hash() {
		return diag.New("E-DS-002", "the test frames were made from scenario %s (%s), the installed one is %s (%s)",
			ts.Scenario, short(ts.ScenarioHash), s.ID, short(s.Hash()))
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func writeYAML(out string) error {
	var b strings.Builder
	b.WriteString("# RubbleSAR body-part segmentation dataset, written by scenario-tool dataset.\n")
	b.WriteString("# Paths are relative to this file. Ultralytics finds labels/ next to images/.\n")
	b.WriteString("train: images/train\nval: images/val\ntest: images/test\nnames:\n")
	for i, n := range ClassNames {
		fmt.Fprintf(&b, "  %d: %s\n", i, n)
	}
	if err := os.WriteFile(filepath.Join(out, "data.yaml"), []byte(b.String()), 0o644); err != nil {
		return diag.Wrap("E-RND-001", err, "writing data.yaml")
	}
	return nil
}
