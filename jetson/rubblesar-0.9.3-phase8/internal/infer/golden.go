package infer

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"rubblesar/internal/diag"
	"rubblesar/internal/raster"
	"rubblesar/internal/tiles"
)

// Golden is golden/golden.json: reference results of the model on a few
// tiles, computed on the ZBook with ONNX Runtime in full precision. The
// Jetson's TensorRT engine is compared against it.
type Golden struct {
	Schema      int          `json:"schema"`
	ModelSHA256 string       `json:"model_sha256"`
	Backend     string       `json:"backend"`
	Params      Params       `json:"params"`
	Size        int          `json:"size"`
	Tiles       []GoldenTile `json:"tiles"`
}

// GoldenTile holds one tile's reference detections and top raw scores.
type GoldenTile struct {
	File       string         `json:"file"`
	Detections []GoldenDet    `json:"detections"`
	Top        []GoldenAnchor `json:"top_anchors"`
}

// GoldenDet is a reference detection (tile pixels).
type GoldenDet struct {
	Class int        `json:"class"`
	Conf  float32    `json:"conf"`
	Box   [4]float32 `json:"box"`
	Area  int        `json:"mask_px"`
	CX    float64    `json:"cx"`
	CY    float64    `json:"cy"`
}

// GoldenAnchor is a raw output column with a high class score.
type GoldenAnchor struct {
	A     int     `json:"anchor"`
	Class int     `json:"class"`
	Score float32 `json:"score"`
	CX    float32 `json:"cx"`
	CY    float32 `json:"cy"`
}

func toGolden(ds []Detection) []GoldenDet {
	out := make([]GoldenDet, 0, len(ds))
	for _, d := range ds {
		out = append(out, GoldenDet{Class: d.Class, Conf: d.Conf, Box: d.Box, Area: d.Area, CX: math.Round(d.CX*100) / 100, CY: math.Round(d.CY*100) / 100})
	}
	return out
}

func topAnchors(o *RawOutput, b, nc, n int) []GoldenAnchor {
	var all []GoldenAnchor
	base := b * o.C * o.A
	for a := 0; a < o.A; a++ {
		best, bc := float32(-1), 0
		for c := 0; c < nc; c++ {
			if s := o.Preds[base+(4+c)*o.A+a]; s > best {
				best, bc = s, c
			}
		}
		all = append(all, GoldenAnchor{A: a, Class: bc, Score: best, CX: o.Preds[base+a], CY: o.Preds[base+o.A+a]})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// runTiles runs single tiles through a backend and returns raw outputs and detections.
func runTiles(b Backend, tl []*raster.RGB, p Params) ([]*RawOutput, [][]Detection, error) {
	size := b.InputSize()
	var raws []*RawOutput
	var dets [][]Detection
	for i, t := range tl {
		if t.W != size || t.H != size {
			return nil, nil, diag.New("E-INF-003", "tile %d is %dx%d, the model takes %dx%d", i, t.W, t.H, size, size)
		}
		in := make([]float32, 3*size*size)
		ToCHW(in, t, tiles.Tile{Size: size})
		o, err := b.Infer(in, 1)
		if err != nil {
			return nil, nil, err
		}
		if err := o.Check(p.NumClasses); err != nil {
			return nil, nil, err
		}
		raws = append(raws, o)
		dets = append(dets, Decode(o, 0, p, size))
	}
	return raws, dets, nil
}

// MakeGolden computes the reference set with a (full-precision) backend.
func MakeGolden(b Backend, tl []*raster.RGB, names []string, p Params, modelSHA string) (*Golden, error) {
	raws, dets, err := runTiles(b, tl, p)
	if err != nil {
		return nil, err
	}
	g := &Golden{Schema: 1, ModelSHA256: modelSHA, Backend: b.Name(), Params: p, Size: b.InputSize()}
	for i := range tl {
		g.Tiles = append(g.Tiles, GoldenTile{File: names[i], Detections: toGolden(dets[i]), Top: topAnchors(raws[i], 0, p.NumClasses, 50)})
	}
	return g, nil
}

// Save writes golden.json and the tiles (PNG, lossless) into dir.
func (g *Golden) Save(dir string, tl []*raster.RGB) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return diag.Wrap("E-RND-001", err, "creating %s", dir)
	}
	for i, t := range tl {
		if err := raster.SavePNG(filepath.Join(dir, g.Tiles[i].File), t.Image()); err != nil {
			return diag.Wrap("E-RND-001", err, "writing tile")
		}
	}
	b, _ := json.MarshalIndent(g, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "golden.json"), b, 0o644); err != nil {
		return diag.Wrap("E-RND-001", err, "writing golden.json")
	}
	return nil
}

// LoadGolden reads a golden folder.
func LoadGolden(dir string) (*Golden, []*raster.RGB, error) {
	b, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if err != nil {
		return nil, nil, diag.Wrap("E-INF-001", err, "golden set (made on the ZBook by scripts/zbook/82-evaluate.sh)")
	}
	var g Golden
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, nil, diag.Wrap("E-INF-001", err, "parsing golden.json")
	}
	var tl []*raster.RGB
	for _, t := range g.Tiles {
		m, err := raster.LoadRGB(filepath.Join(dir, t.File))
		if err != nil {
			return nil, nil, diag.Wrap("E-INF-001", err, "golden tile")
		}
		tl = append(tl, m)
	}
	return &g, tl, nil
}

// Parity summarises how a backend agrees with reference detections.
type Parity struct {
	Tiles          int      `json:"tiles"`
	Reference      int      `json:"reference_detections"`
	Candidate      int      `json:"candidate_detections"`
	Matched        int      `json:"matched"`
	Borderline     int      `json:"borderline"` // unmatched but within 0.05 of the score threshold
	MatchRate      float64  `json:"match_rate"`
	MedianBoxIoU   float64  `json:"median_box_iou"`
	MaxConfDiff    float64  `json:"max_conf_diff"`
	MedianAreaDiff float64  `json:"median_mask_area_diff"` // relative
	MaxScoreDiff   float64  `json:"max_raw_score_diff"`    // over the top reference anchors
	Problems       []string `json:"problems"`
}

// CompareDets matches candidate detections to reference ones (same class, box IoU >= 0.7).
func CompareDets(ref [][]GoldenDet, cand [][]GoldenDet, conf float64) *Parity {
	p := &Parity{Tiles: len(ref), Problems: []string{}}
	var ious, areas []float64
	for t := range ref {
		used := make([]bool, len(cand[t]))
		p.Reference += len(ref[t])
		p.Candidate += len(cand[t])
		for _, r := range ref[t] {
			best, bi := 0.0, -1
			for j, c := range cand[t] {
				if used[j] || c.Class != r.Class {
					continue
				}
				if v := iou(r.Box, c.Box); v > best {
					best, bi = v, j
				}
			}
			if bi >= 0 && best >= 0.7 {
				used[bi] = true
				p.Matched++
				ious = append(ious, best)
				c := cand[t][bi]
				p.MaxConfDiff = math.Max(p.MaxConfDiff, math.Abs(float64(r.Conf-c.Conf)))
				if r.Area > 0 {
					areas = append(areas, math.Abs(float64(c.Area-r.Area))/float64(r.Area))
				}
				continue
			}
			if float64(r.Conf) < conf+0.05 {
				p.Borderline++
				continue
			}
			p.Problems = append(p.Problems, fmt.Sprintf("tile %d: reference %s %.2f at %.0f,%.0f not found", t, name(r.Class), r.Conf, r.CX, r.CY))
		}
		for j, c := range cand[t] {
			if !used[j] && float64(c.Conf) >= conf+0.05 {
				p.Problems = append(p.Problems, fmt.Sprintf("tile %d: extra %s %.2f at %.0f,%.0f", t, name(c.Class), c.Conf, c.CX, c.CY))
			}
		}
	}
	if n := p.Reference - p.Borderline; n > 0 {
		p.MatchRate = float64(p.Matched) / float64(n)
	} else {
		p.MatchRate = 1
	}
	p.MedianBoxIoU, p.MedianAreaDiff = median(ious), median(areas)
	return p
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// CheckParity runs a backend on the golden tiles and compares.
func CheckParity(g *Golden, tl []*raster.RGB, b Backend) (*Parity, error) {
	raws, dets, err := runTiles(b, tl, g.Params)
	if err != nil {
		return nil, err
	}
	var ref, cand [][]GoldenDet
	maxDiff := 0.0
	for i, t := range g.Tiles {
		ref = append(ref, t.Detections)
		cand = append(cand, toGolden(dets[i]))
		o := raws[i]
		for _, a := range t.Top {
			if a.A < o.A {
				s := o.Preds[(4+a.Class)*o.A+a.A]
				maxDiff = math.Max(maxDiff, math.Abs(float64(s-a.Score)))
			}
		}
	}
	p := CompareDets(ref, cand, g.Params.Conf)
	p.MaxScoreDiff = maxDiff
	return p, nil
}

// ParityOK applies the acceptance limits; FP16 engines differ slightly from FP32.
func (p *Parity) ParityOK() bool {
	return p.MatchRate >= 0.95 && (p.Matched == 0 || p.MedianBoxIoU >= 0.9) && p.MaxConfDiff <= 0.1 && p.MaxScoreDiff <= 0.1
}
