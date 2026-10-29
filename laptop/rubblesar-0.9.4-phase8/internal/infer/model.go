package infer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"rubblesar/internal/diag"
	"rubblesar/internal/raster"
)

// Card is model.json, written by the training kit next to model.onnx.
type Card struct {
	Schema       int      `json:"schema"`
	Name         string   `json:"name"`
	Arch         string   `json:"arch"`       // e.g. yolo11s-seg
	Classes      []string `json:"classes"`    // must equal ClassNames
	InputSize    int      `json:"input_size"` // 640
	ONNX         string   `json:"onnx"`       // file name
	ONNXSHA256   string   `json:"onnx_sha256"`
	Dataset      string   `json:"dataset"`
	Epochs       int      `json:"epochs"`
	Ultralytics  string   `json:"ultralytics_version"`
	Torch        string   `json:"torch_version"`
	GPU          string   `json:"gpu"`
	TrainedAt    string   `json:"trained_at"`
	TrainMinutes float64  `json:"train_minutes"`
	// Metrics from Ultralytics validation, keyed "<split>.<class|all>.<mask|box>.<map50|map50_95|p|r>".
	Metrics map[string]float64 `json:"metrics"`
	Notes   string             `json:"notes"`
}

// LoadCard reads model.json from a model folder.
func LoadCard(dir string) (*Card, error) {
	p := filepath.Join(dir, "model.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, diag.Wrap("E-INF-001", err, "model card")
	}
	var c Card
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, diag.Wrap("E-INF-001", err, "parsing %s", p)
	}
	if strings.Join(c.Classes, ",") != strings.Join(ClassNames, ",") {
		return nil, diag.New("E-INF-003", "%s lists classes %v; RubbleSAR expects %v", p, c.Classes, ClassNames)
	}
	if c.InputSize < 32 || c.InputSize%32 != 0 {
		return nil, diag.New("E-INF-003", "%s: input size %d", p, c.InputSize)
	}
	return &c, nil
}

// VerifyONNX checks the ONNX file against the card's checksum.
func (c *Card) VerifyONNX(dir string) error {
	p := filepath.Join(dir, c.ONNX)
	got, err := raster.FileSHA256(p)
	if err != nil {
		return diag.Wrap("E-INF-001", err, "model file")
	}
	if got != c.ONNXSHA256 {
		return diag.New("E-INF-001", "%s: sha256 %s, model.json expects %s (copied incompletely?)", p, got[:12], short(c.ONNXSHA256))
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// EngineInfo is engine.json, written by scripts/jetson/80-engine.sh.
type EngineInfo struct {
	Engine     string `json:"engine"`
	ONNXSHA256 string `json:"onnx_sha256"`
	TensorRT   string `json:"tensorrt"`
	Precision  string `json:"precision"`
	MaxBatch   int    `json:"max_batch"`
	BuiltAt    string `json:"built_at"`
	Device     string `json:"device"`
}

// Open picks the backend for a model folder or file: a TensorRT engine
// (model.engine) when this build has TensorRT and one exists, else the ONNX
// model with ONNX Runtime. maxBatch applies to ONNX Runtime.
func Open(path string, maxBatch int) (Backend, *Card, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, nil, diag.Wrap("E-INF-001", err, "model")
	}
	dir, file := path, ""
	if !fi.IsDir() {
		dir, file = filepath.Dir(path), filepath.Base(path)
	}
	card, cerr := LoadCard(dir)
	switch {
	case strings.HasSuffix(file, ".engine") || strings.HasSuffix(file, ".plan") || (file == "" && TRTAvailable && exists(filepath.Join(dir, "model.engine"))):
		if file == "" {
			file = "model.engine"
		}
		b, err := OpenTRT(filepath.Join(dir, file))
		if err != nil {
			return nil, card, err
		}
		return b, card, nil
	default:
		if cerr != nil {
			return nil, nil, cerr
		}
		if file == "" {
			file = card.ONNX
		}
		b, err := OpenORT(filepath.Join(dir, file), card.InputSize, maxBatch)
		if err != nil {
			return nil, card, err
		}
		return b, card, nil
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// DefaultModelDir is where the setup scripts install the active model.
const DefaultModelDir = "/var/lib/rubblesar/models/current"

// Package file names inside a model folder.
const (
	GoldenDir   = "golden"          // reference tiles and results (golden.json, ultralytics.json)
	TestSetFile = "testset.json"    // held-out test frames (from the dataset)
	EvalZBook   = "eval-zbook.json" // evaluation on the ZBook (ONNX Runtime)
	EngineFile  = "model.engine"    // TensorRT engine (Jetson only)
	EngineJSON  = "engine.json"
	UltraPreds  = "ultralytics.json"
)

// LoadEngineInfo reads engine.json if present.
func LoadEngineInfo(dir string) (*EngineInfo, error) {
	b, err := os.ReadFile(filepath.Join(dir, EngineJSON))
	if err != nil {
		return nil, diag.Wrap("E-INF-001", err, "engine.json (made by scripts/jetson/80-engine.sh)")
	}
	var e EngineInfo
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, diag.Wrap("E-INF-001", err, "parsing engine.json")
	}
	return &e, nil
}
