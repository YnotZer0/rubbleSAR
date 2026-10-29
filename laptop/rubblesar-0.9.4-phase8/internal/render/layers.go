package render

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"rubblesar/internal/diag"
	"rubblesar/internal/raster"
	"rubblesar/internal/scenario"
)

// RendererVersion changes whenever the layer content for a given scenario changes.
const RendererVersion = "1.0"

// Frame maps site layer pixels to local metres. Pixel (x, y) covers
// [E0+x*GSD, E0+(x+1)*GSD] east and [N0-(y+1)*GSD, N0-y*GSD] north.
type Frame struct {
	E0  float64 `json:"e0_m"`
	N0  float64 `json:"n0_m"`
	GSD float64 `json:"gsd_m"`
	W   int     `json:"width_px"`
	H   int     `json:"height_px"`
}

// PixToEN returns the local metres of a pixel-centre coordinate (integer x, y = centre).
func (f Frame) PixToEN(x, y float64) (e, n float64) {
	return f.E0 + (x+0.5)*f.GSD, f.N0 - (y+0.5)*f.GSD
}

// ENToPix is the inverse of PixToEN.
func (f Frame) ENToPix(e, n float64) (x, y float64) {
	return (e-f.E0)/f.GSD - 0.5, (f.N0-n)/f.GSD - 0.5
}

// Contains reports whether local metres (e, n) fall inside the layers.
func (f Frame) Contains(e, n float64) bool {
	return e >= f.E0 && e < f.E0+float64(f.W)*f.GSD && n <= f.N0 && n > f.N0-float64(f.H)*f.GSD
}

// SiteFrame returns the layer frame of a site: the footprint plus the margin,
// with width and height rounded up to multiples of 4 pixels (for mip levels).
func SiteFrame(s *scenario.Scenario, st scenario.Site) Frame {
	margin := math.Max(s.MarginM, st.SpillM+1)
	b := st.Poly().Bounds().Grow(margin)
	g := s.GSDm
	w := int(math.Ceil((b.Max.X-b.Min.X)/g/4)) * 4
	h := int(math.Ceil((b.Max.Y-b.Min.Y)/g/4)) * 4
	// Snap the corner to the pixel grid of the whole incident so that
	// neighbouring renders share pixel boundaries.
	e0 := math.Floor(b.Min.X/g) * g
	n0 := math.Ceil(b.Max.Y/g) * g
	return Frame{E0: e0, N0: n0, GSD: g, W: w, H: h}
}

// Layers is a rendered rectangle of a site.
//
//	RGB      surface colour (albedo with baked ambient occlusion)
//	Height   millimetres above local ground
//	Parts    body-part class (scenario.PartHand ...), 0 = none
//	Victim   victim index that owns the pixel's warmth (body, or debris touching a body)
//	Coupling 0..255: share of the victim's skin temperature the surface shows
//	Absorb   0..255: solar absorptivity
//	TBase    heat offset above air, quarter degrees C (hot debris, decoys)
type Layers struct {
	Rect     raster.Rect
	RGB      *raster.RGB
	Height   *raster.U16
	Parts    *raster.U8
	Victim   *raster.U8
	Coupling *raster.U8
	Absorb   *raster.U8
	TBase    *raster.U8
}

// LayerFiles are the files every layer directory contains.
var LayerFiles = []string{"rgb.png", "height.png", "parts.png", "victim.png", "coupling.png", "absorb.png", "tbase.png"}

// NewLayers allocates empty layers for a rectangle.
func NewLayers(r raster.Rect) *Layers {
	w, h := r.W(), r.H()
	return &Layers{Rect: r, RGB: raster.NewRGB(w, h), Height: raster.NewU16(w, h), Parts: raster.NewU8(w, h),
		Victim: raster.NewU8(w, h), Coupling: raster.NewU8(w, h), Absorb: raster.NewU8(w, h), TBase: raster.NewU8(w, h)}
}

// Save writes the layer PNGs and returns their SHA-256 sums.
func (l *Layers) Save(dir string) (map[string]string, error) {
	imgs := map[string]func() error{
		"rgb.png":      func() error { return raster.SavePNG(filepath.Join(dir, "rgb.png"), l.RGB.Image()) },
		"height.png":   func() error { return raster.SavePNG(filepath.Join(dir, "height.png"), l.Height.Image()) },
		"parts.png":    func() error { return raster.SavePNG(filepath.Join(dir, "parts.png"), l.Parts.Image()) },
		"victim.png":   func() error { return raster.SavePNG(filepath.Join(dir, "victim.png"), l.Victim.Image()) },
		"coupling.png": func() error { return raster.SavePNG(filepath.Join(dir, "coupling.png"), l.Coupling.Image()) },
		"absorb.png":   func() error { return raster.SavePNG(filepath.Join(dir, "absorb.png"), l.Absorb.Image()) },
		"tbase.png":    func() error { return raster.SavePNG(filepath.Join(dir, "tbase.png"), l.TBase.Image()) },
	}
	sums := map[string]string{}
	for _, name := range LayerFiles {
		if err := imgs[name](); err != nil {
			return nil, diag.Wrap("E-RND-001", err, "writing %s", filepath.Join(dir, name))
		}
		sum, err := raster.FileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, diag.Wrap("E-RND-001", err, "hashing %s", name)
		}
		sums[name] = sum
	}
	return sums, nil
}

// LoadLayers reads a layer directory. With sums != nil every file's SHA-256
// is checked first.
func LoadLayers(dir string, r raster.Rect, sums map[string]string) (*Layers, error) {
	for _, name := range LayerFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			return nil, diag.Wrap("E-RND-002", err, "layer missing")
		}
		if sums != nil {
			got, err := raster.FileSHA256(p)
			if err != nil {
				return nil, diag.Wrap("E-RND-003", err, "hashing %s", p)
			}
			if got != sums[name] {
				return nil, diag.New("E-RND-003", "%s: sha256 %s, site.json expects %s", p, got[:12], short(sums[name]))
			}
		}
	}
	l := &Layers{Rect: r}
	var err error
	if l.RGB, err = raster.LoadRGB(filepath.Join(dir, "rgb.png")); err != nil {
		return nil, diag.Wrap("E-RND-003", err, "decoding")
	}
	if l.Height, err = raster.LoadU16(filepath.Join(dir, "height.png")); err != nil {
		return nil, diag.Wrap("E-RND-003", err, "decoding")
	}
	u8 := []struct {
		name string
		dst  **raster.U8
	}{{"parts.png", &l.Parts}, {"victim.png", &l.Victim}, {"coupling.png", &l.Coupling}, {"absorb.png", &l.Absorb}, {"tbase.png", &l.TBase}}
	for _, f := range u8 {
		if *f.dst, err = raster.LoadU8(filepath.Join(dir, f.name)); err != nil {
			return nil, diag.Wrap("E-RND-003", err, "decoding")
		}
	}
	for _, sz := range [][2]int{{l.Height.W, l.Height.H}, {l.Parts.W, l.Parts.H}, {l.Victim.W, l.Victim.H},
		{l.Coupling.W, l.Coupling.H}, {l.Absorb.W, l.Absorb.H}, {l.TBase.W, l.TBase.H}} {
		if sz[0] != l.RGB.W || sz[1] != l.RGB.H || sz[0] != r.W() || sz[1] != r.H() {
			return nil, diag.New("E-RND-003", "%s: layer sizes disagree (%dx%d, rgb %dx%d, site.json %dx%d)", dir, sz[0], sz[1], l.RGB.W, l.RGB.H, r.W(), r.H())
		}
	}
	return l, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(nothing)"
	}
	return s
}

// Patch holds the alternate pose of a moving victim.
type Patch struct {
	Victim int               `json:"victim"`
	X0     int               `json:"x0"`
	Y0     int               `json:"y0"`
	X1     int               `json:"x1"`
	Y1     int               `json:"y1"`
	Dir    string            `json:"dir"`
	Files  map[string]string `json:"sha256"`
}

// Rect returns the patch rectangle in site pixels.
func (p Patch) Rect() raster.Rect { return raster.Rect{X0: p.X0, Y0: p.Y0, X1: p.X1, Y1: p.Y1} }

// TruthPart is the ground truth for one body-part class of one victim.
type TruthPart struct {
	Victim    int     `json:"victim"`
	VictimID  string  `json:"victim_id"`
	Class     int     `json:"class"`
	ClassName string  `json:"class_name"`
	Pixels    int     `json:"pixels"`
	AreaM2    float64 `json:"area_m2"`
	E         float64 `json:"e_m"`
	N         float64 `json:"n_m"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	HeightM   float64 `json:"height_m"`
}

// TruthVictim summarises one victim's visible body.
type TruthVictim struct {
	Victim       int     `json:"victim"`
	ID           string  `json:"id"`
	Pixels       int     `json:"pixels"`
	AreaM2       float64 `json:"area_m2"`
	VisibleFrac  float64 `json:"visible_frac"` // body pixels left after occluding debris
	E            float64 `json:"e_m"`
	N            float64 `json:"n_m"`
	Lat          float64 `json:"lat"`
	Lon          float64 `json:"lon"`
	PatchVictim  bool    `json:"has_pose_b"`
	ClassesShown []int   `json:"classes"`
}

// Decoy is a deliberately confusing object.
type Decoy struct {
	Kind  string  `json:"kind"` // metal_sheet (sun-heated), hot_spot (smouldering), skin_like (looks like skin, cold)
	E     float64 `json:"e_m"`
	N     float64 `json:"n_m"`
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	SizeM float64 `json:"size_m"`
}

// Manifest is site.json.
type Manifest struct {
	Schema        int               `json:"schema"`
	Renderer      string            `json:"renderer"`
	ScenarioID    string            `json:"scenario_id"`
	ScenarioHash  string            `json:"scenario_hash"`
	SiteID        string            `json:"site_id"`
	Frame         Frame             `json:"frame"`
	Files         map[string]string `json:"sha256"`
	Patches       []Patch           `json:"patches"`
	Truth         []TruthPart       `json:"truth"`
	Victims       []TruthVictim     `json:"victims"`
	Decoys        []Decoy           `json:"decoys"`
	RenderSeconds float64           `json:"render_seconds"`
}

// SiteDir is where a site's layers live inside a scenario folder.
func SiteDir(scenarioDir, siteID string) string { return filepath.Join(scenarioDir, "sites", siteID) }

// SaveManifest writes site.json.
func SaveManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "site.json")
	if err := os.WriteFile(p+".tmp", append(b, '\n'), 0o644); err != nil {
		return diag.Wrap("E-RND-001", err, "writing %s", p)
	}
	return os.Rename(p+".tmp", p)
}

// LoadManifest reads site.json and checks it belongs to the scenario.
func LoadManifest(dir string, s *scenario.Scenario) (*Manifest, error) {
	p := filepath.Join(dir, "site.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, diag.Wrap("E-RND-002", err, "no rendered layers")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, diag.Wrap("E-RND-003", err, "parsing %s", p)
	}
	if s != nil {
		if m.ScenarioHash != s.Hash() {
			return nil, diag.New("E-RND-002", "%s was rendered from scenario %s, but scenario.json is now %s", p, short(m.ScenarioHash), short(s.Hash()))
		}
		if m.Renderer != RendererVersion {
			return nil, diag.New("E-RND-002", "%s was made by renderer %s; this build is %s", p, m.Renderer, RendererVersion)
		}
	}
	return &m, nil
}

// Site is a loaded site: manifest, base layers and pose-B patches.
type Site struct {
	Manifest *Manifest
	Base     *Layers
	Patches  map[int]*Layers // by victim index
}

// LoadSite loads a site's layers; verify checks every file's SHA-256.
func LoadSite(scenarioDir string, s *scenario.Scenario, siteID string, verify bool) (*Site, error) {
	dir := SiteDir(scenarioDir, siteID)
	m, err := LoadManifest(dir, s)
	if err != nil {
		return nil, err
	}
	sums := func(f map[string]string) map[string]string {
		if verify {
			return f
		}
		return nil
	}
	base, err := LoadLayers(dir, raster.Rect{X1: m.Frame.W, Y1: m.Frame.H}, sums(m.Files))
	if err != nil {
		return nil, err
	}
	out := &Site{Manifest: m, Base: base, Patches: map[int]*Layers{}}
	for _, p := range m.Patches {
		l, err := LoadLayers(filepath.Join(dir, p.Dir), p.Rect(), sums(p.Files))
		if err != nil {
			return nil, err
		}
		out.Patches[p.Victim] = l
	}
	return out, nil
}

// String describes a frame briefly.
func (f Frame) String() string {
	return fmt.Sprintf("%dx%d px @ %.1f cm", f.W, f.H, f.GSD*100)
}
