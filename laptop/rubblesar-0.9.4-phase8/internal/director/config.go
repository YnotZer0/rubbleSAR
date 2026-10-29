package director

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed demo.toml
var defaultConfig []byte

// DefaultConfigText is the commented demo.toml written by `rubblesar demo init`.
func DefaultConfigText() []byte { return append([]byte(nil), defaultConfig...) }

// Config is demo.toml.
type Config struct {
	Demo struct {
		TakesDir   string  `toml:"takes_dir"`
		Storyboard string  `toml:"storyboard"`
		Web        string  `toml:"web"`
		EdgeSeed   int64   `toml:"edge_seed"`
		SlateS     float64 `toml:"slate_s"`
		TTS        string  `toml:"tts"`
	} `toml:"demo"`
	Live struct {
		Events []LiveEvent `toml:"event"`
	} `toml:"live"`
	Recorders []RecorderConfig `toml:"recorder"`
	Path      string           `toml:"-"`
}

// LiveEvent is a staged link event of the live take.
type LiveEvent struct {
	At         string   `toml:"at"`
	UAV        string   `toml:"uav"`
	BudgetKbps *float64 `toml:"budget_kbps"`
	For        string   `toml:"for"`
	Blackout   string   `toml:"blackout"`
}

// RecorderConfig is one recorder.
type RecorderConfig struct {
	Name        string   `toml:"name"`
	Kind        string   `toml:"kind"` // obs, command
	URL         string   `toml:"url"`
	Password    string   `toml:"password"`
	Takes       []string `toml:"takes"`
	SceneLive   string   `toml:"scene_live"`
	SceneReplay string   `toml:"scene_replay"`
	Start       string   `toml:"start"`
	File        string   `toml:"file"`
	Enabled     *bool    `toml:"enabled"`
}

// On says whether the recorder takes part in a take ("live" or "replay").
func (r RecorderConfig) On(take string) bool {
	if r.Enabled != nil && !*r.Enabled {
		return false
	}
	for _, t := range r.Takes {
		if t == take {
			return true
		}
	}
	return len(r.Takes) == 0
}

// DefaultConfigPath is ~/.config/rubblesar/demo.toml.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rubblesar", "demo.toml")
}

// ExpandHome turns a leading ~/ into the home folder.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// LoadConfig reads demo.toml; a missing file gives the defaults (no recorders but this ZBook's OBS).
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b, path = defaultConfig, "built-in demo settings ("+path+" does not exist: rubblesar demo init)"
	} else if err != nil {
		return nil, fmt.Errorf("E-DIR-002: %w", err)
	}
	md, err := toml.Decode(string(b), c)
	if err != nil {
		return nil, fmt.Errorf("E-DIR-002: %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		var names []string
		for _, k := range und {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("E-DIR-002: %s: unknown setting(s) %s (typo?)", path, strings.Join(names, ", "))
	}
	c.Path = path
	if c.Demo.TakesDir == "" {
		c.Demo.TakesDir = "~/rubblesar-takes"
	}
	c.Demo.TakesDir = ExpandHome(c.Demo.TakesDir)
	c.Demo.Storyboard = ExpandHome(c.Demo.Storyboard)
	if c.Demo.Web == "" {
		c.Demo.Web = "http://127.0.0.1:8090"
	}
	if c.Demo.EdgeSeed == 0 {
		c.Demo.EdgeSeed = 1
	}
	if c.Demo.SlateS < 0 {
		c.Demo.SlateS = 0
	}
	return c, c.Validate()
}

// Validate checks the settings.
func (c *Config) Validate() error {
	var bad []string
	names := map[string]bool{}
	for i, r := range c.Recorders {
		n := r.Name
		if n == "" {
			n = fmt.Sprintf("recorder %d", i+1)
			bad = append(bad, n+": no name")
		}
		if names[r.Name] {
			bad = append(bad, n+": name used twice")
		}
		names[r.Name] = true
		switch r.Kind {
		case "obs":
			if u, err := url.Parse(r.URL); err != nil || u.Scheme != "ws" || u.Host == "" {
				bad = append(bad, fmt.Sprintf("%s: url %q must be ws://host:4455", n, r.URL))
			}
		case "command":
			if strings.TrimSpace(r.Start) == "" {
				bad = append(bad, n+": a command recorder needs start")
			}
		default:
			bad = append(bad, fmt.Sprintf("%s: kind %q (obs or command)", n, r.Kind))
		}
		for _, t := range r.Takes {
			if t != "live" && t != "replay" {
				bad = append(bad, fmt.Sprintf("%s: take %q (live or replay)", n, t))
			}
		}
	}
	for i, e := range c.Live.Events {
		n := fmt.Sprintf("live.event %d", i+1)
		if e.At == "" || e.UAV == "" {
			bad = append(bad, n+": needs at and uav")
		}
		if (e.BudgetKbps == nil) == (e.Blackout == "") {
			bad = append(bad, n+": set either budget_kbps (with for) or blackout")
		}
		for _, d := range []string{e.For, e.Blackout} {
			if d != "" {
				if _, err := time.ParseDuration(d); err != nil {
					bad = append(bad, fmt.Sprintf("%s: %q is not a duration (e.g. 60s)", n, d))
				}
			}
		}
		if e.BudgetKbps != nil && e.For == "" {
			bad = append(bad, n+": budget_kbps needs for (how long)")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("E-DIR-002: %s: %s", c.Path, strings.Join(bad, "; "))
	}
	return nil
}

// Init writes demo.toml and storyboard.toml into dir (not over existing files).
func Init(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("E-DIR-002: %w", err)
	}
	var wrote []string
	for _, f := range []struct {
		name string
		body []byte
		mode os.FileMode
	}{{"demo.toml", defaultConfig, 0o600}, {"storyboard.toml", defaultStoryboard, 0o644}} {
		p := filepath.Join(dir, f.name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		body := f.body
		if f.name == "demo.toml" {
			body = []byte(strings.Replace(string(body), `storyboard = ""`, fmt.Sprintf("storyboard = %q", filepath.Join(dir, "storyboard.toml")), 1))
		}
		if err := os.WriteFile(p, body, f.mode); err != nil {
			return wrote, fmt.Errorf("E-DIR-002: %w", err)
		}
		wrote = append(wrote, p)
	}
	return wrote, nil
}
