package linkbudget

import (
	"math"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) adv(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestUnlimited(t *testing.T) {
	c := newClock()
	b := New(0, c.now)
	for i := 0; i < 1000; i++ {
		if !b.Allow(10_000) {
			t.Fatal("unlimited bucket refused")
		}
	}
	if s := b.Stats(); s.Dropped != 0 || s.BudgetBps != 0 {
		t.Fatalf("stats %+v", s)
	}
}

// Over a long run the delivered byte rate must match the budget.
func TestAverageRateEnforced(t *testing.T) {
	for _, bps := range []uint32{400, 1000, 8000, 64000, 256000} {
		c := newClock()
		b := New(bps, c.now)
		const msg = 170
		sent := 0
		const secs = 600
		for i := 0; i < secs*10; i++ { // offer 10 msgs/s, far above most budgets
			if b.Allow(msg) {
				sent += msg
			}
			c.adv(100 * time.Millisecond)
		}
		want := float64(bps) / 8 * secs
		offered := float64(msg * 10 * secs)
		want = math.Min(want, offered)
		if got := float64(sent); math.Abs(got-want)/want > 0.03 {
			t.Errorf("%d bps: delivered %.0f bytes, want about %.0f", bps, got, want)
		}
	}
}

func TestLargeMessageAllowedWhenNotInDebt(t *testing.T) {
	c := newClock()
	b := New(8000, c.now) // 1000 B/s
	if !b.Allow(10_000) {
		t.Fatal("first large message should pass (bucket full)")
	}
	if b.Allow(10) {
		t.Fatal("bucket in debt must refuse")
	}
	c.adv(9 * time.Second)
	if b.Allow(10) {
		t.Fatal("still in debt after 9 s at 1000 B/s")
	}
	c.adv(1100 * time.Millisecond)
	if !b.Allow(10) {
		t.Fatal("debt repaid after ~10 s, should allow")
	}
}

func TestSetKbpsTakesEffectImmediately(t *testing.T) {
	c := newClock()
	b := New(1_000_000, c.now) // 125 kB/s, bucket full
	b.SetBps(1000)             // 125 B/s: saved allowance must be discarded
	sent := 0
	for i := 0; i < 100; i++ {
		if b.Allow(170) {
			sent++
		}
		c.adv(100 * time.Millisecond)
	}
	// 10 s at 125 B/s = 1250 B = ~7 messages of 170 B, plus at most one burst.
	if sent > 9 {
		t.Fatalf("after reducing to 1 kbps, %d messages passed in 10 s", sent)
	}
	if b.Bps() != 1000 || b.Stats().Dropped == 0 {
		t.Fatalf("kbps/dropped wrong: %+v", b.Stats())
	}
}

func TestUsedKbpsWindow(t *testing.T) {
	c := newClock()
	b := New(0, c.now)
	b.Allow(1000)
	b.Record(250)
	if got := b.Stats().UsedBps; got != 2000 { // 1250 B = 10 kbit over the 5 s window
		t.Fatalf("UsedBps = %d want 2000", got)
	}
	c.adv(5500 * time.Millisecond)
	if got := b.Stats().UsedBps; got != 0 {
		t.Fatalf("UsedBps after window = %d want 0", got)
	}
	if b.Stats().SentBytes != 1250 {
		t.Fatalf("SentBytes = %d", b.Stats().SentBytes)
	}
}

func TestConcurrentUse(t *testing.T) {
	b := New(64000, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				b.Allow(100)
				_ = b.Stats()
				if i%100 == 0 {
					b.SetBps(uint32(32000 + i%64))
				}
			}
		}()
	}
	wg.Wait()
}

func TestKbps(t *testing.T) {
	for in, want := range map[uint32]uint32{0: 0, 1: 1, 999: 1, 1000: 1, 1001: 2, 256000: 256} {
		if got := Kbps(in); got != want {
			t.Errorf("Kbps(%d)=%d want %d", in, got, want)
		}
	}
}

func TestTryReserveKeepsRoomAndDoesNotCountDrops(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(8000, func() time.Time { return now }) // 1000 B/s, starts with 1000 B
	if !b.TryReserve(700, 200) {
		t.Fatal("first message refused")
	}
	if !b.TryReserve(100, 200) { // 300 B left, above the 200 B reserve
		t.Fatal("second message refused with 300 B left and 200 B reserve")
	}
	if b.TryReserve(100, 250) { // 200 left, not above 250
		t.Fatal("reserve not kept")
	}
	if !b.Allow(150) { // telemetry may still go
		t.Fatal("telemetry refused")
	}
	if s := b.Stats(); s.Dropped != 0 || s.SentBytes != 950 {
		t.Fatalf("stats %+v", s)
	}
	now = now.Add(time.Second)
	if !b.TryReserve(500, 200) {
		t.Fatal("refill not applied")
	}
}
