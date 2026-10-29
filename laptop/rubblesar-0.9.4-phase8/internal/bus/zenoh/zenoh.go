//go:build !nozenoh

// Package zenoh implements bus.Bus on top of zenoh-c 1.x through cgo.
//
// Build requirements: cgo enabled, gcc, pkg-config and libzenohc installed
// (scripts/*/10-zenoh-c.sh). Build with -tags nozenoh to get a stub that
// compiles anywhere but cannot open a session (E-BUS-002).
package zenoh

/*
#cgo pkg-config: zenohc
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"context"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/version"
)

// Available reports whether this binary can open zenoh sessions.
const Available = true

var logOnce sync.Once

func init() {
	version.ZenohC = C.GoString(C.sar_zenoh_c_version())
}

// LibraryVersion returns the zenoh-c version this binary was compiled against.
func LibraryVersion() string { return C.GoString(C.sar_zenoh_c_version()) }

// Session is a zenoh session implementing bus.Bus.
type Session struct {
	mu     sync.RWMutex
	s      *C.sar_session
	closed bool
	info   bus.Info
	pubs   map[*publisher]struct{}
	subs   map[*subscriber]struct{}
}

// Open opens one session using the [zenoh] section of a config.
// Zenoh's own log level follows RUST_LOG (default "error").
func Open(_ context.Context, z config.Zenoh) (*Session, error) {
	logOnce.Do(func() {
		f := C.CString("error")
		C.sar_init_logging(f)
		C.free(unsafe.Pointer(f))
	})
	js, err := z.ZenohJSON5()
	if err != nil {
		return nil, err
	}
	cjs := C.CString(js)
	defer C.free(unsafe.Pointer(cjs))
	var s *C.sar_session
	var stage C.int
	if rc := C.sar_open(cjs, &s, &stage); rc != 0 {
		if stage == C.SAR_STAGE_CONFIG {
			return nil, diag.New("E-BUS-011", "zenoh config rejected (code %d): %s", int(rc), js)
		}
		if z.Mode == "client" {
			return nil, diag.New("E-BUS-010", "could not connect to zenoh router at %s (zenoh code %d)", z.Target(), int(rc))
		}
		return nil, diag.New("E-BUS-001", "zenoh_open failed in %s mode (code %d); config %s", z.Mode, int(rc), js)
	}
	eps := z.Connect
	if z.Mode == "peer" {
		eps = append(append([]string{}, z.Listen...), z.Connect...)
	}
	if z.ConfigFile != "" {
		eps = []string{"config " + z.ConfigFile}
	}
	return &Session{
		s:    s,
		info: bus.Info{Backend: "zenoh " + LibraryVersion(), Mode: z.Mode, Endpoints: eps},
		pubs: map[*publisher]struct{}{},
		subs: map[*subscriber]struct{}{},
	}, nil
}

// Opener adapts Open for bus.OpenRetry.
func Opener(z config.Zenoh) bus.OpenFunc {
	return func(ctx context.Context) (bus.Bus, error) {
		s, err := Open(ctx, z)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// Info implements bus.Bus.
func (s *Session) Info() bus.Info { return s.info }

func checkKey(k string) error {
	if k == "" {
		return diag.New("E-BUS-003", "empty key expression")
	}
	return nil
}

type publisher struct {
	sess   *Session
	key    string
	mu     sync.Mutex
	p      *C.sar_publisher
	closed bool
}

func (p *publisher) Key() string { return p.key }

func (p *publisher) Put(payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return diag.New("E-BUS-012", "publisher %s closed", p.key)
	}
	var ptr *C.uint8_t
	if len(payload) > 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&payload[0]))
	}
	if rc := C.sar_publisher_put(p.p, ptr, C.size_t(len(payload))); rc != 0 {
		return diag.New("E-BUS-006", "put on %s failed (zenoh code %d)", p.key, int(rc))
	}
	return nil
}

func (p *publisher) Close() error {
	p.sess.mu.Lock()
	delete(p.sess.pubs, p)
	p.sess.mu.Unlock()
	return p.close()
}

func (p *publisher) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	C.sar_undeclare_publisher(p.p)
	p.p = nil
	return nil
}

// DeclarePublisher implements bus.Bus.
func (s *Session) DeclarePublisher(key string, o bus.PubOptions) (bus.Publisher, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, diag.New("E-BUS-012", "session closed")
	}
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	var p *C.sar_publisher
	if rc := C.sar_declare_publisher(s.s, ck, prio(o.Priority), block(o.Congestion), b2i(o.Express), &p); rc != 0 {
		return nil, diag.New("E-BUS-004", "declare publisher %q failed (zenoh code %d)", key, int(rc))
	}
	pub := &publisher{sess: s, key: key, p: p}
	s.pubs[pub] = struct{}{}
	return pub, nil
}

// Put implements bus.Bus (one-off publication without a declared publisher).
func (s *Session) Put(key string, payload []byte, o bus.PubOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return diag.New("E-BUS-012", "session closed")
	}
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	var ptr *C.uint8_t
	if len(payload) > 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&payload[0]))
	}
	if rc := C.sar_put(s.s, ck, ptr, C.size_t(len(payload)), prio(o.Priority), block(o.Congestion), b2i(o.Express)); rc != 0 {
		return diag.New("E-BUS-006", "put on %s failed (zenoh code %d)", key, int(rc))
	}
	return nil
}

type subscriber struct {
	sess   *Session
	expr   string
	d      *bus.Dispatcher
	handle cgo.Handle
	mu     sync.Mutex
	sub    *C.sar_subscriber
	closed bool
}

func (s *subscriber) Key() string     { return s.expr }
func (s *subscriber) Dropped() uint64 { return s.d.Dropped() }

func (s *subscriber) Close() error {
	s.sess.mu.Lock()
	delete(s.sess.subs, s)
	s.sess.mu.Unlock()
	return s.close()
}

func (s *subscriber) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sub := s.sub
	s.sub = nil
	s.mu.Unlock()
	C.sar_undeclare_subscriber(sub) // zenoh calls the drop callback, which frees the handle
	s.d.Stop()
	return nil
}

// Subscribe implements bus.Bus.
func (s *Session) Subscribe(expr string, h bus.Handler) (bus.Subscription, error) {
	if err := checkKey(expr); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, diag.New("E-BUS-012", "session closed")
	}
	sub := &subscriber{sess: s, expr: expr, d: bus.NewDispatcher(h)}
	sub.handle = cgo.NewHandle(sub)
	ck := C.CString(expr)
	defer C.free(unsafe.Pointer(ck))
	var cs *C.sar_subscriber
	if rc := C.sar_declare_subscriber(s.s, ck, C.uintptr_t(sub.handle), &cs); rc != 0 {
		sub.d.Stop()
		// zenoh drops the closure on failure, which deletes the handle.
		return nil, diag.New("E-BUS-005", "declare subscriber %q failed (zenoh code %d)", expr, int(rc))
	}
	sub.sub = cs
	s.subs[sub] = struct{}{}
	return sub, nil
}

// Close undeclares everything and closes the session. Safe to call twice.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pubs, subs := s.pubs, s.subs
	s.pubs, s.subs = nil, nil
	s.mu.Unlock()
	for p := range pubs {
		_ = p.close()
	}
	for sub := range subs {
		_ = sub.close()
	}
	if rc := C.sar_close(s.s); rc != 0 {
		return diag.New("E-BUS-001", "zenoh close returned code %d", int(rc))
	}
	return nil
}

//export sarGoOnSample
func sarGoOnSample(ctx C.uintptr_t, key *C.char, keyLen C.size_t, data *C.uint8_t, dataLen C.size_t) {
	sub, ok := cgo.Handle(ctx).Value().(*subscriber)
	if !ok {
		return
	}
	k := C.GoStringN(key, C.int(keyLen))
	var payload []byte
	if dataLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(data), C.int(dataLen))
	} else {
		payload = []byte{}
	}
	sub.d.Push(bus.Sample{Key: k, Payload: payload, Received: time.Now()})
}

//export sarGoOnDrop
func sarGoOnDrop(ctx C.uintptr_t) {
	cgo.Handle(ctx).Delete()
}

func prio(p bus.Priority) C.int {
	if p < bus.PriorityRealTime || p > bus.PriorityBackground {
		return C.int(bus.PriorityData)
	}
	return C.int(p)
}

func block(c bus.Congestion) C.int {
	if c == bus.CongestionBlock {
		return 1
	}
	return 0
}

func b2i(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Compile-time check.
var _ bus.Bus = (*Session)(nil)
