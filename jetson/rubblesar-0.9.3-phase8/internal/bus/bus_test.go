package bus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rubblesar/internal/diag"
)

func collect(t *testing.T, b Bus, expr string) (*[]Sample, *sync.Mutex, Subscription) {
	t.Helper()
	var mu sync.Mutex
	var got []Sample
	sub, err := b.Subscribe(expr, func(s Sample) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	return &got, &mu, sub
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemoryRouting(t *testing.T) {
	m := NewMemory()
	defer m.Close()
	all, mu, _ := collect(t, m, "sar/inc/uav/*/telemetry")
	one, mu1, _ := collect(t, m, "sar/inc/uav/scout-1/**")
	pub, _ := m.DeclarePublisher("sar/inc/uav/scout-1/telemetry", QoSTelemetry)
	if err := pub.Put([]byte("a")); err != nil {
		t.Fatal(err)
	}
	_ = m.Put("sar/inc/uav/inv-1/telemetry", []byte("b"), QoSTelemetry)
	_ = m.Put("sar/inc/uav/scout-1/control/reply", []byte("c"), QoSControl)
	_ = m.Put("sar/other/uav/scout-1/telemetry", []byte("d"), QoSTelemetry)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(*all) == 2 })
	waitFor(t, func() bool { mu1.Lock(); defer mu1.Unlock(); return len(*one) == 2 })
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(*all) != 2 || string((*all)[0].Payload) != "a" || string((*all)[1].Payload) != "b" {
		t.Errorf("wildcard subscriber got %v", *all)
	}
	mu.Unlock()
}

func TestMemoryPayloadIsCopied(t *testing.T) {
	m := NewMemory()
	defer m.Close()
	got, mu, _ := collect(t, m, "k")
	buf := []byte("hello")
	_ = m.Put("k", buf, QoSTelemetry)
	buf[0] = 'J'
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(*got) == 1 })
	if string((*got)[0].Payload) != "hello" {
		t.Fatalf("payload aliased: %q", (*got)[0].Payload)
	}
}

func TestMemoryClose(t *testing.T) {
	m := NewMemory()
	_, _, sub := collect(t, m, "k")
	_ = sub.Close()
	_ = sub.Close() // idempotent
	_ = m.Close()
	if err := m.Put("k", nil, QoSTelemetry); diag.CodeOf(err) != "E-BUS-012" {
		t.Fatalf("put after close: %v", err)
	}
	if _, err := m.Subscribe("k", func(Sample) {}); diag.CodeOf(err) != "E-BUS-012" {
		t.Fatalf("subscribe after close: %v", err)
	}
}

func TestDispatcherDropsWhenFull(t *testing.T) {
	block := make(chan struct{})
	d := NewDispatcher(func(Sample) { <-block })
	for i := 0; i < QueueSize+100; i++ {
		d.Push(Sample{})
	}
	if d.Dropped() < 99 {
		t.Fatalf("dropped = %d, want >= 99", d.Dropped())
	}
	close(block)
	d.Stop()
	d.Push(Sample{}) // after stop: must not panic
}

func TestOpenRetry(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var calls atomic.Int32
	open := func(context.Context) (Bus, error) {
		if calls.Add(1) < 3 {
			return nil, diag.New("E-BUS-010", "unreachable")
		}
		return NewMemory(), nil
	}
	b, err := OpenRetry(context.Background(), log, 30*time.Second, false, open)
	if err != nil || b == nil || calls.Load() != 3 {
		t.Fatalf("OpenRetry: b=%v err=%v calls=%d", b, err, calls.Load())
	}

	// Configuration errors are not retried.
	calls.Store(0)
	_, err = OpenRetry(context.Background(), log, 30*time.Second, true, func(context.Context) (Bus, error) {
		calls.Add(1)
		return nil, diag.New("E-BUS-011", "bad config")
	})
	if diag.CodeOf(err) != "E-BUS-011" || calls.Load() != 1 {
		t.Fatalf("config error retried: %v calls=%d", err, calls.Load())
	}

	// Gives up after the timeout when not retrying forever.
	start := time.Now()
	_, err = OpenRetry(context.Background(), log, 1500*time.Millisecond, false, func(context.Context) (Bus, error) {
		return nil, diag.New("E-BUS-010", "unreachable")
	})
	if diag.CodeOf(err) != "E-BUS-010" || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout behaviour: %v after %v", err, time.Since(start))
	}

	// Context cancellation stops a forever retry.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = OpenRetry(ctx, log, time.Second, true, func(context.Context) (Bus, error) {
		return nil, diag.New("E-BUS-010", "unreachable")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline, got %v", err)
	}
}
