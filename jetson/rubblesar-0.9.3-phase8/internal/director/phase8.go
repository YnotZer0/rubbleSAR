package director

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/check"
	"rubblesar/internal/diag"
	"rubblesar/internal/live"
	"rubblesar/internal/scenario"
	"rubblesar/internal/web"
)

// LoadRun reads a recorded run with its after-action report (written again
// when missing), the scenario and the rendered media, and works the facts out.
func LoadRun(dir, scenarioDir string, triage []string) (*live.Player, *Facts, *Media, *scenario.Scenario, error) {
	p, err := live.LoadRun(dir, triage)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sc, _ := scenario.Load(filepath.Join(scenarioDir, "scenario.json"))
	var rep *aar.Report
	if b, err := os.ReadFile(filepath.Join(dir, "report.json")); err == nil {
		var r aar.Report
		if json.Unmarshal(b, &r) == nil {
			rep = &r
		}
	}
	if rep == nil {
		rep, _ = aar.Build(dir, aar.BuildOptions{Scene: sc, Triage: triage})
	}
	m := LoadMedia(dir)
	f := RunFacts(p, rep, sc)
	f.AddMedia(m)
	return p, f, m, sc, nil
}

// Phase8Options are the inputs of the phase 8 acceptance.
type Phase8Options struct {
	Demo *Config
	Web  *Web
	// RunDir: the recorded run the video is made from ("" = none found).
	RunDir      string
	ScenarioDir string
	ModelDir    string
	Triage      []string
	// RecordFor: the test recording on every recorder (0 = 4 s).
	RecordFor time.Duration
	// Rehearse plays the whole storyboard on the presenter page (not recorded).
	Rehearse bool
}

// Phase8 is the phase 8 acceptance, the demo director ready to record: the
// settings and the storyboard (P8-01), a recorded run with the beats the
// storyboard uses (P8-02), a drone's frame rendered again with the model on
// it (P8-03), the narration filled in from the run (P8-04), sar-web's replay and the
// presenter page (P8-05), every recorder connected and recording (P8-06), the
// rough-cut tools (P8-07) and, with Rehearse, the whole take on the presenter
// page (P8-08).
func Phase8(ctx context.Context, o Phase8Options, out io.Writer) *check.Report {
	r := check.NewReport("phase8", out)
	r.Share = true
	if o.RecordFor <= 0 {
		o.RecordFor = 4 * time.Second
	}
	var sb *Storyboard
	r.Run("P8-01", "demo settings and storyboard", func() check.Result {
		return p8safe("E-CHK-801", func() check.Result {
			if err := o.Demo.Validate(); err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-801", Detail: err.Error(), Hint: "fix the setting named in " + o.Demo.Path}
			}
			var err error
			if sb, err = LoadStoryboard(o.Demo.Demo.Storyboard); err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-801", Detail: err.Error(), Hint: "fix the storyboard, or set [demo] storyboard = \"\" for the built-in one"}
			}
			n := 0
			for _, rc := range o.Demo.Recorders {
				if rc.Enabled == nil || *rc.Enabled {
					n++
				}
			}
			return check.Result{Status: check.Pass, Detail: fmt.Sprintf("%s; storyboard %s: %d chapters; %d recorders on", o.Demo.Path, sb.Source, len(sb.Chapters), n)}
		})
	})
	var p *live.Player
	var f *Facts
	var m *Media
	r.Run("P8-02", "a recorded run with the storyboard's beats", func() check.Result {
		return p8safe("E-CHK-802", func() check.Result {
			if o.RunDir == "" {
				return check.Result{Status: check.Fail, Code: "E-CHK-802", Detail: "no recorded run", Hint: "rubblesar demo live records one (or rubblesar mission start)"}
			}
			var err error
			if p, f, m, _, err = LoadRun(o.RunDir, o.ScenarioDir, o.Triage); err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-802", Detail: err.Error(), Hint: "rubblesar report --list shows the runs"}
			}
			have := map[string]bool{}
			var ev []string
			for _, b := range p.Beats() {
				if !have[b.Name] {
					ev = append(ev, fmt.Sprintf("%s at +%s: %s", b.Name, mmss(b.At-p.Start().UnixMilli()), b.Text))
				}
				have[b.Name] = true
			}
			var missing []string
			want := []string{"first-red", "first-hover", "first-team", "first-tak-team", "outage"}
			if sb != nil {
				want = sb.Beats()
			}
			for _, b := range want {
				if !have[b] {
					missing = append(missing, b)
				}
			}
			d := fmt.Sprintf("run %s: %d events, %s long", p.Run, len(p.Events()), mmss(p.End().Sub(p.Start()).Milliseconds()))
			if len(missing) > 0 {
				return check.Result{Status: check.Warn, Code: "E-CHK-802", Detail: d + "; no " + strings.Join(missing, ", "), Evidence: ev,
					Hint: "the chapters that jump there play on from the previous one; a live take (rubblesar demo live) has every beat, first-tak-team only with a TAK team online (rubblesar demo check, TAK teams)"}
			}
			return check.Result{Status: check.Pass, Detail: d + "; every beat the storyboard uses", Evidence: ev}
		})
	})
	r.Run("P8-03", "a drone's frame rendered again, the model run on it", func() check.Result {
		return p8safe("E-CHK-803", func() check.Result {
			if p == nil {
				return check.Result{Status: check.Skip, Detail: "no run (P8-02)"}
			}
			made := false
			if m == nil {
				var err error
				m, err = MakeMedia(p, MediaOptions{ScenarioDir: o.ScenarioDir, ModelDir: o.ModelDir, EdgeSeed: o.Demo.Demo.EdgeSeed})
				if err != nil {
					return check.Result{Status: check.Fail, Code: "E-CHK-803", Detail: err.Error(), Hint: "rubblesar demo media shows each step; the scenario must be the one the run flew"}
				}
				f.AddMedia(m)
				made = true
			}
			d := fmt.Sprintf("frame %s by %s at %.0f m, %.1f cm a pixel: %d parts (%d in view) by %s", m.Frame, m.UAV, m.AltM, m.GSDcm, len(m.Detections), m.Truth, m.Model)
			if made {
				d = "rendered now: " + d
			}
			switch {
			case !m.Exact:
				return check.Result{Status: check.Warn, Code: "E-CHK-803", Detail: d + "; not exactly the frame the drone saw", Hint: "[demo] edge_seed must equal edge-eye's seed on the Jetson; a run recorded before phase 8 has no frame poses"}
			case m.InferMS == 0:
				return check.Result{Status: check.Warn, Code: "E-CHK-803", Detail: d, Hint: "no model on this ZBook (scripts/zbook/12-onnxruntime.sh, then 82-evaluate.sh installs it): the presenter shows the scenario's labels instead"}
			case len(m.Detections) == 0:
				return check.Result{Status: check.Warn, Code: "E-CHK-803", Detail: d, Hint: "rubblesar demo media --frame ID renders another frame (rubblesar demo facts lists them)"}
			}
			return check.Result{Status: check.Pass, Detail: d}
		})
	})
	r.Run("P8-04", "the narration filled in from the run", func() check.Result {
		return p8safe("E-CHK-804", func() check.Result {
			if sb == nil || p == nil {
				return check.Result{Status: check.Skip, Detail: "no storyboard or run (P8-01, P8-02)"}
			}
			pl, err := MakePlan(sb, p, f, m)
			if err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-804", Detail: err.Error(), Hint: "rubblesar demo script prints the narration chapter by chapter"}
			}
			var ev, warns []string
			at := 0.0
			for _, c := range pl.Chapters {
				ev = append(ev, fmt.Sprintf("[%s] %d. %s (%.0f s)", mmss(int64(at*1000)), c.Index, c.Title, c.Seconds))
				for _, w := range c.Warnings {
					warns = append(warns, c.ID+": "+w)
				}
				if strings.Contains(c.Narration, "<no value>") || strings.Contains(c.Narration, " ? ") {
					warns = append(warns, c.ID+": a number the run does not have")
				}
				at += c.Seconds
			}
			d := fmt.Sprintf("%d chapters, %s", len(pl.Chapters), mmss(int64(at*1000)))
			switch {
			case len(warns) > 0:
				return check.Result{Status: check.Warn, Code: "E-CHK-804", Detail: d + "; " + strings.Join(limitList(warns, 3), "; "), Evidence: ev, Hint: "rubblesar demo script shows the chapters with their warnings"}
			case at < 90 || at > 480:
				return check.Result{Status: check.Warn, Code: "E-CHK-804", Detail: d + ": aim for 2 to 8 minutes", Evidence: ev, Hint: "min_seconds and the narration in the storyboard set the length"}
			}
			return check.Result{Status: check.Pass, Detail: d, Evidence: ev}
		})
	})
	r.Run("P8-05", "sar-web's replay and the presenter page", func() check.Result {
		return p8safe("E-CHK-805", func() check.Result {
			if p == nil {
				return check.Result{Status: check.Skip, Detail: "no run (P8-02)"}
			}
			if _, err := o.Web.Replay(web.ReplayCommand{Action: "load", Run: p.Run}); err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-805", Detail: err.Error(), Hint: "sar-web 0.9 or later must run on this ZBook (sudo systemctl status sar-web; [demo] web)"}
			}
			if _, err := o.Web.Replay(web.ReplayCommand{Action: "seek", At: "end"}); err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-805", Detail: err.Error()}
			}
			pic, err := o.Web.Picture("replay")
			if err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-805", Detail: err.Error()}
			}
			d := fmt.Sprintf("replay of %s: %d drones, %d casualties, %d teams at the end", p.Run, len(pic.Drones), len(pic.Casualties), len(pic.Teams))
			if len(pic.Drones) == 0 || len(pic.Casualties) == 0 {
				return check.Result{Status: check.Fail, Code: "E-CHK-805", Detail: d, Hint: "the run has no drones or casualties: record a full one (rubblesar demo live)"}
			}
			n, err := o.Web.Present(web.Cue{Source: "replay", Slate: "sar-check phase 8: presenter test"})
			if err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-805", Detail: err.Error()}
			}
			time.Sleep(1500 * time.Millisecond)
			_, _ = o.Web.Present(web.Cue{Source: "live"})
			if n == 0 {
				return check.Result{Status: check.Warn, Code: "E-CHK-805", Detail: d + "; no presenter page open",
					Hint: "rubblesar demo obs-setup creates OBS's presenter scene (a browser source); or open " + o.Web.Base + "/ops/?present=1"}
			}
			return check.Result{Status: check.Pass, Detail: fmt.Sprintf("%s; %d presenter page(s) showed a test cue", d, n)}
		})
	})
	r.Run("P8-06", "every recorder connected and recording", func() check.Result {
		return p8safe("E-CHK-806", func() check.Result { return p8Recorders(ctx, o) })
	})
	r.Run("P8-07", "the rough-cut tools", func() check.Result {
		return p8safe("E-CHK-807", func() check.Result {
			ff, err := exec.LookPath("ffmpeg")
			if err != nil {
				return check.Result{Status: check.Warn, Code: "E-CHK-807", Detail: "ffmpeg not installed: no rough cut", Hint: "sudo apt install ffmpeg"}
			}
			d := "ffmpeg " + ff
			if o.Demo.Demo.TTS == "" {
				return check.Result{Status: check.Pass, Detail: d + "; no text-to-speech ([demo] tts): record the voice-over from the take's narration.txt"}
			}
			prog := strings.Fields(o.Demo.Demo.TTS)[0]
			if _, err := exec.LookPath(prog); err != nil {
				return check.Result{Status: check.Warn, Code: "E-CHK-807", Detail: d + "; text-to-speech " + prog + " not found", Hint: "install it, or set [demo] tts = \"\""}
			}
			return check.Result{Status: check.Pass, Detail: d + "; text-to-speech " + prog}
		})
	})
	if !o.Rehearse {
		r.Skip("P8-08", "a rehearsal on the presenter page", "not asked for (--rehearse)")
		return r
	}
	r.Run("P8-08", "a rehearsal on the presenter page", func() check.Result {
		return p8safe("E-CHK-808", func() check.Result {
			if sb == nil || p == nil {
				return check.Result{Status: check.Skip, Detail: "no storyboard or run (P8-01, P8-02)"}
			}
			pl, err := MakePlan(sb, p, f, m)
			if err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-808", Detail: err.Error()}
			}
			pl.RunDir = o.RunDir
			t0 := time.Now()
			cs, dir, err := ReplayTake(ctx, ReplayOptions{Cfg: o.Demo, Plan: pl, Web: o.Web, Rehearse: true, Out: io.Discard})
			if err != nil {
				return check.Result{Status: check.Fail, Code: "E-CHK-808", Detail: err.Error(), Hint: "rubblesar demo replay --rehearse shows each chapter as it goes"}
			}
			return check.Result{Status: check.Pass, Detail: fmt.Sprintf("%d chapters in %s; cue sheet %s", len(cs.Chapters), mmss(time.Since(t0).Milliseconds()), dir)}
		})
	})
	return r
}

func p8Recorders(ctx context.Context, o Phase8Options) check.Result {
	var rs []RecorderConfig
	for _, rc := range o.Demo.Recorders {
		if rc.Enabled == nil || *rc.Enabled {
			rs = append(rs, rc)
		}
	}
	if len(rs) == 0 {
		return check.Result{Status: check.Warn, Code: "E-CHK-806", Detail: "no recorder on: nothing records the takes", Hint: "set up OBS on this ZBook ([[recorder]] zbook in " + o.Demo.Path + ", then rubblesar demo obs-setup)"}
	}
	dir := filepath.Join(o.Demo.Demo.TakesDir, TakeName("test"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return check.Result{Status: check.Fail, Code: "E-CHK-806", Detail: err.Error(), Hint: "[demo] takes_dir must be writable"}
	}
	var ev, bad []string
	var running []Recorder
	for _, rc := range rs {
		rec := NewRecorder(rc)
		ok := true
		for _, take := range []string{"live", "replay"} {
			if !rc.On(take) {
				continue
			}
			d, err := rec.Check(ctx, take)
			if err != nil {
				bad, ok = append(bad, err.Error()), false
				break
			}
			ev = append(ev, fmt.Sprintf("%s (%s): %s", rc.Name, take, d))
		}
		if !ok {
			rec.Close()
			continue
		}
		take := "live"
		if !rc.On("live") {
			take = "replay"
		}
		if _, err := rec.Start(ctx, take, dir); err != nil {
			bad = append(bad, err.Error())
			rec.Close()
			continue
		}
		running = append(running, rec)
	}
	select {
	case <-ctx.Done():
	case <-time.After(o.RecordFor):
	}
	for _, rec := range running {
		path, err := rec.Stop(ctx)
		rec.Close()
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if st, err := os.Stat(path); err == nil && st.Size() == 0 {
			bad = append(bad, fmt.Sprintf("%s wrote an empty file %s", rec.Name(), path))
			continue
		}
		ev = append(ev, fmt.Sprintf("%s recorded %s", rec.Name(), path))
	}
	if len(bad) > 0 {
		return check.Result{Status: check.Fail, Code: "E-CHK-806", Detail: strings.Join(bad, "; "), Evidence: ev,
			Hint: "OBS: Tools > WebSocket Server Settings (enabled, port 4455, the password in demo.toml; on Windows the firewall must let the ZBook in); scrcpy: adb devices must list the phone"}
	}
	return check.Result{Status: check.Pass, Detail: fmt.Sprintf("%d recorder(s), %s test recordings in %s", len(running), o.RecordFor, dir), Evidence: ev}
}

func p8safe(code diag.Code, fn func() check.Result) (res check.Result) {
	defer func() {
		if x := recover(); x != nil {
			res = check.Result{Status: check.Fail, Code: code, Detail: fmt.Sprintf("internal error: %v", x), Hint: "paste the block"}
		}
	}()
	return fn()
}

func mmss(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	s := ms / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func limitList(v []string, n int) []string {
	if len(v) > n {
		return append(v[:n:n], fmt.Sprintf("and %d more", len(v)-n))
	}
	return v
}
