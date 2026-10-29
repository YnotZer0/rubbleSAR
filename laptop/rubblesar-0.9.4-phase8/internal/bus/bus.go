// Package bus is RubbleSAR's messaging abstraction. Applications only see this
// interface; the Zenoh implementation lives in internal/bus/zenoh and an
// in-memory implementation (for unit tests) lives here.
package bus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"rubblesar/internal/diag"
)

// Priority mirrors Zenoh's eight priorities (1 = highest).
type Priority int

const (
	PriorityRealTime        Priority = 1
	PriorityInteractiveHigh Priority = 2
	PriorityInteractiveLow  Priority = 3
	PriorityDataHigh        Priority = 4
	PriorityData            Priority = 5
	PriorityDataLow         Priority = 6
	PriorityBackground      Priority = 7
)

// Congestion says what to do when the network cannot keep up.
type Congestion int

const (
	// CongestionDrop discards the message (telemetry: newest wins).
	CongestionDrop Congestion = iota
	// CongestionBlock waits until it can be sent (commands, detections).
	CongestionBlock
)

// PubOptions are per-publisher quality-of-service settings.
type PubOptions struct {
	Priority   Priority
	Congestion Congestion
	// Express disables batching for lower latency.
	Express bool
}

// Telemetry and control defaults used across the code base.
var (
	QoSTelemetry = PubOptions{Priority: PriorityData, Congestion: CongestionDrop}
	QoSControl   = PubOptions{Priority: PriorityRealTime, Congestion: CongestionBlock, Express: true}
	// Detections: high priority and never dropped by zenoh (the outbox meters them).
	QoSDetection = PubOptions{Priority: PriorityDataHigh, Congestion: CongestionBlock}
	// Acknowledgements are tiny and let the drone free its queue.
	QoSAck = PubOptions{Priority: PriorityInteractiveHigh, Congestion: CongestionBlock, Express: true}
	// Simulation truth: background, may be dropped.
	QoSSim = PubOptions{Priority: PriorityBackground, Congestion: CongestionDrop}
	// QoSCasualty: casualty records and task lists between ZBook programs (reliable, not on the radio).
	QoSCasualty = PubOptions{Priority: PriorityDataHigh, Congestion: CongestionBlock}
)

// Sample is one received message.
type Sample struct {
	Key      string
	Payload  []byte
	Received time.Time
}

// Handler processes samples. It runs on a dedicated goroutine per
// subscription, never on a network thread, so it may block briefly.
type Handler func(Sample)

// Publisher sends on one fixed key.
type Publisher interface {
	Key() string
	Put(payload []byte) error
	Close() error
}

// Subscription receives samples matching a key expression.
type Subscription interface {
	Key() string
	// Dropped counts samples discarded because the handler fell behind.
	Dropped() uint64
	Close() error
}

// Info describes an open bus.
type Info struct {
	Backend   string // "zenoh" or "memory"
	Mode      string
	Endpoints []string
}

// Bus is an open messaging session.
type Bus interface {
	DeclarePublisher(key string, o PubOptions) (Publisher, error)
	Put(key string, payload []byte, o PubOptions) error
	Subscribe(keyExpr string, h Handler) (Subscription, error)
	Info() Info
	Close() error
}

// QueueSize is the per-subscription buffer between the network and the handler.
const QueueSize = 4096

// Dispatcher decouples network callbacks from handlers. Push never blocks.
type Dispatcher struct {
	ch      chan Sample
	h       Handler
	dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

// NewDispatcher starts the handler goroutine.
func NewDispatcher(h Handler) *Dispatcher {
	d := &Dispatcher{ch: make(chan Sample, QueueSize), h: h, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for s := range d.ch {
			d.h(s)
		}
	}()
	return d
}

// Push queues a sample or counts it as dropped if the queue is full.
func (d *Dispatcher) Push(s Sample) {
	defer func() { _ = recover() }() // push after Stop: ignore
	select {
	case d.ch <- s:
	default:
		d.dropped.Add(1)
	}
}

// Dropped returns the number of discarded samples.
func (d *Dispatcher) Dropped() uint64 { return d.dropped.Load() }

// Stop drains and stops the handler goroutine. Safe to call more than once.
func (d *Dispatcher) Stop() {
	d.once.Do(func() { close(d.ch) })
	<-d.done
}

// OpenFunc opens one session attempt.
type OpenFunc func(ctx context.Context) (Bus, error)

// OpenRetry calls open until it succeeds. With forever=false it gives up
// after timeout. Each failure is logged with its error code.
func OpenRetry(ctx context.Context, log *slog.Logger, timeout time.Duration, forever bool, open OpenFunc) (Bus, error) {
	deadline := time.Now().Add(timeout)
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		b, err := open(ctx)
		if err == nil {
			if attempt > 1 {
				log.Info("connected to zenoh after retries", "attempts", attempt)
			}
			return b, nil
		}
		if c := diag.CodeOf(err); c != "E-BUS-010" && c != "E-BUS-001" {
			return nil, err // configuration errors will not fix themselves
		}
		if !forever && time.Now().Add(backoff).After(deadline) {
			return nil, err
		}
		log.Warn("zenoh connection attempt failed; retrying", "attempt", attempt, "retry_in", backoff, "err", err, "hint", diag.HintOf(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}
