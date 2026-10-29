package director

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rubblesar/internal/web"
)

// ReplayOptions configure the narrated replay take.
type ReplayOptions struct {
	Cfg  *Config
	Plan *Plan
	Web  *Web
	// Recorders take part (none in a rehearsal).
	Recorders []Recorder
	Rehearse  bool
	// Only these chapters (ids), e.g. to take one again.
	Only []string
	Out  io.Writer
	// Live is the live take of the same run (for the shot list's inserts), nil if none.
	Live *CueSheet
}

// TakeName is a new take's folder name.
func TakeName(kind string) string {
	return time.Now().UTC().Format("20060102T150405Z") + "-" + kind
}

// ReplayTake plays the storyboard on the presenter page while the recorders
// record, and writes the take's cue sheet, narration and subtitles.
func ReplayTake(ctx context.Context, o ReplayOptions) (*CueSheet, string, error) {
	pl := o.Plan
	out := o.Out
	kind := "replay"
	if o.Rehearse {
		kind = "rehearsal"
	}
	take := TakeName(kind)
	dir := filepath.Join(o.Cfg.Demo.TakesDir, take)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("E-DIR-006: %w", err)
	}
	b, _ := json.MarshalIndent(pl, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "plan.json"), append(b, '\n'), 0o644)
	cs := &CueSheet{Take: take, Kind: "replay", Run: pl.Run, RunDir: pl.RunDir, Rehearsal: o.Rehearse}
	chapters := pl.Chapters
	if len(o.Only) > 0 {
		var keep []*Planned
		for _, c := range chapters {
			for _, id := range o.Only {
				if c.ID == id {
					keep = append(keep, c)
				}
			}
		}
		if len(keep) == 0 {
			return nil, "", fmt.Errorf("E-DIR-001: no chapter %s in the storyboard", strings.Join(o.Only, ", "))
		}
		chapters = keep
	}
	// sar-web: the run loaded, the presenter page open.
	fmt.Fprintf(out, "==> loading run %s in sar-web (%s)\n", pl.Run, o.Web.Base)
	if _, err := o.Web.Replay(web.ReplayCommand{Action: "load", Run: pl.Run}); err != nil {
		return nil, "", err
	}
	n, err := o.Web.Presenters()
	if err != nil {
		return nil, "", err
	}
	if n == 0 {
		fmt.Fprintf(out, "    WARNING: no presenter page is open: the recording would show nothing. Open %s/ops/?present=1\n", o.Web.Base)
		fmt.Fprintf(out, "    (OBS: the scene made by 'rubblesar demo obs-setup' shows it as a browser source)\n")
		if !o.Rehearse {
			return nil, "", fmt.Errorf("E-DIR-005: no presenter page open in OBS or a browser (sar-web reports 0)")
		}
	} else {
		fmt.Fprintf(out, "    ok: %d presenter page(s) open\n", n)
	}
	cleanup := func() {
		_, _ = o.Web.Replay(web.ReplayCommand{Action: "pause"})
	}
	defer cleanup()
	// The first chapter's picture before the recorders start (no jump on screen).
	if len(chapters) > 0 {
		c := chapters[0]
		if c.Seek != "" {
			_, _ = o.Web.Replay(web.ReplayCommand{Action: "seek", At: c.Seek})
		}
		cue := c.Cue(len(pl.Chapters), -1)
		cue.Caption, cue.Slate = "", "Take "+take
		if _, err := o.Web.Present(cue); err != nil {
			return nil, "", err
		}
	}
	cs.Started = time.Now()
	if err := startRecorders(ctx, o.Recorders, "replay", dir, cs, out); err != nil {
		stopRecorders(ctx, o.Recorders, cs, out)
		_ = cs.Save(dir)
		return cs, dir, err
	}
	slate := time.Duration(o.Cfg.Demo.SlateS * float64(time.Second))
	sleepCtx(ctx, slate)
	fmt.Fprintf(out, "==> %d chapters, %s\n", len(chapters), short(time.Duration(total(chapters)*float64(time.Second))))
	at := time.Now()
	var runErr error
	for _, c := range chapters {
		if ctx.Err() != nil {
			runErr = ctx.Err()
			break
		}
		start := at
		sleepUntil(ctx, start)
		cc := ChapterCue{ID: c.ID, Title: c.Title, Started: time.Now(), Seconds: c.Seconds, Narration: c.Narration, Segments: c.Segments, Insert: c.Insert}
		fmt.Fprintf(out, "  %s  %d. %s (%.0f s)\n", short(time.Since(cs.Started)), c.Index, c.Title, c.Seconds)
		for name, scene := range c.Scenes {
			for _, r := range o.Recorders {
				if r.Name() == name {
					if err := r.Scene(ctx, scene); err != nil {
						fmt.Fprintf(out, "    WARNING: %s: scene %q: %v\n", name, scene, err)
					}
				}
			}
		}
		for _, r := range o.Recorders {
			_ = r.Chapter(ctx, fmt.Sprintf("%d %s", c.Index, c.Title))
		}
		// The replay.
		cmd := web.ReplayCommand{Action: "seek", At: c.Seek}
		if c.Speed != nil {
			if *c.Speed > 0 {
				cmd.Action, cmd.Speed = "play", *c.Speed
			} else {
				cmd.Action = "pause"
			}
		}
		if cmd.At != "" || c.Speed != nil {
			if r, err := o.Web.Replay(cmd); err != nil {
				fmt.Fprintf(out, "    WARNING: replay: %v\n", err)
			} else {
				cc.RunAt = r.At
			}
		}
		// The captions, one segment at a time.
		segs := c.Segments
		if len(segs) == 0 {
			segs = []Segment{{Start: 0, End: c.Seconds}}
		}
		for i, s := range segs {
			sleepUntil(ctx, start.Add(time.Duration(s.Start*float64(time.Second))))
			if ctx.Err() != nil {
				break
			}
			if _, err := o.Web.Present(c.Cue(len(pl.Chapters), i)); err != nil {
				fmt.Fprintf(out, "    WARNING: presenter: %v\n", err)
			}
		}
		at = start.Add(time.Duration(c.Seconds * float64(time.Second)))
		cs.Chapters = append(cs.Chapters, cc)
	}
	sleepUntil(ctx, at)
	// Hold the last picture for the slate, then stop.
	if last := chapters[len(chapters)-1]; ctx.Err() == nil {
		cue := last.Cue(len(pl.Chapters), -1)
		cue.Caption = ""
		_, _ = o.Web.Present(cue)
		sleepCtx(ctx, slate)
	}
	stopRecorders(context.Background(), o.Recorders, cs, out)
	cs.Ended = time.Now()
	if err := cs.Save(dir); err != nil {
		return cs, dir, err
	}
	if err := cs.WriteScript(dir, o.Live); err != nil {
		return cs, dir, err
	}
	return cs, dir, runErr
}

func total(cs []*Planned) float64 {
	t := 0.0
	for _, c := range cs {
		t += c.Seconds
	}
	return t
}

func startRecorders(ctx context.Context, rs []Recorder, take, dir string, cs *CueSheet, out io.Writer) error {
	for _, r := range rs {
		at, err := r.Start(ctx, take, dir)
		rec := Recording{Name: r.Name(), Kind: kindOf(r), Started: at}
		if err != nil {
			rec.Error = err.Error()
			cs.Recorders = append(cs.Recorders, rec)
			return err
		}
		cs.Recorders = append(cs.Recorders, rec)
		fmt.Fprintf(out, "    recording: %s (from %s)\n", r.Name(), at.Format("15:04:05.000"))
	}
	return nil
}

func stopRecorders(ctx context.Context, rs []Recorder, cs *CueSheet, out io.Writer) {
	for _, r := range rs {
		for i := range cs.Recorders {
			rec := &cs.Recorders[i]
			if rec.Name != r.Name() || rec.Started.IsZero() || !rec.Stopped.IsZero() {
				continue
			}
			p, err := r.Stop(ctx)
			rec.Stopped, rec.Output = time.Now(), p
			if err != nil {
				rec.Error = err.Error()
				fmt.Fprintf(out, "    WARNING: %s: %v\n", r.Name(), err)
			} else {
				fmt.Fprintf(out, "    stopped: %s -> %s\n", r.Name(), p)
			}
		}
	}
}

func kindOf(r Recorder) string {
	if _, ok := r.(*obsRecorder); ok {
		return "obs"
	}
	return "command"
}

func sleepUntil(ctx context.Context, t time.Time) { sleepCtx(ctx, time.Until(t)) }

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
