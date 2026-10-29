// Package dataset turns simulated scenarios into a training set for the
// body-part segmentation model: many random scenarios, camera poses like the
// drones fly, 640x640 tiles with exact outline labels in the Ultralytics YOLO
// segmentation format, plus a held-out test set taken from the installed demo
// scenario, which is never used for training.
package dataset

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"rubblesar/internal/diag"
)

// Range is a [min, max] pair.
type Range [2]float64

// Config is configs/dataset/*.toml.
type Config struct {
	Dataset struct {
		Name           string  `toml:"name"`
		Seed           int64   `toml:"seed"`
		Tile           int     `toml:"tile"`
		JPEGQuality    Range   `toml:"jpeg_quality"`
		BackgroundKeep float64 `toml:"background_keep"`
		MinInstancePx  int     `toml:"min_instance_px"`
		SimplifyPx     float64 `toml:"simplify_px"`
		// MergeFragments labels the pieces of one victim's body part that debris
		// splits apart as one object (one outline, joined by zero-area bridges);
		// pieces smaller than MinPiecePx are left out. v1 labels every piece separately.
		MergeFragments bool `toml:"merge_fragments"`
		MinPiecePx     int  `toml:"min_piece_px"`
	} `toml:"dataset"`
	Split struct {
		TrainScenarios int `toml:"train_scenarios"`
		ValScenarios   int `toml:"val_scenarios"`
	} `toml:"split"`
	Scenarios struct {
		BaseConfig     string `toml:"base_config"`
		Sites          Range  `toml:"sites"`
		VictimsPerSite Range  `toml:"victims_per_site"`
		StartHour      Range  `toml:"start_hour"`
		AirMinC        Range  `toml:"air_min_c"`
		AirSpanC       Range  `toml:"air_span_c"`
		AliveFraction  Range  `toml:"alive_fraction"`
	} `toml:"scenarios"`
	Frames struct {
		PerScenario       int     `toml:"per_scenario"`
		InvestigatorShare float64 `toml:"investigator_share"`
		ScoutShare        float64 `toml:"scout_share"`
		InvAltM           Range   `toml:"investigator_alt_m"`
		InvOffsetM        float64 `toml:"investigator_offset_m"`
		ScoutAltM         Range   `toml:"scout_alt_m"`
		ScoutOffsetM      float64 `toml:"scout_offset_m"`
		PitchDeg          Range   `toml:"pitch_deg"`
		TimeOffsetH       Range   `toml:"time_offset_h"`
		NoiseSigma        Range   `toml:"noise_sigma"`
		BlurShare         float64 `toml:"blur_share"`
		BlurSigma         Range   `toml:"blur_sigma"`
	} `toml:"frames"`
	Test struct {
		Scenario    string  `toml:"scenario"`
		InvAltM     float64 `toml:"investigator_alt_m"`
		ScoutAltM   float64 `toml:"scout_alt_m"`
		GPSNoiseM   float64 `toml:"gps_noise_m"`
		AltNoiseM   float64 `toml:"alt_noise_m"`
		YawNoiseDeg float64 `toml:"yaw_noise_deg"`
	} `toml:"test"`
	dir string // directory of the config file, for relative paths
}

// Default returns the built-in v1 configuration.
func Default() Config {
	var c Config
	c.Dataset.Name, c.Dataset.Seed, c.Dataset.Tile = "rubblesar-v1", 100, 640
	c.Dataset.JPEGQuality = Range{82, 95}
	c.Dataset.BackgroundKeep, c.Dataset.MinInstancePx, c.Dataset.SimplifyPx = 0.15, 6, 0.7
	c.Dataset.MergeFragments, c.Dataset.MinPiecePx = false, 3
	c.Split.TrainScenarios, c.Split.ValScenarios = 30, 6
	c.Scenarios.BaseConfig = "../scenario/antakya-demo.toml"
	c.Scenarios.Sites = Range{3, 4}
	c.Scenarios.VictimsPerSite = Range{4, 7}
	c.Scenarios.StartHour = Range{7.5, 15.5}
	c.Scenarios.AirMinC = Range{-3, 14}
	c.Scenarios.AirSpanC = Range{5, 14}
	c.Scenarios.AliveFraction = Range{0.5, 0.8}
	f := &c.Frames
	f.PerScenario, f.InvestigatorShare, f.ScoutShare = 80, 0.5, 0.4
	f.InvAltM, f.InvOffsetM = Range{10, 18}, 4
	f.ScoutAltM, f.ScoutOffsetM = Range{30, 50}, 15
	f.PitchDeg, f.TimeOffsetH = Range{-90, -65}, Range{0, 6}
	f.NoiseSigma, f.BlurShare, f.BlurSigma = Range{1, 4}, 0.25, Range{0.4, 1.2}
	c.Test.Scenario = "/var/lib/rubblesar/scenarios/antakya-demo"
	c.Test.InvAltM, c.Test.ScoutAltM = 13, 40
	c.Test.GPSNoiseM, c.Test.AltNoiseM, c.Test.YawNoiseDeg = 1.5, 0.3, 1.0
	return c
}

// Load reads a dataset config on top of the defaults.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, diag.Wrap("E-CFG-001", err, "%s", path)
	}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return c, diag.Wrap("E-CFG-002", err, "%s", path)
	}
	if und := md.Undecoded(); len(und) > 0 {
		names := make([]string, len(und))
		for i, k := range und {
			names[i] = k.String()
		}
		return c, diag.New("E-CFG-003", "%s: unknown setting(s) %s", path, strings.Join(names, ", "))
	}
	c.dir = filepath.Dir(path)
	return c, c.Validate()
}

// Resolve returns a path relative to the config file.
func (c Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) || c.dir == "" {
		return p
	}
	return filepath.Join(c.dir, p)
}

func (r Range) ok(lo, hi float64) bool { return r[0] >= lo && r[1] <= hi && r[0] <= r[1] }

// Validate checks the configuration.
func (c Config) Validate() error {
	var p []string
	add := func(s string) { p = append(p, s) }
	if c.Dataset.Tile < 320 || c.Dataset.Tile > 1280 || c.Dataset.Tile%32 != 0 {
		add("[dataset] tile must be a multiple of 32 between 320 and 1280")
	}
	if !c.Dataset.JPEGQuality.ok(50, 100) {
		add("[dataset] jpeg_quality must lie within 50..100")
	}
	if c.Dataset.MinPiecePx < 1 || c.Dataset.MinPiecePx > c.Dataset.MinInstancePx {
		add("[dataset] min_piece_px must be at least 1 and at most min_instance_px")
	}
	if c.Dataset.BackgroundKeep < 0 || c.Dataset.BackgroundKeep > 1 {
		add("[dataset] background_keep must be 0..1")
	}
	if c.Split.TrainScenarios < 1 || c.Split.ValScenarios < 1 || c.Split.TrainScenarios+c.Split.ValScenarios > 500 {
		add("[split] need >= 1 train and >= 1 val scenario (500 at most)")
	}
	if !c.Scenarios.Sites.ok(1, 10) || !c.Scenarios.VictimsPerSite.ok(1, 15) || !c.Scenarios.StartHour.ok(5, 19) ||
		!c.Scenarios.AliveFraction.ok(0, 1) || !c.Scenarios.AirSpanC.ok(0, 25) || !c.Scenarios.AirMinC.ok(-30, 40) {
		add("[scenarios] a range is outside its allowed limits (see the comments in the config)")
	}
	f := c.Frames
	if f.PerScenario < 1 || f.InvestigatorShare < 0 || f.ScoutShare < 0 || f.InvestigatorShare+f.ScoutShare > 1 {
		add("[frames] per_scenario >= 1 and investigator_share + scout_share <= 1")
	}
	if !f.InvAltM.ok(3, 120) || !f.ScoutAltM.ok(3, 200) || !f.PitchDeg.ok(-90, -20) || !f.TimeOffsetH.ok(0, 12) {
		add("[frames] altitude, pitch (-90..-20) or time range out of limits")
	}
	if len(p) > 0 {
		return diag.New("E-CFG-003", "dataset config: %s", strings.Join(p, "; "))
	}
	return nil
}
