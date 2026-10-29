package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"rubblesar/internal/live"
)

// Replays holds the recorded run being replayed in the operator view (phase 8).
type Replays struct {
	// Dir: the incident's reports folder ([web] root/reports/<incident>).
	Dir    string
	Triage []string

	mu  sync.Mutex
	cur *live.Player
}

var runRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Current is the loaded replay (nil: none).
func (r *Replays) Current() *live.Player {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// Load replaces the replay with a recorded run (its folder name, or "latest").
func (r *Replays) Load(run string) (*live.Player, error) {
	if run == "" || run == "latest" {
		runs := r.Runs()
		if len(runs) == 0 {
			return nil, fmt.Errorf("E-WEB-003: no recorded run in %s", r.Dir)
		}
		run = runs[0]
	}
	if !runRE.MatchString(run) {
		return nil, fmt.Errorf("E-WEB-003: %q is not a run's folder name", run)
	}
	p, err := live.LoadRun(filepath.Join(r.Dir, run), r.Triage)
	if err != nil {
		return nil, fmt.Errorf("E-WEB-003: %w", err)
	}
	r.mu.Lock()
	old := r.cur
	r.cur = p
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
	p.Seek(p.Start())
	return p, nil
}

// Runs lists the recorded runs, newest first.
func (r *Replays) Runs() []string {
	ents, _ := os.ReadDir(r.Dir)
	var out []string
	for i := len(ents) - 1; i >= 0; i-- {
		e := ents[i]
		if _, err := os.Stat(filepath.Join(r.Dir, e.Name(), "events.jsonl")); err == nil && e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// ReplayCommand is what the demo director asks of the replay.
type ReplayCommand struct {
	// Action: load, seek, play, pause.
	Action string `json:"action"`
	Run    string `json:"run,omitempty"`
	// At: a beat ("first-red", "first-red-10s"), a run time ("+12:30"), "start", "end", or unix ms.
	At    string  `json:"at,omitempty"`
	Speed float64 `json:"speed,omitempty"`
}

// ReplayState is GET /v1/replay.
type ReplayState struct {
	Loaded *live.Replay `json:"loaded,omitempty"`
	Beats  []live.Beat  `json:"beats,omitempty"`
	Runs   []string     `json:"runs"`
}

func (s *Server) replayHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/replay", func(w http.ResponseWriter, _ *http.Request) {
		if s.Replays == nil {
			writeJSON(w, ReplayState{Runs: []string{}})
			return
		}
		st := ReplayState{Runs: s.Replays.Runs()}
		if st.Runs == nil {
			st.Runs = []string{}
		}
		if p := s.Replays.Current(); p != nil {
			st.Loaded, st.Beats = p.Info(), p.Beats()
		}
		writeJSON(w, st)
	})
	mux.HandleFunc("POST /v1/replay", func(w http.ResponseWriter, r *http.Request) {
		if s.Replays == nil {
			http.Error(w, "replays are not set up in this sar-web", http.StatusNotFound)
			return
		}
		if !fromThisMachine(r) {
			http.Error(w, "replays are controlled from the ZBook itself (rubblesar demo)", http.StatusForbidden)
			return
		}
		var c ReplayCommand
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err := json.Unmarshal(b, &c); err != nil {
			http.Error(w, "command: "+err.Error(), http.StatusBadRequest)
			return
		}
		st, err := s.Replays.Do(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, st)
	})
}

// Do carries out a command.
func (r *Replays) Do(c ReplayCommand) (*live.Replay, error) {
	p := r.Current()
	if c.Action == "load" || (p == nil && c.Run != "") || (p != nil && c.Run != "" && c.Run != p.Run) {
		var err error
		if p, err = r.Load(c.Run); err != nil {
			return nil, err
		}
	}
	if p == nil {
		return nil, fmt.Errorf("E-WEB-003: no replay loaded (action load with a run first)")
	}
	if c.At != "" {
		t, ok := p.Beat(c.At)
		if !ok {
			if n, err := strconv.ParseInt(c.At, 10, 64); err == nil {
				t, ok = time.UnixMilli(n), true
			}
		}
		if !ok {
			return nil, fmt.Errorf("E-WEB-003: %q is not a moment of run %s (beats: %s)", c.At, p.Run, beatNames(p))
		}
		p.Seek(t)
	}
	switch c.Action {
	case "play":
		p.Play(c.Speed)
	case "pause":
		p.Pause()
	case "load", "seek", "":
	default:
		return nil, fmt.Errorf("E-WEB-003: unknown action %q (load, seek, play, pause)", c.Action)
	}
	return p.Info(), nil
}

func beatNames(p *live.Player) string {
	seen := map[string]bool{}
	var out []byte
	for _, b := range p.Beats() {
		if seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		if len(out) > 0 {
			out = append(out, ", "...)
		}
		out = append(out, b.Name...)
	}
	return string(out)
}
