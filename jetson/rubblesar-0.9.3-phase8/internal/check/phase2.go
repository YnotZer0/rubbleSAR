package check

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"rubblesar/internal/dataset"
	"rubblesar/internal/evalseg"
	"rubblesar/internal/infer"
	"rubblesar/internal/vcam"
)

// Phase2Options configures the phase 2 acceptance test.
type Phase2Options struct {
	Role        string // zbook | jetson
	ModelDir    string
	ScenarioDir string
	Quick       bool   // evaluate every third test frame
	SaveEval    string // where to write the evaluation JSON ("" = report folder only)
}

// Phase 2 limits. Accuracy is reported for agreement (plan, phase 2); the
// FAIL limits only catch a model that has clearly not learned.
const (
	p2FailRecallLenient   = 0.30 // all views
	p2FailInvNadirLenient = 0.50
	p2WarnInvNadirStrict  = 0.70
	p2WarnPrecisionLen    = 0.50
	p2WarnVictimsFound    = 0.70
	p2WarnPeopleCloseUp   = 0.85 // people found in investigator views
	p2JetsonAgreement     = 0.05
	p2JetsonPassMS        = 250.0
	p2JetsonWarnMS        = 600.0
	p2UltraMatchRate      = 0.90
)

var p2Names = map[string]map[string]string{
	"zbook": {
		"P2-01": "model folder",
		"P2-02": "ONNX Runtime runs the model",
		"P2-03": "Go decoding = Ultralytics",
		"P2-04": "accuracy on held-out frames",
		"P2-05": "victims found after geolocation",
	},
	"jetson": {
		"P2-01": "model folder and engine",
		"P2-02": "TensorRT runs the engine",
		"P2-03": "TensorRT = ZBook reference",
		"P2-04": "accuracy on held-out frames",
		"P2-05": "inference speed",
	},
}

// Phase2 runs the model acceptance test for one machine.
func Phase2(o Phase2Options, out io.Writer) *Report {
	r := NewReport("phase2-"+o.Role, out)
	r.Share = true // accuracy and speed are to be reviewed and agreed, pass or not
	names := p2Names[o.Role]
	r.Context["role"] = o.Role
	r.Context["model"] = o.ModelDir
	r.Context["scenario"] = o.ScenarioDir
	r.Context["cpus"] = fmt.Sprint(runtime.NumCPU())
	jetson := o.Role == "jetson"
	var card *infer.Card
	res := r.Run("P2-01", names["P2-01"], func() Result {
		return safe("E-CHK-201", func() Result { return checkPackage(o, &card) })
	})
	if res.Status == Fail {
		for _, id := range []string{"P2-02", "P2-03", "P2-04", "P2-05"} {
			r.Skip(id, names[id], "needs a complete model folder (P2-01)")
		}
		return r
	}
	r.Context["model_name"] = card.Name
	var be infer.Backend
	res = r.Run("P2-02", names["P2-02"], func() Result {
		return safe("E-CHK-202", func() Result { return checkBackend(o, &be) })
	})
	if res.Status == Fail || be == nil {
		for _, id := range []string{"P2-03", "P2-04", "P2-05"} {
			r.Skip(id, names[id], "needs a working back end (P2-02)")
		}
		return r
	}
	defer be.Close()
	r.Context["backend"] = be.Name()
	if jetson {
		r.Run("P2-03", names["P2-03"], func() Result { return safe("E-CHK-203", func() Result { return checkTRTParity(o, be) }) })
	} else {
		r.Run("P2-03", names["P2-03"], func() Result { return safe("E-CHK-203", func() Result { return checkUltraParity(o) }) })
	}
	var rep *evalseg.Report
	r.Run("P2-04", names["P2-04"], func() Result {
		return safe("E-CHK-204", func() Result { return checkAccuracy(o, be, &rep) })
	})
	if jetson {
		r.Run("P2-05", names["P2-05"], func() Result { return safe("E-CHK-206", func() Result { return checkSpeed2(o, be) }) })
	} else if rep != nil {
		r.Run("P2-05", names["P2-05"], func() Result { return safe("E-CHK-205", func() Result { return checkGeo(rep) }) })
	} else {
		r.Skip("P2-05", names["P2-05"], "needs the evaluation (P2-04)")
	}
	return r
}

func checkPackage(o Phase2Options, out **infer.Card) Result {
	card, err := infer.LoadCard(o.ModelDir)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-201", Detail: "model.json unusable", Evidence: []string{err.Error()}}
	}
	*out = card
	var ev []string
	if err := card.VerifyONNX(o.ModelDir); err != nil {
		ev = append(ev, err.Error())
	}
	for _, f := range []string{filepath.Join(infer.GoldenDir, "golden.json"), infer.TestSetFile} {
		if _, err := os.Stat(filepath.Join(o.ModelDir, f)); err != nil {
			ev = append(ev, fmt.Sprintf("%s missing (made by scripts/zbook/82-evaluate.sh)", f))
		}
	}
	detail := fmt.Sprintf("%s (%s, %d epochs, trained %s on %s); val mask mAP50 %.3f", card.Name, card.Arch, card.Epochs, card.TrainedAt, card.GPU, card.Metrics["val.all.mask.map50"])
	if o.Role == "jetson" {
		ei, err := infer.LoadEngineInfo(o.ModelDir)
		switch {
		case err != nil:
			ev = append(ev, err.Error())
		case ei.ONNXSHA256 != card.ONNXSHA256:
			ev = append(ev, "model.engine was built from a different model.onnx; re-run scripts/jetson/80-engine.sh")
		default:
			detail += fmt.Sprintf("; engine %s %s, max batch %d", ei.TensorRT, ei.Precision, ei.MaxBatch)
		}
		if _, err := os.Stat(filepath.Join(o.ModelDir, infer.EvalZBook)); err != nil {
			ev = append(ev, infer.EvalZBook+" missing: copy the whole model folder from the ZBook")
		}
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-201", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail}
}

func checkBackend(o Phase2Options, out *infer.Backend) Result {
	var b infer.Backend
	var err error
	if o.Role == "jetson" {
		var t *infer.TRT
		t, err = infer.OpenTRT(filepath.Join(o.ModelDir, infer.EngineFile))
		if err == nil {
			b = t
		}
	} else {
		b, _, err = infer.Open(filepath.Join(o.ModelDir, "model.onnx"), 6)
	}
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-202", Detail: "cannot load the model", Evidence: []string{err.Error()}}
	}
	// One blank tile through the network: shapes and timing.
	s := b.InputSize()
	in := make([]float32, 3*s*s)
	t0 := time.Now()
	o0, err := b.Infer(in, 1)
	if err == nil {
		err = o0.Check(len(infer.ClassNames))
	}
	if err != nil {
		b.Close()
		return Result{Status: Fail, Code: "E-CHK-202", Detail: "the model ran but its output is wrong", Evidence: []string{err.Error()}}
	}
	*out = b
	return Result{Status: Pass, Detail: fmt.Sprintf("%s: input %dx%d, max batch %d, outputs %dx%d and %dx%dx%d, first tile %.0f ms",
		b.Name(), s, s, b.MaxBatch(), o0.C, o0.A, o0.NM, o0.MH, o0.MW, float64(time.Since(t0).Microseconds())/1000)}
}

func goldenDets(g *infer.Golden) [][]infer.GoldenDet {
	var out [][]infer.GoldenDet
	for _, t := range g.Tiles {
		out = append(out, t.Detections)
	}
	return out
}

func parityResult(p *infer.Parity, ok bool, what string) Result {
	detail := fmt.Sprintf("%s: %d/%d reference detections matched (%d borderline), median box IoU %.3f, max confidence difference %.3f",
		what, p.Matched, p.Reference-p.Borderline, p.Borderline, p.MedianBoxIoU, p.MaxConfDiff)
	if p.MaxScoreDiff > 0 {
		detail += fmt.Sprintf(", max raw score difference %.3f", p.MaxScoreDiff)
	}
	if !ok {
		return Result{Status: Fail, Code: "E-CHK-203", Detail: detail, Evidence: limit(p.Problems, 10)}
	}
	if len(p.Problems) > 0 {
		return Result{Status: Pass, Detail: detail + fmt.Sprintf(" (%d small differences)", len(p.Problems))}
	}
	return Result{Status: Pass, Detail: detail}
}

// checkUltraParity compares the Go (ONNX Runtime) reference detections with
// Ultralytics' own predictions on the same golden tiles.
func checkUltraParity(o Phase2Options) Result {
	g, _, err := infer.LoadGolden(filepath.Join(o.ModelDir, infer.GoldenDir))
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-203", Detail: "golden set unusable", Evidence: []string{err.Error()}}
	}
	up := filepath.Join(o.ModelDir, infer.GoldenDir, infer.UltraPreds)
	u, err := LoadUltra(up)
	if err != nil {
		return Result{Status: Warn, Code: "E-CHK-203", Detail: "no Ultralytics predictions to compare with", Evidence: []string{err.Error()},
			Hint: "scripts/zbook/82-evaluate.sh asks the training container for them (needs Docker and the image built by 81-train.sh)"}
	}
	var cand [][]infer.GoldenDet
	for _, t := range g.Tiles {
		cand = append(cand, u[t.File])
	}
	p := infer.CompareDets(goldenDets(g), cand, g.Params.Conf)
	ok := p.MatchRate >= p2UltraMatchRate && (p.Matched == 0 || p.MedianBoxIoU >= 0.85)
	return parityResult(p, ok, fmt.Sprintf("%d golden tiles", len(g.Tiles)))
}

// LoadUltra reads ultralytics.json (per-tile detections from rubblesar_train.py predict).
func LoadUltra(path string) (map[string][]infer.GoldenDet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tiles []struct {
			File       string            `json:"file"`
			Detections []infer.GoldenDet `json:"detections"`
		} `json:"tiles"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	out := map[string][]infer.GoldenDet{}
	for _, t := range doc.Tiles {
		out[t.File] = t.Detections
	}
	return out, nil
}

func checkTRTParity(o Phase2Options, be infer.Backend) Result {
	g, tl, err := infer.LoadGolden(filepath.Join(o.ModelDir, infer.GoldenDir))
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-203", Detail: "golden set unusable", Evidence: []string{err.Error()}}
	}
	p, err := infer.CheckParity(g, tl, be)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-203", Detail: "inference on the golden tiles failed", Evidence: []string{err.Error()}}
	}
	return parityResult(p, p.ParityOK(), fmt.Sprintf("%d golden tiles vs %s", len(g.Tiles), g.Backend))
}

// RunEvaluation evaluates a backend on the test frames listed in the model folder.
// conf is the detection confidence threshold (0 = the default, 0.25).
func RunEvaluation(modelDir, scenarioDir string, be infer.Backend, quick bool, overlays string, conf float64, progress func(string)) (*evalseg.Report, error) {
	ts, err := dataset.LoadTestSetFile(filepath.Join(modelDir, infer.TestSetFile))
	if err != nil {
		return nil, err
	}
	w, err := vcam.Open(scenarioDir, false)
	if err != nil {
		return nil, err
	}
	if err := ts.CheckScenario(w.Scn); err != nil {
		return nil, err
	}
	frames := ts.Frames
	if quick {
		var q []dataset.TestFrame
		for i, f := range frames {
			if i%3 == 0 {
				q = append(q, f)
			}
		}
		frames = q
	}
	opt := evalseg.DefaultOptions()
	opt.Overlays = overlays
	p := infer.DefaultParams()
	if conf > 0 {
		p.Conf = conf
	}
	return evalseg.Evaluate(w, frames, &infer.Detector{B: be, P: p}, opt, progress)
}

func kind(rep *evalseg.Report, k string) evalseg.KindReport {
	for _, x := range rep.Kinds {
		if x.Kind == k {
			return x
		}
	}
	return evalseg.KindReport{Kind: k}
}

func checkAccuracy(o Phase2Options, be infer.Backend, out **evalseg.Report) Result {
	rep, err := RunEvaluation(o.ModelDir, o.ScenarioDir, be, o.Quick, "", 0, nil)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-204", Detail: "evaluation could not run", Evidence: []string{err.Error()}}
	}
	*out = rep
	if o.SaveEval != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(o.SaveEval, b, 0o644)
	}
	all := rep.Overall
	var detail string
	var ev []string
	mInvL, mAllL, mInvS, mPrecL := 0.0, 0.0, 0.0, 0.0
	people := evalseg.VictimReport{}
	if m := rep.Merged; m != nil {
		inv := m.Kind("investigator-nadir")
		pi, ps := m.VictimsPrefix("investigator"), m.VictimsPrefix("scout")
		people = pi
		mInvL, mAllL, mInvS, mPrecL = inv.RecallLenient, m.Overall.RecallLenient, inv.Recall, m.Overall.PrecisionLenient
		detail = fmt.Sprintf("%d frames, each visible part counted once: mask AP50 %.3f, recall %.3f strict / %.3f lenient, precision %.3f lenient; close-up (investigator nadir) recall %.3f / %.3f; people found: close-ups %d of %d, scout %d of %d",
			rep.Frames, m.Overall.AP50, m.Overall.Recall, m.Overall.RecallLenient, m.Overall.PrecisionLenient, inv.Recall, inv.RecallLenient, pi.Found, pi.Visible, ps.Found, ps.Visible)
		for _, c := range m.Classes {
			ev = append(ev, fmt.Sprintf("%-6s truth %4d  AP50 %.3f  recall %.3f / %.3f  precision %.3f", c.Class, c.Truth, c.AP50, c.Recall, c.RecallLenient, c.PrecisionLenient))
		}
		for _, k := range m.Kinds {
			v := m.VictimsFor(k.Kind)
			ev = append(ev, fmt.Sprintf("%-21s %2d frames  recall %.3f / %.3f  false alarms per frame %.2f  people %d of %d", k.Kind, k.Frames, k.Recall, k.RecallLenient, k.FalsePerFrame, v.Found, v.Visible))
		}
		for _, sz := range m.Sizes {
			ev = append(ev, fmt.Sprintf("size %-10s %4d parts: found %3.0f%%, right class %3.0f%%", sz.Band, sz.Truth, sz.RecallAny*100, sz.RecallClass*100))
		}
		ev = append(ev, m.Confusion.Rows()...)
		ev = append(ev, fmt.Sprintf("piece by piece (phase 2.0 method): AP50 %.3f, recall %.3f / %.3f, precision %.3f", all.AP50, all.Recall, all.RecallLenient, all.PrecisionLenient))
	} else { // not produced by this version; keep the phase 2.0 view
		inv := kind(rep, "investigator-nadir")
		mInvL, mAllL, mInvS, mPrecL = inv.RecallLenient, all.RecallLenient, inv.Recall, all.PrecisionLenient
		detail = fmt.Sprintf("%d frames: mask AP50 %.3f, recall %.3f strict / %.3f lenient, precision %.3f lenient", rep.Frames, all.AP50, all.Recall, all.RecallLenient, all.PrecisionLenient)
	}
	if o.SaveEval != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(o.SaveEval, b, 0o644)
	}
	if o.Role == "jetson" {
		z, err := loadEval(filepath.Join(o.ModelDir, infer.EvalZBook))
		if err != nil {
			return Result{Status: Warn, Code: "E-CHK-204", Detail: detail, Evidence: append([]string{"cannot compare with the ZBook: " + err.Error()}, ev...)}
		}
		// The piece-by-piece numbers exist in every ZBook evaluation, so they are always compared.
		zr, jr := z.Overall.RecallLenient, all.RecallLenient
		za, ja := z.Overall.AP50, all.AP50
		detail += fmt.Sprintf("; ZBook (piece by piece): AP50 %.3f, recall %.3f lenient, Jetson %.3f / %.3f", za, zr, ja, jr)
		if o.Quick {
			return Result{Status: Pass, Detail: detail + " (quick run: subset, not compared)", Evidence: ev}
		}
		diff := math.Max(math.Abs(zr-jr), math.Abs(za-ja))
		if z.Merged != nil && rep.Merged != nil {
			diff = math.Max(diff, math.Max(math.Abs(z.Merged.Overall.AP50-rep.Merged.Overall.AP50), math.Abs(z.Merged.Overall.RecallLenient-rep.Merged.Overall.RecallLenient)))
		}
		if diff > p2JetsonAgreement {
			return Result{Status: Warn, Code: "E-CHK-203", Detail: detail, Evidence: append([]string{fmt.Sprintf("Jetson and ZBook differ by %.3f (more than %.2f)", diff, p2JetsonAgreement)}, ev...)}
		}
		return Result{Status: Pass, Detail: detail, Evidence: ev}
	}
	switch {
	case mAllL < p2FailRecallLenient || mInvL < p2FailInvNadirLenient:
		return Result{Status: Fail, Code: "E-CHK-204", Detail: detail, Evidence: ev}
	case mInvS < p2WarnInvNadirStrict, mPrecL < p2WarnPrecisionLen, people.Visible > 0 && people.Recall < p2WarnPeopleCloseUp:
		return Result{Status: Warn, Code: "E-CHK-204", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

func loadEval(p string) (*evalseg.Report, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r evalseg.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func checkGeo(rep *evalseg.Report) Result {
	g := rep.Geo
	if g == nil || g.VictimsTotal == 0 {
		return Result{Status: Warn, Code: "E-CHK-205", Detail: "no victims in view"}
	}
	frac := float64(g.VictimsFound) / float64(g.VictimsTotal)
	detail := fmt.Sprintf("reported pose, flat ground: %d/%d victims found, part precision %.2f, median error %.2f m (p95 %.2f m); with the true pose %d/%d, median %.2f m",
		g.VictimsFound, g.VictimsTotal, g.Overall.Precision, g.ErrMedianM, g.ErrP95M, rep.GeoTrue.VictimsFound, rep.GeoTrue.VictimsTotal, rep.GeoTrue.ErrMedianM)
	if frac < p2WarnVictimsFound || g.Overall.Precision < 0.3 {
		return Result{Status: Warn, Code: "E-CHK-205", Detail: detail, Evidence: []string{"missed: " + fmt.Sprint(g.VictimsMissed),
			fmt.Sprintf("%d geolocated detections were more than %.0f m from any part of their class", g.Overall.FP, g.RadiusM)}}
	}
	return Result{Status: Pass, Detail: detail}
}

// checkSpeed2 times whole 1080p frames (6 tiles) through the engine.
func checkSpeed2(o Phase2Options, be infer.Backend) Result {
	w, err := vcam.Open(o.ScenarioDir, false)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-206", Detail: err.Error()}
	}
	st := w.Scn.Sites[0]
	f, err := w.Render(vcam.Request{UAV: "check", Pose: vcam.Pose{E: st.CenterE, N: st.CenterN, AltAGL: 40, PitchDeg: -90}, NoTherm: true})
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-206", Detail: err.Error()}
	}
	d := &infer.Detector{B: be, P: infer.DefaultParams()}
	var tot, inf []float64
	for i := 0; i < 12; i++ {
		_, tm, err := d.DetectFrame(f.RGB)
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-206", Detail: err.Error()}
		}
		if i >= 2 { // warm-up
			tot = append(tot, tm.TotalMS())
			inf = append(inf, tm.InferMS)
		}
	}
	sort.Float64s(tot)
	sort.Float64s(inf)
	med, medInf := tot[len(tot)/2], inf[len(inf)/2]
	detail := fmt.Sprintf("1080p frame (6 tiles, batch %d): median %.0f ms (network %.0f ms) = %.1f frames/s; pass <= %.0f ms",
		be.MaxBatch(), med, medInf, 1000/med, p2JetsonPassMS)
	switch {
	case med > p2JetsonWarnMS:
		return Result{Status: Fail, Code: "E-CHK-206", Detail: detail}
	case med > p2JetsonPassMS:
		return Result{Status: Warn, Code: "E-CHK-206", Detail: detail}
	}
	return Result{Status: Pass, Detail: detail}
}
