package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Cue is what the presenter page (/ops/?present=1) shows: which picture
// (live or a replay), the chapter's title and caption, an explanation panel,
// where the map looks and an optional full-screen card. The demo director
// (rubblesar demo) sets it, chapter by chapter.
type Cue struct {
	Seq uint64 `json:"seq"`
	// At: when the cue was set (unix ms).
	At int64 `json:"at"`
	// Source: "live" or "replay".
	Source  string `json:"source"`
	Chapter string `json:"chapter,omitempty"`
	Index   int    `json:"index,omitempty"` // 1-based chapter number
	Count   int    `json:"count,omitempty"`
	Title   string `json:"title,omitempty"`
	Caption string `json:"caption,omitempty"`
	// Panel: the explanation panel ("" = the operator view's own panels); Data its parameters.
	Panel string         `json:"panel,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
	Map   *MapCue        `json:"map,omitempty"`
	Card  *Card          `json:"card,omitempty"`
	// Focus: a casualty (or drone) to highlight.
	Focus string `json:"focus,omitempty"`
	// Clock: "mission" shows the run time and replay speed.
	Clock string `json:"clock,omitempty"`
	// Slate: a take's clapperboard line (shown large for a second or two, to line recordings up).
	Slate string `json:"slate,omitempty"`
}

// MapCue moves the map.
type MapCue struct {
	// Mode: fit (everything), view (Lat, Lon, Zoom), follow (drone Target), site, casualty (Target).
	Mode   string  `json:"mode"`
	Lat    float64 `json:"lat,omitempty"`
	Lon    float64 `json:"lon,omitempty"`
	Zoom   float64 `json:"zoom,omitempty"`
	Target string  `json:"target,omitempty"`
	// Ms: how long the camera takes to get there.
	Ms int `json:"ms,omitempty"`
}

// Card is a full-screen card over the map (a title, a section, the end).
type Card struct {
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Lines    []string `json:"lines,omitempty"`
}

// Presenter holds the current cue. Safe for concurrent use.
type Presenter struct {
	mu   sync.Mutex
	cue  Cue
	seq  uint64
	subs map[chan struct{}]bool
}

// NewPresenter starts with the live picture and no chapter.
func NewPresenter() *Presenter {
	return &Presenter{cue: Cue{Source: "live"}, subs: map[chan struct{}]bool{}}
}

// Set replaces the cue.
func (p *Presenter) Set(c Cue) Cue {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.Source != "replay" {
		c.Source = "live"
	}
	p.seq++
	c.Seq, c.At = p.seq, time.Now().UnixMilli()
	p.cue = c
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return c
}

// Cue is the current cue and its number.
func (p *Presenter) Cue() (Cue, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cue, p.seq
}

// Source is the picture the presenter shows.
func (p *Presenter) Source() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cue.Source
}

// Subscribe returns a channel that receives a value after each new cue.
func (p *Presenter) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs[ch] = true
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// fromThisMachine: the director's control requests are accepted only from the ZBook itself.
func fromThisMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) presentHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/present", func(w http.ResponseWriter, _ *http.Request) {
		if s.Present == nil {
			http.NotFound(w, nil)
			return
		}
		c, _ := s.Present.Cue()
		writeJSON(w, map[string]any{"cue": c, "presenters": s.presenters.Load(), "streams": s.streams.Load()})
	})
	mux.HandleFunc("POST /v1/present", func(w http.ResponseWriter, r *http.Request) {
		if s.Present == nil {
			http.NotFound(w, r)
			return
		}
		if !fromThisMachine(r) {
			http.Error(w, "the presenter is controlled from the ZBook itself (rubblesar demo)", http.StatusForbidden)
			return
		}
		var c Cue
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err := json.Unmarshal(b, &c); err != nil {
			http.Error(w, "cue: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"cue": s.Present.Set(c), "presenters": s.presenters.Load()})
	})
}
