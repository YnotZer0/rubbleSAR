package director

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/check"
	"rubblesar/internal/live"
	"rubblesar/internal/ops"
	"rubblesar/internal/web"
)

// LiveOptions configure the live take.
type LiveOptions struct {
	Cfg       *Config
	Env       *ops.Env
	Mission   ops.MissionOptions
	Web       *Web
	Recorders []Recorder
	Out       io.Writer
	// SetBudget and Blackout send the staged link events (default: the control channel).
	SetBudget func(uav string, bps uint32) error
	Blackout  func(uav string, seconds uint32) error
}

// beatRules turn timeline lines into beats: kind, text that must be in the line, beat, once only.
var beatRules = []struct {
	kind, has, beat string
	once            bool
}{
	{"found", "", "first-casualty", true},
	{"look", " sent for a close look", "first-look", true},
	{"look", ", watching it", "first-hover", true},
	{"look", "close look at ", "first-look-done", true},
	{"team", "TAK device", "first-tak-team", true},
	{"team", " sent to ", "first-team", true},
	{"team", " reached ", "first-on-scene", true},
	{"freed", "", "first-freed", true},
	{"survey", "finished its survey", "survey-done", true},
	{"survey", "is surveying", "survey", true},
	{"outage", "not heard", "outage", false},
	{"outage", "heard again", "outage-end", false},
	{"outage", "queue empty", "queue-flushed", false},
}

// LiveTake runs a mission (as `rubblesar mission start`) while the recorders
// record, stages the link events of [live] and marks the beats in the run's
// recording, on the recorders (chapters) and in the take's cue sheet.
func LiveTake(ctx context.Context, o LiveOptions) (*CueSheet, string, error) {
	out := o.Out
	take := TakeName("live")
	dir := filepath.Join(o.Cfg.Demo.TakesDir, take)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("E-DIR-006: %w", err)
	}
	cs := &CueSheet{Take: take, Kind: "live"}
	if _, err := o.Web.Present(web.Cue{Source: "live", Slate: "Take " + take}); err != nil {
		fmt.Fprintf(out, "    WARNING: presenter: %v\n", err)
	}
	cs.Started = time.Now()
	if err := startRecorders(ctx, o.Recorders, "live", dir, cs, out); err != nil {
		stopRecorders(context.Background(), o.Recorders, cs, out)
		_ = cs.Save(dir)
		return cs, dir, err
	}
	sleepCtx(ctx, time.Duration(o.Cfg.Demo.SlateS*float64(time.Second)))
	_, _ = o.Web.Present(web.Cue{Source: "live"})

	inc := o.Env.Cfg.Site.IncidentID
	var (
		mu      sync.Mutex
		rec     *aar.Recorder
		hub     *live.Hub
		feed    *live.Live
		stop    = make(chan struct{})
		done    = make(chan struct{})
		started time.Time
	)
	beat := func(name, ref, text string) {
		mu.Lock()
		cs.Beats = append(cs.Beats, BeatCue{Name: name, At: time.Now(), Ref: ref, Text: text})
		r := rec
		mu.Unlock()
		if r != nil {
			r.Beat(name, ref, text)
		}
		for _, x := range o.Recorders {
			_ = x.Chapter(context.Background(), name)
		}
		fmt.Fprintf(out, "  beat %-15s %s\n", name, text)
	}
	setBudget, blackout := o.SetBudget, o.Blackout
	if setBudget == nil {
		setBudget = func(uav string, bps uint32) error {
			return check.SetLinkBudget(o.Env.Bus, inc, uav, bps, 3*o.Env.Cfg.Check.PingTimeout.Duration)
		}
	}
	if blackout == nil {
		blackout = func(uav string, s uint32) error {
			return check.SetLinkOutage(o.Env.Bus, inc, uav, s, 3*o.Env.Cfg.Check.PingTimeout.Duration)
		}
	}
	mo := o.Mission
	mo.Suite = "demo live take " + take
	mo.OnRecording = func(r *aar.Recorder, at time.Time) {
		h := live.NewHub(live.HubOptions{Incident: inc, Triage: o.Env.Cfg.Dispatch.Triage})
		l, err := live.Start(live.Options{Incident: inc, Bus: o.Env.Bus, Hub: h})
		if err != nil {
			fmt.Fprintf(out, "    WARNING: beats will not be marked: %v\n", err)
		}
		mu.Lock()
		rec, hub, feed, started = r, h, l, at
		mu.Unlock()
		go func() {
			defer close(done)
			watchBeats(o, hub, started, beat, setBudget, blackout, stop)
		}()
	}
	mo.OnEnd = func() {
		close(stop)
		<-done
	}
	res, err := ops.Mission(ctx, o.Env, out, mo)
	if feed != nil {
		feed.Close()
	}
	stopRecorders(context.Background(), o.Recorders, cs, out)
	cs.Ended = time.Now()
	if res != nil {
		cs.Run, cs.RunDir = filepath.Base(res.Dir), res.Dir
	}
	if serr := cs.Save(dir); serr != nil && err == nil {
		err = serr
	}
	return cs, dir, err
}

type staged struct {
	ev     LiveEvent
	due    time.Time
	done   bool
	undo   time.Time
	undone bool
	prev   uint32
}

// watchBeats turns the live timeline into beats and fires the staged link events when their moment comes.
func watchBeats(o LiveOptions, hub *live.Hub, started time.Time, beat func(name, ref, text string),
	setBudget func(string, uint32) error, blackout func(string, uint32) error, stop chan struct{}) {
	seen := 0
	fired := map[string]time.Time{}
	var evs []*staged
	for _, e := range o.Cfg.Live.Events {
		evs = append(evs, &staged{ev: e})
	}
	resolve := func(at string) (time.Time, bool) {
		if strings.HasPrefix(at, "+") {
			if d, ok := live.ParseClock(at[1:]); ok {
				return started.Add(d), true
			}
			if d, err := time.ParseDuration(at[1:]); err == nil {
				return started.Add(d), true
			}
			return time.Time{}, false
		}
		base, off := at, time.Duration(0)
		if i := strings.LastIndexAny(at, "+-"); i > 0 {
			if d, err := time.ParseDuration(at[i+1:]); err == nil {
				base, off = at[:i], d
				if at[i] == '-' {
					off = -d
				}
			}
		}
		if t, ok := fired[base]; ok {
			return t.Add(off), true
		}
		return time.Time{}, false
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		tl := hub.Timeline()
		if len(tl) < seen {
			seen = 0
		}
		for _, it := range tl[seen:] {
			for _, r := range beatRules {
				if it.Kind != r.kind || (r.has != "" && !strings.Contains(it.Text, r.has)) {
					continue
				}
				if r.once {
					if _, ok := fired[r.beat]; ok {
						continue
					}
				}
				if _, ok := fired[r.beat]; !ok {
					fired[r.beat] = time.UnixMilli(it.T)
				}
				beat(r.beat, it.Ref, it.Text)
				if r.kind == "team" && r.beat == "first-tak-team" {
					break // not also first-team
				}
			}
			if it.Kind == "found" && it.Tone == "red" {
				if _, ok := fired["first-red"]; !ok {
					fired["first-red"] = time.UnixMilli(it.T)
					beat("first-red", it.Ref, it.Text)
				}
			}
			if it.Kind == "chat" && !strings.HasPrefix(it.Text, "SAR-OPS") {
				if _, ok := fired["first-tak-reply"]; !ok {
					fired["first-tak-reply"] = time.UnixMilli(it.T)
					beat("first-tak-reply", "", it.Text)
				}
			}
		}
		seen = len(tl)
		now := time.Now()
		for _, s := range evs {
			if s.due.IsZero() {
				if t, ok := resolve(s.ev.At); ok {
					s.due = t
					fmt.Fprintf(o.Out, "  staged: %s at %s\n", eventWords(s.ev), t.Format("15:04:05"))
				}
				continue
			}
			if !s.done && !now.Before(s.due) {
				s.done = true
				fire(o, hub, s, beat, setBudget, blackout)
			}
			if s.done && !s.undo.IsZero() && !s.undone && !now.Before(s.undo) {
				s.undone = true
				if s.ev.BudgetKbps != nil {
					if err := setBudget(s.ev.UAV, s.prev); err != nil {
						fmt.Fprintf(o.Out, "    WARNING: restoring %s's budget: %v\n", s.ev.UAV, err)
					}
					beat("budget-restore", s.ev.UAV, fmt.Sprintf("%s radio budget back to %s", s.ev.UAV, kbpsWords(s.prev)))
				} else {
					beat("blackout-end", s.ev.UAV, fmt.Sprintf("%s staged blackout over", s.ev.UAV))
				}
			}
		}
	}
}

func eventWords(e LiveEvent) string {
	if e.BudgetKbps != nil {
		return fmt.Sprintf("%s radio budget %g kbit/s for %s", e.UAV, *e.BudgetKbps, e.For)
	}
	return fmt.Sprintf("%s blackout for %s", e.UAV, e.Blackout)
}

func kbpsWords(bps uint32) string {
	if bps == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g kbit/s", float64(bps)/1000)
}

func fire(o LiveOptions, hub *live.Hub, s *staged, beat func(name, ref, text string), setBudget func(string, uint32) error, blackout func(string, uint32) error) {
	e := s.ev
	if e.BudgetKbps != nil {
		s.prev = 256000
		for _, d := range hub.Snapshot().Drones {
			if d.ID == e.UAV && d.Link != nil {
				s.prev = d.Link.BudgetBps
			}
		}
		bps := uint32(*e.BudgetKbps * 1000)
		if err := setBudget(e.UAV, bps); err != nil {
			fmt.Fprintf(o.Out, "    WARNING: staged budget cut on %s: %v\n", e.UAV, err)
			return
		}
		d, _ := time.ParseDuration(e.For)
		s.undo = time.Now().Add(d)
		beat("budget-cut", e.UAV, fmt.Sprintf("%s radio budget cut to %s for %s (staged from the ZBook)", e.UAV, kbpsWords(bps), e.For))
		return
	}
	d, _ := time.ParseDuration(e.Blackout)
	if err := blackout(e.UAV, uint32(d.Seconds())); err != nil {
		fmt.Fprintf(o.Out, "    WARNING: staged blackout on %s: %v\n", e.UAV, err)
		return
	}
	s.undo = time.Now().Add(d)
	beat("blackout", e.UAV, fmt.Sprintf("%s radio blackout for %s (staged from the ZBook)", e.UAV, e.Blackout))
}
