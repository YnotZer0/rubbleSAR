package web

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/live"
)

// Feed is a source of pictures: the live run (a live.Hub) or a replay.
type Feed interface {
	Snapshot() *live.Picture
	Subscribe() (<-chan struct{}, func())
	Seq() uint64
}

//go:embed ops
var opsFS embed.FS

// maxStreams caps the open event streams (each browser tab holds one).
const maxStreams = 48

func (s *Server) opsHandlers(mux *http.ServeMux) {
	sub, _ := fs.Sub(opsFS, "ops")
	static := http.FileServer(http.FS(sub))
	if dir := os.Getenv("RUBBLESAR_OPS_DIR"); dir != "" {
		// Development: the page's files from a folder, to change them without rebuilding.
		static = http.FileServer(http.Dir(dir))
	}
	mux.HandleFunc("GET /ops", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ops/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ops/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ops/fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.StripPrefix("/ops", static).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /v1/scene", s.scene)
	mux.HandleFunc("GET /v1/picture", s.picture)
	mux.HandleFunc("GET /v1/timeline", s.timeline)
	mux.HandleFunc("GET /v1/stream", s.stream)
	mux.HandleFunc("GET /v1/crop/{id}", s.crop)
	mux.HandleFunc("GET /v1/thermal/{id}", s.thermal)
	s.presentHandlers(mux)
	s.replayHandlers(mux)
}

// crops is where a request's crops come from: the live run's, or the replay's (src=replay).
func (s *Server) crops(r *http.Request) *live.Crops {
	if r.URL.Query().Get("src") == "replay" && s.Replays != nil {
		if p := s.Replays.Current(); p != nil {
			return p.Crops
		}
		return nil
	}
	return s.Crops
}

func (s *Server) sceneNow() *Scene {
	s.sceneOnce.Do(func() {
		s.scenes = &sceneCache{build: func() *Scene {
			if s.Scene != nil {
				return s.Scene()
			}
			return &Scene{Version: s.Version, Rubble: [][][2]float64{}, Sites: []SceneSite{}, Teams: []SceneTeam{}}
		}}
	})
	return s.scenes.get()
}

func (s *Server) scene(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.sceneNow())
}

// feed is the picture source a request asks for: src=live (default) or src=replay.
func (s *Server) feed(src string) Feed {
	switch src {
	case "replay":
		if s.Replays != nil {
			if p := s.Replays.Current(); p != nil {
				return p
			}
		}
		return nil
	default:
		if s.Live != nil {
			return s.Live
		}
		return nil
	}
}

var offPicture = func(inc, why string) *live.Picture {
	return &live.Picture{Incident: inc, Mode: "off", Bus: why, Drones: []*live.Drone{}, Casualties: []*live.Casualty{}, Tasks: []*live.Task{},
		Teams: []*live.Team{}, Assignments: []*live.Assignment{}, Detections: []*live.Det{}, Timeline: []*live.Item{}, Chats: []*live.Chat{}}
}

func (s *Server) snapshot(src string) *live.Picture {
	if f := s.feed(src); f != nil {
		return f.Snapshot()
	}
	why := "the live view is not running in this sar-web"
	if src == "replay" {
		why = "no replay loaded"
	}
	return offPicture(s.sceneNow().Incident, why)
}

func (s *Server) picture(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot(r.URL.Query().Get("src")))
}

// timeline is the whole run's timeline (src=live or replay).
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	f := s.feed(r.URL.Query().Get("src"))
	if t, ok := f.(interface{ Timeline() []*live.Item }); ok && f != nil {
		writeJSON(w, t.Timeline())
		return
	}
	writeJSON(w, []*live.Item{})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// encoded caches the JSON of a feed's latest picture, so that many viewers cost one encoding.
type encoded struct {
	mu   sync.Mutex
	feed Feed
	seq  uint64
	b    []byte
	at   time.Time
}

func (e *encoded) get(f Feed, maxAge time.Duration) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	seq := f.Seq()
	if e.feed == f && e.b != nil && e.seq == seq && time.Since(e.at) < maxAge {
		return e.b
	}
	b, _ := json.Marshal(f.Snapshot())
	e.feed, e.seq, e.b, e.at = f, seq, b, time.Now()
	return b
}

// eventWriter writes server-sent events, gzip-compressed when the browser accepts it.
type eventWriter struct {
	w  io.Writer
	gz *gzip.Writer
	rc *http.ResponseController
}

func newEventWriter(w http.ResponseWriter, r *http.Request) *eventWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ew := &eventWriter{w: w, rc: http.NewResponseController(w)}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		ew.gz, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
		ew.w = ew.gz
	}
	w.WriteHeader(http.StatusOK)
	return ew
}

func (e *eventWriter) send(event string, data []byte) error {
	var err error
	if event != "" {
		_, err = fmt.Fprintf(e.w, "event: %s\n", event)
	}
	if err == nil {
		_, err = e.w.Write([]byte("data: "))
	}
	if err == nil {
		_, err = e.w.Write(data)
	}
	if err == nil {
		_, err = e.w.Write([]byte("\n\n"))
	}
	if err != nil {
		return err
	}
	return e.flush()
}

func (e *eventWriter) comment(text string) error {
	if _, err := fmt.Fprintf(e.w, ": %s\n\n", text); err != nil {
		return err
	}
	return e.flush()
}

func (e *eventWriter) flush() error {
	if e.gz != nil {
		if err := e.gz.Flush(); err != nil {
			return err
		}
	}
	return e.rc.Flush()
}

func (e *eventWriter) close() {
	if e.gz != nil {
		_ = e.gz.Close()
	}
}

// stream sends the picture as server-sent events ("picture"), at most every
// 0.5 s (a replay: every 0.1 s), and in presenter mode the cues ("cue").
//
//	GET /v1/stream?src=live|replay|present
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if n := s.streams.Add(1); n > maxStreams {
		s.streams.Add(-1)
		http.Error(w, "too many open views", http.StatusServiceUnavailable)
		return
	}
	defer s.streams.Add(-1)
	src := r.URL.Query().Get("src")
	present := src == "present"
	ew := newEventWriter(w, r)
	defer ew.close()
	ctx := r.Context()
	var cueCh <-chan struct{}
	if present && s.Present != nil {
		ch, cancel := s.Present.Subscribe()
		defer cancel()
		cueCh = ch
		s.presenters.Add(1)
		defer s.presenters.Add(-1)
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var (
		cur      Feed
		changes  <-chan struct{}
		unsub    func()
		lastSent time.Time
		pending  = true
		cueSeq   uint64
		timer    *time.Timer
		timerC   <-chan time.Time
	)
	defer func() {
		if unsub != nil {
			unsub()
		}
	}()
	want := func() string {
		if present && s.Present != nil {
			return s.Present.Source()
		}
		return src
	}
	attach := func() {
		f := s.feed(want())
		if f == cur && f != nil {
			return
		}
		if unsub != nil {
			unsub()
			unsub, changes = nil, nil
		}
		cur = f
		if f != nil {
			changes, unsub = f.Subscribe()
		}
		pending = true
	}
	sendCue := func() error {
		if !present || s.Present == nil {
			return nil
		}
		c, seq := s.Present.Cue()
		if seq == cueSeq {
			return nil
		}
		cueSeq = seq
		b, _ := json.Marshal(c)
		return ew.send("cue", b)
	}
	attach()
	if err := sendCue(); err != nil {
		return
	}
	retry := time.NewTicker(2 * time.Second) // a feed appearing later (zenohd up, replay loaded)
	defer retry.Stop()
	for {
		if pending {
			gap := 500 * time.Millisecond
			if want() == "replay" {
				gap = 100 * time.Millisecond
			}
			if wait := gap - time.Since(lastSent); wait > 0 {
				if timer == nil {
					timer = time.NewTimer(wait)
					timerC = timer.C
				}
			} else {
				var b []byte
				if cur != nil {
					b = s.enc(want()).get(cur, 5*time.Second)
				} else {
					b, _ = json.Marshal(s.snapshot(want()))
				}
				if err := ew.send("picture", b); err != nil {
					return
				}
				lastSent, pending = time.Now(), false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
			pending = true
		case <-timerC:
			timer, timerC = nil, nil
		case <-cueCh:
			if err := sendCue(); err != nil {
				return
			}
			attach()
		case <-retry.C:
			attach()
		case <-heartbeat.C:
			if err := ew.comment("ping"); err != nil {
				return
			}
			pending = pending || cur == nil
		}
	}
}

func (s *Server) enc(src string) *encoded {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if s.encs == nil {
		s.encs = map[string]*encoded{}
	}
	e := s.encs[src]
	if e == nil {
		e = &encoded{}
		s.encs[src] = e
	}
	return e
}

func (s *Server) crop(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".jpg")
	c := s.crops(r)
	if c == nil || !live.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	b, ok := c.Visible(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// thermal is a detection's thermal crop in false colour (lo and hi in degrees C, default from the crop).
func (s *Server) thermal(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".png")
	c := s.crops(r)
	if c == nil || !live.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	raw, ok := c.Thermal(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	lo, hi := math.NaN(), math.NaN()
	if v, err := strconv.ParseFloat(q.Get("lo"), 64); err == nil {
		lo = v
	}
	if v, err := strconv.ParseFloat(q.Get("hi"), 64); err == nil {
		hi = v
	}
	b, err := live.ThermalView(raw, lo, hi, 128)
	if err != nil {
		http.Error(w, "thermal crop: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}
