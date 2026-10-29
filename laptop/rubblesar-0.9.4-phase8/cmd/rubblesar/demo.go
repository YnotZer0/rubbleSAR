package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/director"
	"rubblesar/internal/infer"
	"rubblesar/internal/live"
	"rubblesar/internal/ops"
	"rubblesar/internal/scenario"
)

func demoUsage() {
	fmt.Fprint(os.Stderr, `rubblesar demo: record the material for a video of the system (phase 8, docs/PHASE8.md)

  rubblesar demo init                    write ~/.config/rubblesar/demo.toml (recorders) and storyboard.toml (chapters)
  rubblesar demo check                   settings, storyboard, sar-web, presenter page, recorders, ffmpeg
  rubblesar demo obs-setup [--recorder NAME]
                                         create the scenes in OBS (the presenter page, the operator view, WinTAK)
  rubblesar demo test-record [--seconds 5]
                                         a short recording on every recorder, to check they work
  rubblesar demo live [--max-minutes N]  the live take: a mission recorded on every recorder, with the staged
                                         link events of demo.toml, its beats marked
  rubblesar demo media [--run RUN] [--frame ID]
                                         render a drone's camera frame again and run the model on it (for the presenter)
  rubblesar demo facts [--run RUN]       the run's numbers the narration uses
  rubblesar demo script [--run RUN]      the narration filled in for the run, chapter by chapter, and how long
  rubblesar demo replay [--run RUN] [--rehearse] [--chapter ID,ID]
                                         the narrated replay take, recorded (--rehearse: shown, not recorded)
  rubblesar demo voice [--take TAKE]     speak the narration with the text-to-speech command ([demo] tts)
  rubblesar demo cut [--take TAKE] [--live TAKE] [--main FILE] [--clip NAME=FILE ...] [--out FILE] [--dry-run]
                                         a rough cut with ffmpeg: the replay take's recording, the live take's
                                         inserts, the voice (if made)
  rubblesar demo takes                   the takes so far

Options for all: --demo FILE (default ~/.config/rubblesar/demo.toml), --config FILE (sar.toml).
RUN is a recorded run (rubblesar report --list; default the latest); TAKE a take folder or name (default the latest).
`)
}

type demoEnv struct {
	cfg  *config.ZBook
	dcfg *director.Config
	web  *director.Web
}

func demoLoad(fs *flag.FlagSet, args []string) *demoEnv {
	demoPath := fs.String("demo", director.DefaultConfigPath(), "demo settings")
	cfg, _ := load(fs, args)
	d, err := director.LoadConfig(*demoPath)
	if err != nil {
		cli.Fatal(tool, err, "settings: "+*demoPath, "rubblesar demo init writes a commented one")
	}
	return &demoEnv{cfg: cfg, dcfg: d, web: director.NewWeb(d.Demo.Web)}
}

// runDir finds a recorded run: a folder, a run's name, or the latest.
func runDir(cfg *config.ZBook, run string) string {
	switch {
	case run == "" || run == "latest":
		runs := ops.Runs(cfg.ReportsDir())
		if len(runs) == 0 {
			cli.Fatal(tool, diag.New("E-AAR-002", "no recorded runs in %s", cfg.ReportsDir()), "rubblesar demo live (or rubblesar mission start) records one")
		}
		return filepath.Join(cfg.ReportsDir(), runs[0])
	case strings.Contains(run, "/"):
		return run
	}
	return filepath.Join(cfg.ReportsDir(), run)
}

// takeDir finds a take: a folder, a take's name, or the latest of a kind ("" = any).
func takeDir(d *director.Config, take, kind string) string {
	if strings.Contains(take, "/") {
		return take
	}
	if take != "" {
		return filepath.Join(d.Demo.TakesDir, take)
	}
	for _, t := range director.Takes(d.Demo.TakesDir) {
		if kind == "" || strings.HasSuffix(t, "-"+kind) {
			return filepath.Join(d.Demo.TakesDir, t)
		}
	}
	return ""
}

// loadRun reads a run with its report, the scenario and its media, and the facts from them.
func loadRun(cfg *config.ZBook, dir string) (*live.Player, *director.Facts, *director.Media, *scenario.Scenario) {
	p, f, m, sc, err := director.LoadRun(dir, cfg.Site.Scenario(), cfg.Dispatch.Triage)
	if err != nil {
		cli.Fatal(tool, err)
	}
	return p, f, m, sc
}

// storyBeats are the beats the storyboard uses.
func storyBeats(d *director.Config) []string {
	if sb, err := director.LoadStoryboard(d.Demo.Storyboard); err == nil {
		return sb.Beats()
	}
	return nil
}

func recorders(d *director.Config, take string) []director.Recorder {
	var out []director.Recorder
	for _, rc := range d.Recorders {
		if rc.On(take) {
			out = append(out, director.NewRecorder(rc))
		}
	}
	return out
}

func demo(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		demoUsage()
		os.Exit(cli.ExitSetup)
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("demo "+sub, flag.ExitOnError)
	run := fs.String("run", "", "the recorded run (default the latest)")
	take := fs.String("take", "", "the take (default the latest)")
	liveTake := fs.String("live", "", "cut: the live take with the inserts (default the latest live take of the same run)")
	recorder := fs.String("recorder", "", "obs-setup: only this recorder")
	seconds := fs.Float64("seconds", 5, "test-record: how long")
	maxMin := fs.Float64("max-minutes", 0, "live: stop the mission after this long (default [ops] max_mission)")
	frame := fs.String("frame", "", "media: render this frame instead of the one chosen")
	rehearse := fs.Bool("rehearse", false, "replay: show the take on the presenter page without recording")
	chapters := fs.String("chapter", "", "replay: only these chapters (ids, comma-separated)")
	out := fs.String("out", "", "cut: the video to write (default <take>/rough-cut.mp4)")
	mainFile := fs.String("main", "", "cut: the replay take's recording, when it is not where the recorder wrote it")
	dry := fs.Bool("dry-run", false, "cut: print the ffmpeg command only")
	var clips multiFlag
	fs.Var(&clips, "clip", "cut: NAME=FILE, a live-take recording copied to this ZBook (repeat for each)")
	if sub == "init" {
		dir := filepath.Dir(director.DefaultConfigPath())
		wrote, err := director.Init(dir)
		if err != nil {
			cli.Fatal(tool, err)
		}
		for _, w := range wrote {
			fmt.Println("wrote", w)
		}
		if len(wrote) == 0 {
			fmt.Printf("%s already has demo.toml and storyboard.toml (left as they are)\n", dir)
		}
		fmt.Println("next: set the OBS addresses and passwords in demo.toml, then rubblesar demo obs-setup and rubblesar demo check")
		return
	}
	e := demoLoad(fs, args)
	ctx, stop := cli.SignalContext()
	defer stop()
	header(e.cfg)
	switch sub {
	case "check":
		fmt.Println("==> what the takes need")
		lines := director.Check(ctx, e.dcfg, e.web, os.Stdout)
		fails := 0
		for _, l := range lines {
			if l.Err != nil {
				fails++
			}
		}
		runs := ops.Runs(e.cfg.ReportsDir())
		if len(runs) > 0 {
			dir := filepath.Join(e.cfg.ReportsDir(), runs[0])
			p, err := live.LoadRun(dir, e.cfg.Dispatch.Triage)
			if err == nil {
				names := map[string]bool{}
				for _, b := range p.Beats() {
					names[b.Name] = true
				}
				var missing []string
				for _, b := range storyBeats(e.dcfg) {
					if !names[b] {
						missing = append(missing, b)
					}
				}
				st := "ok  "
				d := fmt.Sprintf("%s: %d events, every beat the storyboard uses", runs[0], len(p.Events()))
				if len(missing) > 0 {
					st, d = "WARN", fmt.Sprintf("%s lacks %s (record a live take: rubblesar demo live)", runs[0], strings.Join(missing, ", "))
				}
				fmt.Printf("  [%s] %-24s %s\n", st, "latest run", d)
				if director.LoadMedia(dir) == nil {
					fmt.Printf("  [WARN] %-24s %s\n", "media", "not made for "+runs[0]+": rubblesar demo media")
				}
			}
		}
		if fails > 0 {
			os.Exit(cli.ExitFail)
		}
	case "obs-setup":
		n := 0
		for _, rc := range e.dcfg.Recorders {
			if rc.Kind != "obs" || (*recorder != "" && rc.Name != *recorder) || (*recorder == "" && rc.Enabled != nil && !*rc.Enabled) {
				continue
			}
			n++
			fmt.Printf("==> %s\n", rc.Name)
			if err := director.OBSSetup(ctx, rc, e.dcfg.Demo.Web, os.Stdout); err != nil {
				cli.Fatal(tool, err, "OBS: Tools > WebSocket Server Settings (enabled, the password in demo.toml)")
			}
		}
		if n == 0 {
			cli.Fatal(tool, diag.New("E-DIR-003", "no OBS recorder to set up (demo.toml [[recorder]], or --recorder NAME)"))
		}
	case "test-record":
		dir := filepath.Join(e.dcfg.Demo.TakesDir, director.TakeName("test"))
		_ = os.MkdirAll(dir, 0o755)
		var rs []director.Recorder
		for _, rc := range e.dcfg.Recorders {
			if rc.Enabled == nil || *rc.Enabled {
				rs = append(rs, director.NewRecorder(rc))
			}
		}
		for _, r := range rs {
			at, err := r.Start(ctx, "live", dir)
			if err != nil {
				fmt.Printf("  [FAIL] %s: %v\n", r.Name(), err)
				continue
			}
			fmt.Printf("  recording %s from %s\n", r.Name(), at.Format("15:04:05.000"))
			defer r.Close()
		}
		time.Sleep(time.Duration(*seconds * float64(time.Second)))
		for _, r := range rs {
			if p, err := r.Stop(ctx); err != nil {
				fmt.Printf("  [FAIL] %s: %v\n", r.Name(), err)
			} else {
				fmt.Printf("  [ok  ] %s wrote %s\n", r.Name(), p)
			}
		}
	case "live":
		b, err := openBus(ctx, e.cfg, e.cfg.Zenoh.OpenTimeout.Duration)
		if err != nil {
			cli.Fatal(tool, diag.Wrap("E-OPS-002", err, "connecting to zenohd"), "rubblesar up")
		}
		defer b.Close()
		oe := env(e.cfg)
		oe.Bus = b
		mo := ops.MissionOptions{}
		if *maxMin > 0 {
			mo.Max = time.Duration(*maxMin * float64(time.Minute))
		}
		rs := recorders(e.dcfg, "live")
		for _, r := range rs {
			if d, err := r.Check(ctx, "live"); err != nil {
				cli.Fatal(tool, err, "rubblesar demo check")
			} else {
				fmt.Printf("    %s: %s\n", r.Name(), d)
			}
		}
		defer func() {
			for _, r := range rs {
				r.Close()
			}
		}()
		fmt.Printf("==> live take: a mission, recorded by %s\n", names(rs))
		cs, dir, err := director.LiveTake(ctx, director.LiveOptions{Cfg: e.dcfg, Env: oe, Mission: mo, Web: e.web, Recorders: rs, Out: os.Stdout})
		if err != nil && cs == nil {
			cli.Fatal(tool, err)
		}
		fmt.Printf("\nLive take %s: %d beats, run %s\n  cue sheet: %s\n", cs.Take, len(cs.Beats), cs.Run, filepath.Join(dir, "cues.json"))
		for _, r := range cs.Recorders {
			fmt.Printf("  %s: %s\n", r.Name, nonEmpty(r.Output, r.Error))
		}
		fmt.Printf("next: rubblesar demo media, then rubblesar demo replay --rehearse\n")
		if err != nil {
			cli.Fatal(tool, err)
		}
	case "media":
		dir := runDir(e.cfg, *run)
		p, err := live.LoadRun(dir, e.cfg.Dispatch.Triage)
		if err != nil {
			cli.Fatal(tool, err)
		}
		fmt.Printf("==> run %s: rendering a drone's frame again and the model on it\n", p.Run)
		m, err := director.MakeMedia(p, director.MediaOptions{ScenarioDir: e.cfg.Site.Scenario(), ModelDir: infer.DefaultModelDir, EdgeSeed: e.dcfg.Demo.EdgeSeed,
			Frame: *frame, Log: func(s string) { fmt.Println("    " + s) }})
		if err != nil {
			cli.Fatal(tool, err)
		}
		fmt.Printf("    frame %s by %s at %.0f m (%.1f cm a pixel%s): %d parts by %s\n    %s\n", m.Frame, m.UAV, m.AltM, m.GSDcm,
			map[bool]string{true: ", exactly as the drone saw it", false: ""}[m.Exact], len(m.Detections), m.Model, filepath.Join(dir, "media"))
	case "facts":
		_, f, _, _ := loadRun(e.cfg, runDir(e.cfg, *run))
		b, _ := json.MarshalIndent(f, "", "  ")
		fmt.Println(string(b))
	case "script":
		sb, err := director.LoadStoryboard(e.dcfg.Demo.Storyboard)
		if err != nil {
			cli.Fatal(tool, err)
		}
		p, f, m, _ := loadRun(e.cfg, runDir(e.cfg, *run))
		pl, err := director.MakePlan(sb, p, f, m)
		if err != nil {
			cli.Fatal(tool, err)
		}
		at := 0.0
		for _, c := range pl.Chapters {
			fmt.Printf("\n[%s] %d. %s (%.0f s)\n%s\n", clockS(at), c.Index, c.Title, c.Seconds, c.Narration)
			for _, w := range c.Warnings {
				fmt.Printf("  WARNING: %s\n", w)
			}
			at += c.Seconds
		}
		fmt.Printf("\nTotal %s (%d chapters) for run %s (%s).\n", clockS(at), len(pl.Chapters), pl.Run, sb.Source)
	case "replay":
		sb, err := director.LoadStoryboard(e.dcfg.Demo.Storyboard)
		if err != nil {
			cli.Fatal(tool, err)
		}
		dir := runDir(e.cfg, *run)
		p, f, m, _ := loadRun(e.cfg, dir)
		pl, err := director.MakePlan(sb, p, f, m)
		if err != nil {
			cli.Fatal(tool, err)
		}
		for _, w := range pl.Warnings {
			fmt.Printf("    WARNING: %s\n", w)
		}
		var rs []director.Recorder
		if !*rehearse {
			rs = recorders(e.dcfg, "replay")
			for _, r := range rs {
				if d, err := r.Check(ctx, "replay"); err != nil {
					cli.Fatal(tool, err, "rubblesar demo check")
				} else {
					fmt.Printf("    %s: %s\n", r.Name(), d)
				}
			}
			defer func() {
				for _, r := range rs {
					r.Close()
				}
			}()
		}
		var only []string
		if *chapters != "" {
			only = strings.Split(*chapters, ",")
		}
		var liveCS *director.CueSheet
		for _, t := range director.Takes(e.dcfg.Demo.TakesDir) {
			if cs, err := director.LoadCueSheet(filepath.Join(e.dcfg.Demo.TakesDir, t)); err == nil && cs.Kind == "live" && cs.Run == p.Run {
				liveCS = cs
				break
			}
		}
		kind := "recorded by " + names(rs)
		if *rehearse {
			kind = "a rehearsal, not recorded"
		}
		fmt.Printf("==> replay take of run %s, %s (%s)\n", p.Run, kind, clockS(pl.Seconds))
		cs, dir2, err := director.ReplayTake(ctx, director.ReplayOptions{Cfg: e.dcfg, Plan: pl, Web: e.web, Recorders: rs, Rehearse: *rehearse, Only: only, Out: os.Stdout, Live: liveCS})
		if err != nil && cs == nil {
			cli.Fatal(tool, err)
		}
		fmt.Printf("\nTake %s: %d chapters\n  %s\n", cs.Take, len(cs.Chapters), dir2)
		for _, r := range cs.Recorders {
			fmt.Printf("  %s: %s\n", r.Name, nonEmpty(r.Output, r.Error))
		}
		fmt.Printf("  narration.txt (the voice-over), narration.srt (subtitles), shotlist.md (what goes where)\n")
		if !*rehearse {
			fmt.Printf("next: rubblesar demo cut (copy other machines' recordings here first: --clip wintak=FILE)\n")
		}
		if err != nil {
			cli.Fatal(tool, err)
		}
	case "voice":
		dir := takeDir(e.dcfg, *take, "replay")
		if dir == "" {
			cli.Fatal(tool, diag.New("E-DIR-006", "no replay take in %s", e.dcfg.Demo.TakesDir))
		}
		fmt.Printf("==> speaking the narration of %s\n", filepath.Base(dir))
		made, err := director.Voice(ctx, dir, e.dcfg.Demo.TTS, os.Stdout)
		if err != nil {
			cli.Fatal(tool, err)
		}
		fmt.Printf("    %d chapters spoken into %s\n", len(made), filepath.Join(dir, "voice"))
	case "cut":
		dir := takeDir(e.dcfg, *take, "replay")
		if dir == "" {
			cli.Fatal(tool, diag.New("E-DIR-006", "no replay take in %s", e.dcfg.Demo.TakesDir))
		}
		cs, err := director.LoadCueSheet(dir)
		if err != nil {
			cli.Fatal(tool, err)
		}
		ld := ""
		if *liveTake != "" {
			ld = takeDir(e.dcfg, *liveTake, "live")
		} else {
			for _, t := range director.Takes(e.dcfg.Demo.TakesDir) {
				if l, err := director.LoadCueSheet(filepath.Join(e.dcfg.Demo.TakesDir, t)); err == nil && l.Kind == "live" && l.Run == cs.Run {
					ld = filepath.Join(e.dcfg.Demo.TakesDir, t)
					break
				}
			}
		}
		cm := map[string]string{}
		for _, c := range clips {
			k, v, ok := strings.Cut(c, "=")
			if !ok {
				cli.Fatal(tool, diag.New("E-DIR-006", "--clip %q: NAME=FILE", c))
			}
			cm[k] = director.ExpandHome(v)
		}
		fmt.Printf("==> rough cut of %s%s\n", filepath.Base(dir), map[bool]string{true: " with inserts from " + filepath.Base(ld), false: ""}[ld != ""])
		mf := ""
		if *mainFile != "" {
			mf = director.ExpandHome(*mainFile)
		}
		p, err := director.Cut(ctx, director.CutOptions{Take: dir, Live: ld, Main: mf, Clips: cm, Out: *out, DryRun: *dry})
		if p != nil {
			for _, w := range p.Warnings {
				fmt.Printf("    WARNING: %s\n", w)
			}
			for _, i := range p.Inserts {
				fmt.Printf("    insert %s\n", i)
			}
			if *dry {
				fmt.Printf("    ffmpeg %s\n", strings.Join(quoteArgs(p.Args), " "))
			}
		}
		if err != nil {
			cli.Fatal(tool, err)
		}
		if !*dry {
			fmt.Printf("    %s (%s, %d chapters' voice)\n", p.Out, clockS(p.Seconds), p.Voice)
		}
	case "takes":
		ts := director.Takes(e.dcfg.Demo.TakesDir)
		if len(ts) == 0 {
			fmt.Printf("no takes in %s yet\n", e.dcfg.Demo.TakesDir)
		}
		for _, t := range ts {
			cs, err := director.LoadCueSheet(filepath.Join(e.dcfg.Demo.TakesDir, t))
			if err != nil {
				continue
			}
			var recs []string
			for _, r := range cs.Recorders {
				recs = append(recs, r.Name)
			}
			sort.Strings(recs)
			fmt.Printf("%s  %s of run %s, %s, %d chapters, %d beats, recorded by %s\n", t, cs.Kind, cs.Run, clockS(cs.Ended.Sub(cs.Started).Seconds()),
				len(cs.Chapters), len(cs.Beats), nonEmpty(strings.Join(recs, ", "), "nothing"))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown: rubblesar demo %s\n\n", sub)
		demoUsage()
		os.Exit(cli.ExitSetup)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func names(rs []director.Recorder) string {
	if len(rs) == 0 {
		return "no recorder"
	}
	var n []string
	for _, r := range rs {
		n = append(n, r.Name())
	}
	return strings.Join(n, ", ")
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func clockS(s float64) string {
	d := int(s + 0.5)
	return fmt.Sprintf("%d:%02d", d/60, d%60)
}

func quoteArgs(a []string) []string {
	out := make([]string, len(a))
	for i, s := range a {
		if strings.ContainsAny(s, " ;'[]|=,") {
			out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		} else {
			out[i] = s
		}
	}
	return out
}
