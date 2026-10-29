//go:build !nozenoh

package zenoh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
)

// freePort finds a free TCP port on loopback.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// pair opens two peer sessions connected over loopback TCP, no router needed.
func pair(t *testing.T) (*Session, *Session) {
	t.Helper()
	ep := fmt.Sprintf("tcp/127.0.0.1:%d", freePort(t))
	a, err := Open(context.Background(), config.Zenoh{Mode: "peer", Listen: []string{ep}})
	if err != nil {
		t.Fatalf("open listener: %v", err)
	}
	b, err := Open(context.Background(), config.Zenoh{Mode: "peer", Connect: []string{ep}})
	if err != nil {
		a.Close()
		t.Fatalf("open connector: %v", err)
	}
	t.Cleanup(func() { b.Close(); a.Close() })
	time.Sleep(300 * time.Millisecond) // let the peers exchange declarations
	return a, b
}

func TestLibraryVersion(t *testing.T) {
	if v := LibraryVersion(); v == "" || v == "none" {
		t.Fatalf("LibraryVersion = %q", v)
	}
}

func TestPubSubAcrossSessions(t *testing.T) {
	a, b := pair(t)
	var mu sync.Mutex
	var got []bus.Sample
	sub, err := b.Subscribe("sar/test/uav/*/telemetry", func(s bus.Sample) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	time.Sleep(300 * time.Millisecond)

	pub, err := a.DeclarePublisher("sar/test/uav/scout-1/telemetry", bus.QoSControl)
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := pub.Put([]byte(fmt.Sprintf("msg-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// One-off put and an empty payload on a matching key.
	if err := a.Put("sar/test/uav/inv-1/telemetry", nil, bus.QoSControl); err != nil {
		t.Fatal(err)
	}
	// Non-matching key must not arrive.
	_ = a.Put("sar/test/uav/inv-1/control", []byte("x"), bus.QoSControl)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		l := len(got)
		mu.Unlock()
		if l >= n+1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n+1 {
		t.Fatalf("received %d samples, want %d", len(got), n+1)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("msg-%03d", i)
		if got[i].Key != "sar/test/uav/scout-1/telemetry" || !bytes.Equal(got[i].Payload, []byte(want)) {
			t.Fatalf("sample %d = %q on %q, want %q (order must be preserved)", i, got[i].Payload, got[i].Key, want)
		}
	}
	if last := got[n]; last.Key != "sar/test/uav/inv-1/telemetry" || len(last.Payload) != 0 {
		t.Fatalf("one-off put = %+v", last)
	}
}

func TestLargePayload(t *testing.T) {
	a, b := pair(t)
	ch := make(chan []byte, 1)
	sub, err := b.Subscribe("big", func(s bus.Sample) { ch <- s.Payload })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	time.Sleep(300 * time.Millisecond)
	payload := bytes.Repeat([]byte{0xAB, 0xCD, 0x01}, 100_000) // 300 KB: larger than one batch
	if err := a.Put("big", payload, bus.QoSControl); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-ch:
		if !bytes.Equal(p, payload) {
			t.Fatalf("payload corrupted: %d bytes", len(p))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("large payload not received")
	}
}

func TestSubscriberCloseStopsDelivery(t *testing.T) {
	a, b := pair(t)
	var mu sync.Mutex
	count := 0
	sub, err := b.Subscribe("k", func(bus.Sample) { mu.Lock(); count++; mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = a.Put("k", []byte("1"), bus.QoSControl)
	time.Sleep(300 * time.Millisecond)
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	_ = sub.Close()
	_ = a.Put("k", []byte("2"), bus.QoSControl)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestInvalidKeyRejected(t *testing.T) {
	a, _ := pair(t)
	if _, err := a.DeclarePublisher("bad//key", bus.QoSControl); diag.CodeOf(err) != "E-BUS-004" {
		t.Fatalf("want E-BUS-004, got %v", err)
	}
	if err := a.Put("", nil, bus.QoSControl); diag.CodeOf(err) != "E-BUS-003" {
		t.Fatalf("want E-BUS-003, got %v", err)
	}
	if _, err := a.Subscribe("a/**/**/b?", func(bus.Sample) {}); err == nil {
		t.Fatal("want error for malformed key expression")
	}
}

func TestClientWithoutRouterFails(t *testing.T) {
	ep := fmt.Sprintf("tcp/127.0.0.1:%d", freePort(t)) // nothing listens here
	start := time.Now()
	_, err := Open(context.Background(), config.Zenoh{Mode: "client", Connect: []string{ep}})
	if diag.CodeOf(err) != "E-BUS-010" {
		t.Fatalf("want E-BUS-010, got %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("open took %v", time.Since(start))
	}
}

func TestBadConfigRejected(t *testing.T) {
	_, err := Open(context.Background(), config.Zenoh{Mode: "client", Connect: []string{"nonsense"}})
	if c := diag.CodeOf(err); c != "E-BUS-011" && c != "E-BUS-010" {
		t.Fatalf("want E-BUS-011 or E-BUS-010, got %v", err)
	}
}

func TestCloseTwiceAndUseAfterClose(t *testing.T) {
	a, _ := pair(t)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Put("k", nil, bus.QoSControl); diag.CodeOf(err) != "E-BUS-012" {
		t.Fatalf("put after close: %v", err)
	}
}
