package bus

import (
	"sync"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/keys"
)

// Memory is an in-process bus with Zenoh-like key matching. It is used by unit
// tests and by sar-check's self-test, never across machines.
type Memory struct {
	mu     sync.RWMutex
	subs   map[*memSub]struct{}
	closed bool
}

// NewMemory returns an empty in-memory bus.
func NewMemory() *Memory { return &Memory{subs: map[*memSub]struct{}{}} }

type memSub struct {
	m    *Memory
	expr string
	d    *Dispatcher
}

func (s *memSub) Key() string     { return s.expr }
func (s *memSub) Dropped() uint64 { return s.d.Dropped() }
func (s *memSub) Close() error {
	s.m.mu.Lock()
	_, ok := s.m.subs[s]
	delete(s.m.subs, s)
	s.m.mu.Unlock()
	if ok {
		s.d.Stop()
	}
	return nil
}

type memPub struct {
	m   *Memory
	key string
	o   PubOptions
}

func (p *memPub) Key() string              { return p.key }
func (p *memPub) Put(payload []byte) error { return p.m.Put(p.key, payload, p.o) }
func (p *memPub) Close() error             { return nil }

// DeclarePublisher implements Bus.
func (m *Memory) DeclarePublisher(key string, o PubOptions) (Publisher, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, diag.New("E-BUS-012", "memory bus closed")
	}
	return &memPub{m: m, key: key, o: o}, nil
}

// Put implements Bus. The payload is copied for each subscriber.
func (m *Memory) Put(key string, payload []byte, _ PubOptions) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return diag.New("E-BUS-012", "memory bus closed")
	}
	now := time.Now()
	for s := range m.subs {
		if keys.Match(s.expr, key) {
			cp := append([]byte(nil), payload...)
			s.d.Push(Sample{Key: key, Payload: cp, Received: now})
		}
	}
	return nil
}

// Subscribe implements Bus.
func (m *Memory) Subscribe(expr string, h Handler) (Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, diag.New("E-BUS-012", "memory bus closed")
	}
	s := &memSub{m: m, expr: expr, d: NewDispatcher(h)}
	m.subs[s] = struct{}{}
	return s, nil
}

// Info implements Bus.
func (m *Memory) Info() Info { return Info{Backend: "memory", Mode: "in-process"} }

// Close implements Bus.
func (m *Memory) Close() error {
	m.mu.Lock()
	subs := m.subs
	m.subs = map[*memSub]struct{}{}
	m.closed = true
	m.mu.Unlock()
	for s := range subs {
		s.d.Stop()
	}
	return nil
}
