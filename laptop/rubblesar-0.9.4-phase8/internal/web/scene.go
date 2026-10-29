package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/scenario"
)

// Scene is what the operator view needs once: where the incident is, the map
// to draw it on, the rubble, the command post, the teams and the model.
type Scene struct {
	Incident string     `json:"incident"`
	Name     string     `json:"name,omitempty"`
	Version  string     `json:"version"`
	Centre   [2]float64 `json:"centre"`
	RadiusM  float64    `json:"radius_m"`
	Staging  [2]float64 `json:"staging"`
	Map      *SceneMap  `json:"map,omitempty"`
	// Rubble: each collapse's spill area (lat, lon rings); Sites: the collapsed buildings.
	Rubble [][][2]float64 `json:"rubble"`
	Sites  []SceneSite    `json:"sites"`
	Teams  []SceneTeam    `json:"teams"`
	Triage []string       `json:"triage"`
	// Simulated teams move TimeScale and free casualties RescueTimeScale times faster than real ones.
	TimeScale       float64 `json:"time_scale"`
	RescueTimeScale float64 `json:"rescue_time_scale"`
	// ScenarioStart: the scenario clock's zero (RFC 3339, local time at the incident).
	ScenarioStart string `json:"scenario_start,omitempty"`
	// Victims: the simulated victims by condition (the scenario's set-up; never their positions).
	Victims map[string]int `json:"victims,omitempty"`
	Model   *SceneModel    `json:"model,omitempty"`
}

// SceneMap is the offline map's tiles.
type SceneMap struct {
	URL     string `json:"url"` // /map/<incident>/{z}/{x}/{y}.png
	MinZoom int    `json:"min_zoom"`
	MaxZoom int    `json:"max_zoom"`
}

// SceneSite is a collapsed building.
type SceneSite struct {
	ID string `json:"id"`
	// Name: after its nearest street, from the offline map's layers ("" = not drawn yet).
	Name      string       `json:"name,omitempty"`
	Kind      string       `json:"kind"`
	Lat       float64      `json:"lat"`
	Lon       float64      `json:"lon"`
	HeightM   float64      `json:"height_m"`
	Footprint [][2]float64 `json:"footprint"`
}

// SceneTeam is a rescue team as configured.
type SceneTeam struct {
	ID       string `json:"id"`
	Callsign string `json:"callsign"`
	Mode     string `json:"mode"`
	Source   string `json:"source"`
}

// SceneModel is the body-part model's card.
type SceneModel struct {
	Name      string             `json:"name"`
	Arch      string             `json:"arch"`
	Classes   []string           `json:"classes"`
	InputSize int                `json:"input_size"`
	Epochs    int                `json:"epochs"`
	TrainedAt string             `json:"trained_at,omitempty"`
	GPU       string             `json:"gpu,omitempty"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
}

// SceneInput is where a Scene comes from.
type SceneInput struct {
	Incident, Version   string
	Centre, Staging     [2]float64
	RadiusM             float64
	ScenarioDir, MapDir string
	ModelDir            string
	// LayersFile: what the offline map was drawn from (site names).
	LayersFile        string
	Teams             []SceneTeam
	Triage            []string
	TimeScale, Rescue float64
}

// BuildScene reads the scenario, the offline map and the model card.
func BuildScene(in SceneInput) *Scene {
	s := &Scene{Incident: in.Incident, Version: in.Version, Centre: in.Centre, Staging: in.Staging, RadiusM: in.RadiusM, Teams: in.Teams,
		Triage: in.Triage, TimeScale: in.TimeScale, RescueTimeScale: in.Rescue, Rubble: [][][2]float64{}, Sites: []SceneSite{}}
	if s.Teams == nil {
		s.Teams = []SceneTeam{}
	}
	if sc, err := scenario.Load(filepath.Join(in.ScenarioDir, "scenario.json")); err == nil {
		s.Name, s.ScenarioStart = sc.Name, sc.Start
		loc := sc.Local()
		for _, b := range sc.Blockages {
			var ring [][2]float64
			for _, p := range b.Polygon {
				ring = append(ring, [2]float64{round6(p.Lat), round6(p.Lon)})
			}
			s.Rubble = append(s.Rubble, ring)
		}
		names := siteNames(in.LayersFile)
		for _, st := range sc.Sites {
			ss := SceneSite{ID: st.ID, Name: names[st.ID], Kind: st.Kind, Lat: round6(st.Center.Lat), Lon: round6(st.Center.Lon), HeightM: st.HeightM}
			for _, p := range st.Footprint {
				ll := loc.ToPoint(p[0], p[1])
				ss.Footprint = append(ss.Footprint, [2]float64{round6(ll.Lat), round6(ll.Lon)})
			}
			s.Sites = append(s.Sites, ss)
		}
		s.Victims = map[string]int{}
		for _, v := range sc.Victims {
			s.Victims[strings.ReplaceAll(v.Status, "_", " ")]++
		}
	}
	if in.MapDir != "" {
		minZ, maxZ := -1, -1
		ents, _ := os.ReadDir(in.MapDir)
		for _, e := range ents {
			var z int
			if !e.IsDir() || !isNumber(e.Name()) {
				continue
			}
			z = atoi(e.Name())
			if minZ < 0 || z < minZ {
				minZ = z
			}
			maxZ = max(maxZ, z)
		}
		if minZ >= 0 {
			s.Map = &SceneMap{URL: "/map/" + filepath.Base(in.MapDir) + "/{z}/{x}/{y}.png", MinZoom: minZ, MaxZoom: maxZ}
		}
	}
	if in.ModelDir != "" {
		if b, err := os.ReadFile(filepath.Join(in.ModelDir, "model.json")); err == nil {
			var m SceneModel
			var raw struct {
				Metrics map[string]float64 `json:"metrics"`
			}
			if json.Unmarshal(b, &m) == nil && json.Unmarshal(b, &raw) == nil {
				// Keep the overall test-set figures only.
				m.Metrics = map[string]float64{}
				keys := make([]string, 0, len(raw.Metrics))
				for k := range raw.Metrics {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if strings.Contains(k, ".all.") {
						m.Metrics[k] = raw.Metrics[k]
					}
				}
				s.Model = &m
			}
		}
	}
	return s
}

func isNumber(s string) bool {
	if s == "" || len(s) > 2 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func round6(v float64) float64 {
	if v < 0 {
		return -round6(-v)
	}
	return float64(int64(v*1e6+0.5)) / 1e6
}

// sceneCache rebuilds the scene at most every 30 s (a map or model installed meanwhile shows up).
type sceneCache struct {
	mu    sync.Mutex
	build func() *Scene
	at    time.Time
	s     *Scene
}

func (c *sceneCache) get() *Scene {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s == nil || time.Since(c.at) > 30*time.Second {
		c.s, c.at = c.build(), time.Now()
	}
	return c.s
}

// siteNames reads the collapse sites' names from the offline map's layers file.
func siteNames(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var l struct {
		Sites []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"sites"`
	}
	if json.Unmarshal(b, &l) == nil {
		for _, s := range l.Sites {
			if s.Name != "" {
				out[s.ID] = s.Name
			}
		}
	}
	return out
}
