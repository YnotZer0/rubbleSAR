package edge

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/version"
)

// Admin serves edge-eye's local HTTP API (default 127.0.0.1:9101):
//
//	GET  /healthz            200 "ok" once connected, 503 while connecting
//	GET  /v1/status          JSON status of every drone
//	POST /v1/link/budget     {"uav":"all"|"<id>", "kbps":64} or {"bps":400}
//	GET  /debug/vars         expvar counters
type Admin struct {
	log     *slog.Logger
	srv     *http.Server
	ln      net.Listener
	rt      atomic.Pointer[Runtime]
	stateMu sync.Mutex
	state   string
	lastErr string
	started time.Time
}

// StartAdmin binds the listener immediately so a port clash is reported at once.
func StartAdmin(addr string, log *slog.Logger) (*Admin, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, diag.Wrap("E-EDGE-001", err, "listen on %s", addr)
	}
	a := &Admin{log: log, ln: ln, state: "starting", started: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /v1/status", a.status)
	mux.HandleFunc("POST /v1/link/budget", a.budget)
	mux.Handle("GET /debug/vars", expvar.Handler())
	a.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := a.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin API stopped", "err", err)
		}
	}()
	log.Info("admin API listening", "addr", ln.Addr().String())
	return a, nil
}

// Addr returns the bound address (useful when listening on port 0).
func (a *Admin) Addr() string { return a.ln.Addr().String() }

// SetState records the connection state shown by /healthz and /v1/status.
func (a *Admin) SetState(state string, err error) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.state = state
	a.lastErr = ""
	if err != nil {
		a.lastErr = err.Error()
	}
}

// SetRuntime attaches the running runtime.
func (a *Admin) SetRuntime(r *Runtime) { a.rt.Store(r) }

// Close stops the server.
func (a *Admin) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.srv.Shutdown(ctx)
}

func (a *Admin) snapshotState() (string, string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.state, a.lastErr
}

func (a *Admin) health(w http.ResponseWriter, _ *http.Request) {
	state, lastErr := a.snapshotState()
	if state != "running" {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(state + " " + lastErr + "\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

type statusDoc struct {
	Version   string  `json:"version"`
	State     string  `json:"state"`
	LastError string  `json:"last_error,omitempty"`
	Process   string  `json:"process_uptime"`
	Runtime   *Status `json:"runtime,omitempty"`
}

func (a *Admin) status(w http.ResponseWriter, _ *http.Request) {
	state, lastErr := a.snapshotState()
	doc := statusDoc{Version: version.String("edge-eye"), State: state, LastError: lastErr,
		Process: time.Since(a.started).Truncate(time.Second).String()}
	if rt := a.rt.Load(); rt != nil {
		s := rt.Status()
		doc.Runtime = &s
	}
	writeJSON(w, http.StatusOK, doc)
}

// BudgetRequest is the body of POST /v1/link/budget.
type BudgetRequest struct {
	UAV  string  `json:"uav"`
	Kbps *uint32 `json:"kbps,omitempty"`
	Bps  *uint32 `json:"bps,omitempty"`
}

// BudgetResponse is its reply.
type BudgetResponse struct {
	OK    bool     `json:"ok"`
	UAVs  []string `json:"uavs,omitempty"`
	Bps   uint32   `json:"bps"`
	Error string   `json:"error,omitempty"`
	Hint  string   `json:"hint,omitempty"`
}

func (a *Admin) budget(w http.ResponseWriter, r *http.Request) {
	rt := a.rt.Load()
	if rt == nil {
		writeJSON(w, http.StatusServiceUnavailable, BudgetResponse{Error: "edge-eye is not connected yet"})
		return
	}
	var req BudgetRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		e := diag.Wrap("E-EDGE-003", err, "bad JSON body")
		writeJSON(w, http.StatusBadRequest, BudgetResponse{Error: e.Error(), Hint: `body: {"uav":"all","kbps":64}`})
		return
	}
	var bps uint32
	switch {
	case req.Bps != nil && req.Kbps != nil:
		writeJSON(w, http.StatusBadRequest, BudgetResponse{Error: "E-EDGE-003: give kbps or bps, not both"})
		return
	case req.Bps != nil:
		bps = *req.Bps
	case req.Kbps != nil:
		if *req.Kbps > 4_000_000 {
			writeJSON(w, http.StatusBadRequest, BudgetResponse{Error: "E-EDGE-003: kbps too large"})
			return
		}
		bps = *req.Kbps * 1000
	default:
		writeJSON(w, http.StatusBadRequest, BudgetResponse{Error: "E-EDGE-003: kbps or bps is required (0 = unlimited)"})
		return
	}
	ids, err := rt.SetBudget(req.UAV, bps)
	if err != nil {
		writeJSON(w, http.StatusNotFound, BudgetResponse{Error: err.Error(), Hint: diag.HintOf(err)})
		return
	}
	writeJSON(w, http.StatusOK, BudgetResponse{OK: true, UAVs: ids, Bps: bps})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
