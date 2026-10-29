// Command sar-infer runs and checks the body-part segmentation model.
//
//	sar-infer info     --model DIR|FILE
//	sar-infer bench    --model DIR|FILE [--frames 20] [--scenario DIR]
//	sar-infer frame    --model DIR|FILE --out DIR [--scenario DIR] [--victim vNN | --e M --n M] [--alt 40] [--yaw 0] [--pitch -90] [--t S]
//	sar-infer eval     --model DIR [--scenario DIR] [--json FILE] [--overlays DIR] [--quick] [--conf 0.25]
//	sar-infer package  --export DIR --dataset DIR --out DIR      assemble a model folder from a training run
//	sar-infer golden   --model DIR --dataset DIR [--tiles 12]     reference tiles and results (ZBook, ONNX Runtime)
//	sar-infer parity   --model DIR|FILE [--golden DIR]            compare a back end with the golden results
//	sar-infer stub-model --out DIR                               a stand-in model (fixed outputs) for testing the tools
//	sar-infer version
//
// Exit codes: 0 success, 1 a comparison failed, 2 could not run.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"rubblesar/internal/check"
	"rubblesar/internal/cli"
	"rubblesar/internal/dataset"
	"rubblesar/internal/diag"
	"rubblesar/internal/evalseg"
	"rubblesar/internal/infer"
	"rubblesar/internal/onnxgen"
	"rubblesar/internal/raster"
	"rubblesar/internal/vcam"
	"rubblesar/internal/version"
)

const tool = "sar-infer"

func usage() {
	fmt.Fprintf(os.Stderr, `sar-infer: run and check the body-part segmentation model

  sar-infer info     --model DIR|FILE
  sar-infer bench    --model DIR|FILE [--frames 20] [--scenario DIR]
  sar-infer frame    --model DIR|FILE --out DIR [--scenario DIR] [--victim vNN | --e M --n M] [--alt 40] [--yaw 0] [--pitch -90] [--t S]
  sar-infer eval     --model DIR [--scenario DIR] [--json FILE] [--overlays DIR] [--quick] [--conf 0.25]
  sar-infer package  --export DIR --dataset DIR --out DIR      model folder from a training run (ZBook)
  sar-infer golden   --model DIR --dataset DIR [--tiles 12]     reference tiles and results (ZBook)
  sar-infer parity   --model DIR|FILE [--golden DIR]            compare a back end with the golden results
  sar-infer stub-model --out DIR                                stand-in model with fixed outputs, for testing the tools only
  sar-infer version

--model DIR uses model.engine (TensorRT, Jetson) when present in this build, else model.onnx (ONNX Runtime).
Defaults: --model %s, --scenario /var/lib/rubblesar/scenarios/antakya-demo.
Back ends in this build: ONNX Runtime %v, TensorRT %v.
`, infer.DefaultModelDir, infer.ORTAvailable, infer.TRTAvailable)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(cli.ExitSetup)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	model := fs.String("model", infer.DefaultModelDir, "model folder or file (.onnx / .engine)")
	scen := fs.String("scenario", "/var/lib/rubblesar/scenarios/antakya-demo", "scenario folder")
	out := fs.String("out", "", "output folder")
	frames := fs.Int("frames", 20, "bench: frames to time")
	victim := fs.String("victim", "", "frame: centre on this victim")
	e := fs.Float64("e", math.NaN(), "frame: metres east")
	n := fs.Float64("n", math.NaN(), "frame: metres north")
	alt := fs.Float64("alt", 40, "frame: altitude above ground, m")
	yaw := fs.Float64("yaw", 0, "frame: heading, degrees")
	pitch := fs.Float64("pitch", -90, "frame: gimbal pitch, degrees")
	tOff := fs.Float64("t", 0, "frame: seconds after scenario start")
	jsonOut := fs.String("json", "", "eval/parity: write the result as JSON")
	overlays := fs.String("overlays", "", "eval: folder for overlay pictures")
	quick := fs.Bool("quick", false, "eval: every third test frame")
	conf := fs.Float64("conf", 0, "eval: detection confidence threshold (default 0.25)")
	export := fs.String("export", "", "package: training export folder (model.onnx, model.json)")
	ds := fs.String("dataset", "", "package/golden: dataset folder")
	ntiles := fs.Int("tiles", 12, "golden: tiles")
	golden := fs.String("golden", "", "parity: golden folder (default <model>/golden)")
	switch cmd {
	case "version", "--version":
		fmt.Println(version.String(tool))
		return
	case "help", "-h", "--help":
		usage()
		return
	case "info", "bench", "frame", "eval", "package", "golden", "parity", "stub-model":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(cli.ExitSetup)
	}
	_ = fs.Parse(args)
	fmt.Println(version.String(tool))
	switch cmd {
	case "info":
		info(*model)
	case "bench":
		bench(*model, *scen, *frames)
	case "frame":
		need(*out, "out")
		frameCmd(*model, *scen, *out, *victim, *e, *n, *alt, *yaw, *pitch, *tOff)
	case "eval":
		evalCmd(*model, *scen, *jsonOut, *overlays, *quick, *conf)
	case "package":
		need(*export, "export")
		need(*ds, "dataset")
		need(*out, "out")
		packageCmd(*export, *ds, *out)
	case "golden":
		need(*ds, "dataset")
		goldenCmd(*model, *ds, *ntiles)
	case "stub-model":
		need(*out, "out")
		stubModel(*out)
	case "parity":
		if *golden == "" {
			*golden = filepath.Join(dirOf(*model), infer.GoldenDir)
		}
		parityCmd(*model, *golden, *jsonOut)
	}
}

func need(v, name string) {
	if v == "" {
		fmt.Fprintf(os.Stderr, "--%s is required\n\n", name)
		usage()
		os.Exit(cli.ExitSetup)
	}
}

func fatal(err error, ctx ...string) { cli.Fatal(tool, err, ctx...) }

func dirOf(p string) string {
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return filepath.Dir(p)
}

func open(model string) (infer.Backend, *infer.Card) {
	b, card, err := infer.Open(model, 6)
	if err != nil {
		fatal(err, "model: "+model)
	}
	return b, card
}

func info(model string) {
	b, card := open(model)
	defer b.Close()
	fmt.Printf("back end:   %s\ninput:      %dx%d, max batch %d\n", b.Name(), b.InputSize(), b.InputSize(), b.MaxBatch())
	if card != nil {
		fmt.Printf("model:      %s (%s), %d epochs, trained %s on %s in %.0f min\n", card.Name, card.Arch, card.Epochs, card.TrainedAt, card.GPU, card.TrainMinutes)
		keys := make([]string, 0, len(card.Metrics))
		for k := range card.Metrics {
			if strings.Contains(k, ".all.") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-26s %.3f\n", k, card.Metrics[k])
		}
	}
	if ei, err := infer.LoadEngineInfo(dirOf(model)); err == nil {
		fmt.Printf("engine:     %s %s, max batch %d, built %s on %s\n", ei.TensorRT, ei.Precision, ei.MaxBatch, ei.BuiltAt, ei.Device)
	}
}

func bench(model, scen string, n int) {
	b, _ := open(model)
	defer b.Close()
	w, err := vcam.Open(scen, false)
	if err != nil {
		fatal(err)
	}
	st := w.Scn.Sites[0]
	f, err := w.Render(vcam.Request{UAV: "bench", Pose: vcam.Pose{E: st.CenterE, N: st.CenterN, AltAGL: 40, PitchDeg: -90}, NoTherm: true})
	if err != nil {
		fatal(err)
	}
	d := &infer.Detector{B: b, P: infer.DefaultParams()}
	var tot, inf, pre, post []float64
	for i := 0; i < n+2; i++ {
		ds, tm, err := d.DetectFrame(f.RGB)
		if err != nil {
			fatal(err)
		}
		if i < 2 {
			if i == 0 {
				fmt.Printf("back end %s; %d detections in the bench frame; warming up\n", b.Name(), len(ds))
			}
			continue
		}
		tot, inf, pre, post = append(tot, tm.TotalMS()), append(inf, tm.InferMS), append(pre, tm.PreMS), append(post, tm.PostMS)
	}
	med := func(v []float64) float64 { s := append([]float64(nil), v...); sort.Float64s(s); return s[len(s)/2] }
	fmt.Printf("1080p frame = 6 tiles of %d px, batch %d, %d frames timed\n", b.InputSize(), b.MaxBatch(), n)
	fmt.Printf("median per frame: total %.1f ms (prepare %.1f, network %.1f, decode %.1f) = %.2f frames/s\n",
		med(tot), med(pre), med(inf), med(post), 1000/med(tot))
}

func frameCmd(model, scen, out, victim string, e, n, alt, yaw, pitch, t float64) {
	b, _ := open(model)
	defer b.Close()
	w, err := vcam.Open(scen, false)
	if err != nil {
		fatal(err)
	}
	switch {
	case victim != "":
		found := false
		for _, v := range w.Scn.Victims {
			if v.ID == victim {
				e, n, found = v.E, v.N, true
			}
		}
		if !found {
			fatal(diag.New("E-SCN-003", "no victim %q", victim))
		}
	case math.IsNaN(e) || math.IsNaN(n):
		e, n = w.Scn.Sites[0].CenterE, w.Scn.Sites[0].CenterN
	}
	pose := vcam.Pose{E: e, N: n, AltAGL: alt, YawDeg: yaw, PitchDeg: pitch}
	if pitch > -89.9 {
		back := alt / math.Tan(-pitch*math.Pi/180)
		pose.E -= back * math.Sin(yaw*math.Pi/180)
		pose.N -= back * math.Cos(yaw*math.Pi/180)
	}
	f, err := w.Render(vcam.Request{UAV: "sar-infer", FrameID: "frame", Pose: pose, TOffsetS: t})
	if err != nil {
		fatal(err)
	}
	d := &infer.Detector{B: b, P: infer.DefaultParams()}
	ds, tm, err := d.DetectFrame(f.RGB)
	if err != nil {
		fatal(err)
	}
	if err := f.Save(out); err != nil {
		fatal(err)
	}
	ts := evalseg.TruthInstances(f.PartsRGB, f.InstRGB, 6)
	if err := raster.SavePNG(filepath.Join(out, "detections.png"), evalseg.Overlay(f.RGB, ts, ds)); err != nil {
		fatal(err)
	}
	cam, _ := vcam.New(vcam.DefaultRGB, pose)
	type outDet struct {
		infer.Detection
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	}
	var od []outDet
	fmt.Printf("%d detections (%d labelled parts in view) in %.0f ms (network %.0f ms):\n", len(ds), len(ts), tm.TotalMS(), tm.InferMS)
	for _, x := range ds {
		ge, gn, _ := cam.GroundAt(x.CX, x.CY, 0)
		ll := w.Scn.Local().ToPoint(ge, gn)
		od = append(od, outDet{x, ll.Lat, ll.Lon})
		fmt.Printf("  %-6s %.2f  box %4.0f,%4.0f-%4.0f,%4.0f  mask %5d px  at %.7f, %.7f\n", x.Name, x.Conf, x.Box[0], x.Box[1], x.Box[2], x.Box[3], x.Area, ll.Lat, ll.Lon)
	}
	bj, _ := json.MarshalIndent(od, "", "  ")
	os.WriteFile(filepath.Join(out, "detections.json"), bj, 0o644)
	fmt.Printf("wrote %s/detections.png (white: labelled parts; colour: detections), detections.json and the frame files\n", out)
}

func evalCmd(model, scen, jsonOut, overlays string, quick bool, conf float64) {
	if conf < 0 || conf >= 1 {
		fmt.Fprintf(os.Stderr, "--conf %.2f must be between 0 and 1 (e.g. --conf 0.15)\n", conf)
		os.Exit(cli.ExitSetup)
	}
	b, _ := open(model)
	defer b.Close()
	fmt.Printf("evaluating %s with %s on the held-out frames of %s\n", model, b.Name(), scen)
	rep, err := check.RunEvaluation(dirOf(model), scen, b, quick, overlays, conf, func(s string) { fmt.Println("  " + s) })
	if err != nil {
		fatal(err, "model: "+model, "scenario: "+scen)
	}
	fmt.Println()
	fmt.Print(rep.Text())
	if jsonOut != "" {
		bj, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(jsonOut, append(bj, '\n'), 0o644); err != nil {
			fatal(diag.Wrap("E-RND-001", err, "writing %s", jsonOut))
		}
		fmt.Printf("wrote %s\n", jsonOut)
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func packageCmd(export, ds, out string) {
	card, err := infer.LoadCard(export)
	if err != nil {
		fatal(err)
	}
	if err := card.VerifyONNX(export); err != nil {
		fatal(err)
	}
	ts, err := dataset.LoadTestSet(ds)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "creating %s", out))
	}
	for _, f := range []string{"model.json", card.ONNX} {
		if err := copyFile(filepath.Join(export, f), filepath.Join(out, f)); err != nil {
			fatal(diag.Wrap("E-RND-001", err, "copying %s", f))
		}
	}
	bj, _ := json.MarshalIndent(ts, "", "  ")
	if err := os.WriteFile(filepath.Join(out, infer.TestSetFile), bj, 0o644); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing testset.json"))
	}
	fmt.Printf("model folder %s: %s (%s), %d test frames of %s\n", out, card.Name, card.Arch, len(ts.Frames), ts.Scenario)
}

// goldenCmd picks the test tiles with the most labelled parts (plus two empty
// ones) and stores the ONNX Runtime results for them.
func goldenCmd(model, ds string, n int) {
	dir := dirOf(model)
	card, err := infer.LoadCard(dir)
	if err != nil {
		fatal(err)
	}
	b, err := infer.OpenORT(filepath.Join(dir, card.ONNX), card.InputSize, 1)
	if err != nil {
		fatal(err)
	}
	defer b.Close()
	labels, _ := filepath.Glob(filepath.Join(ds, "labels", "test", "*.txt"))
	if len(labels) == 0 {
		fatal(diag.New("E-DS-001", "no test labels in %s", ds))
	}
	type cand struct {
		stem  string
		count int
	}
	var cs []cand
	for _, l := range labels {
		f, err := os.Open(l)
		if err != nil {
			continue
		}
		c := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				c++
			}
		}
		f.Close()
		cs = append(cs, cand{strings.TrimSuffix(filepath.Base(l), ".txt"), c})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].count != cs[j].count {
			return cs[i].count > cs[j].count
		}
		return cs[i].stem < cs[j].stem
	})
	var pick []cand
	for _, c := range cs {
		if c.count > 0 && len(pick) < n-2 {
			pick = append(pick, c)
		}
	}
	empty := 0
	for _, c := range cs {
		if c.count == 0 && empty < 2 {
			pick = append(pick, c)
			empty++
		}
	}
	var tl []*raster.RGB
	var names []string
	for i, c := range pick {
		f, err := os.Open(filepath.Join(ds, "images", "test", c.stem+".jpg"))
		if err != nil {
			fatal(diag.Wrap("E-DS-001", err, "test tile"))
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			fatal(diag.Wrap("E-DS-001", err, "decoding %s", c.stem))
		}
		m := raster.NewRGB(img.Bounds().Dx(), img.Bounds().Dy())
		for y := 0; y < m.H; y++ {
			for x := 0; x < m.W; x++ {
				r, g, bb, _ := img.At(x, y).RGBA()
				m.Set(x, y, [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(bb >> 8)})
			}
		}
		tl = append(tl, m)
		names = append(names, fmt.Sprintf("tile-%02d.png", i))
	}
	g, err := infer.MakeGolden(b, tl, names, infer.DefaultParams(), card.ONNXSHA256)
	if err != nil {
		fatal(err)
	}
	gd := filepath.Join(dir, infer.GoldenDir)
	if err := g.Save(gd, tl); err != nil {
		fatal(err)
	}
	total := 0
	for _, t := range g.Tiles {
		total += len(t.Detections)
	}
	fmt.Printf("golden set %s: %d tiles, %d reference detections (%s)\n", gd, len(tl), total, b.Name())
}

func parityCmd(model, golden, jsonOut string) {
	b, _ := open(model)
	defer b.Close()
	g, tl, err := infer.LoadGolden(golden)
	if err != nil {
		fatal(err)
	}
	p, err := infer.CheckParity(g, tl, b)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s vs %s on %d golden tiles: %d/%d matched (%d borderline), median box IoU %.3f, max conf diff %.3f, max raw score diff %.3f\n",
		b.Name(), g.Backend, len(tl), p.Matched, p.Reference-p.Borderline, p.Borderline, p.MedianBoxIoU, p.MaxConfDiff, p.MaxScoreDiff)
	for _, pr := range p.Problems {
		fmt.Println("  " + pr)
	}
	if jsonOut != "" {
		bj, _ := json.MarshalIndent(p, "", "  ")
		os.WriteFile(jsonOut, bj, 0o644)
	}
	if !p.ParityOK() {
		fmt.Println("RESULT: FAIL")
		os.Exit(cli.ExitFail)
	}
	fmt.Println("RESULT: PASS")
}

// stubModel writes a model folder whose network ignores the picture: every
// tile gets the same two detections. It exists to test the tool chain
// (packaging, TensorRT engine building, parity) before a real model is trained.
func stubModel(out string) {
	const size, nc, nm = 640, 5, 32
	A := 8400
	C := 4 + nc + nm
	mh, mw := size/4, size/4
	spec := onnxgen.Spec{C: C, A: A, NM: nm, MH: mh, MW: mw, Preds: make([]float32, C*A), Protos: make([]float32, nm*mh*mw)}
	put := func(a int, cx, cy, w, h float32, cls int, score float32) {
		spec.Preds[0*A+a], spec.Preds[1*A+a], spec.Preds[2*A+a], spec.Preds[3*A+a] = cx, cy, w, h
		spec.Preds[(4+cls)*A+a] = score
		spec.Preds[(4+nc)*A+a] = 1
	}
	put(100, 200, 200, 40, 20, 0, 0.9)
	put(5000, 450, 300, 30, 80, 2, 0.7)
	for y := 0; y < mh; y++ {
		for x := 0; x < mw; x++ {
			v := float32(-1)
			if (x >= 45 && x < 55 && y >= 47 && y < 53) || (x >= 108 && x < 117 && y >= 66 && y < 84) {
				v = 0.5 // plus the image mean (0..1): positive inside the two regions
			}
			spec.Protos[y*mw+x] = v
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "creating %s", out))
	}
	p := filepath.Join(out, "model.onnx")
	if err := onnxgen.Write(p, spec); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing %s", p))
	}
	sum, _ := raster.FileSHA256(p)
	card := infer.Card{Schema: 1, Name: "stub", Arch: "stub (fixed outputs, for testing the tools)", Classes: infer.ClassNames, InputSize: size,
		ONNX: "model.onnx", ONNXSHA256: sum, Notes: "NOT A TRAINED MODEL: every tile gets the same two detections"}
	bj, _ := json.MarshalIndent(card, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "model.json"), bj, 0o644); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing model.json"))
	}
	fmt.Printf("wrote %s/model.onnx and model.json (stand-in model; not trained)\n", out)
}
