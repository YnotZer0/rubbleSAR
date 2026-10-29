// Package linkbudget meters a simulated radio link with a token bucket whose
// rate can be changed at run time (decision D11: throttle from the Jetson).
//
// A message is allowed whenever the bucket is not in debt; its full size is
// then charged, possibly driving the bucket negative. This lets messages larger
// than one second's allowance through while still enforcing the average rate.
package linkbudget

import (
	"sync"
	"time"
)

// Bucket is safe for concurrent use.
type Bucket struct {
	mu      sync.Mutex
	now     func() time.Time
	bps     uint32  // bits per second, 0 = unlimited
	rate    float64 // bytes per second
	tokens  float64
	last    time.Time
	sent    []event // sends in the last usageWindow, for UsedBps
	dropped uint64
	sentTot uint64
}

type event struct {
	at time.Time
	n  int
}

// New creates a bucket. bps (bits per second) 0 means unlimited. now may be nil (time.Now).
func New(bps uint32, now func() time.Time) *Bucket {
	if now == nil {
		now = time.Now
	}
	b := &Bucket{now: now}
	b.last = now()
	b.setLocked(bps)
	b.tokens = b.rate // start with one second of allowance
	return b
}

func (b *Bucket) setLocked(bps uint32) {
	b.bps = bps
	b.rate = float64(bps) / 8
}

// SetBps changes the budget immediately. Saved-up allowance above one second
// of the new rate is discarded, so a reduction takes effect at once.
func (b *Bucket) SetBps(bps uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	b.setLocked(bps)
	if b.tokens > b.rate {
		b.tokens = b.rate
	}
}

// Bps returns the current budget in bits per second (0 = unlimited).
func (b *Bucket) Bps() uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bps
}

func (b *Bucket) refillLocked() {
	t := b.now()
	dt := t.Sub(b.last).Seconds()
	b.last = t
	if dt <= 0 || b.bps == 0 {
		return
	}
	b.tokens += dt * b.rate
	if b.tokens > b.rate { // burst cap: one second of traffic
		b.tokens = b.rate
	}
}

// Allow reports whether a message of n bytes may be sent now and, if so,
// charges it. A refused message is counted as dropped.
func (b *Bucket) Allow(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.bps != 0 && b.tokens <= 0 {
		b.dropped++
		return false
	}
	if b.bps != 0 {
		b.tokens -= float64(n)
	}
	b.recordLocked(n)
	return true
}

// TryReserve charges and allows a message of n bytes only while more than
// reserve bytes of allowance remain, keeping room for telemetry. A refusal is
// not counted as a drop: the caller keeps the message queued.
func (b *Bucket) TryReserve(n, reserve int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.bps != 0 {
		if b.tokens <= float64(reserve) {
			return false
		}
		b.tokens -= float64(n)
	}
	b.recordLocked(n)
	return true
}

// CountDrop records a message given up for lack of budget.
func (b *Bucket) CountDrop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropped++
}

// Record counts bytes sent outside the budget (e.g. control replies) in the
// usage statistics without charging the bucket.
func (b *Bucket) Record(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(n)
}

func (b *Bucket) recordLocked(n int) {
	t := b.now()
	b.sent = append(b.sent, event{t, n})
	b.sentTot += uint64(n)
	b.pruneLocked(t)
}

func (b *Bucket) pruneLocked(t time.Time) {
	cut := t.Add(-usageWindow)
	i := 0
	for i < len(b.sent) && !b.sent[i].at.After(cut) {
		i++
	}
	if i > 0 {
		b.sent = append(b.sent[:0], b.sent[i:]...)
	}
}

// Stats is a snapshot for telemetry and the admin API.
type Stats struct {
	BudgetBps uint32
	UsedBps   uint32
	Dropped   uint64
	SentBytes uint64
}

// usageWindow is the averaging period for UsedBps. Five seconds smooths over
// 1 Hz telemetry so the figure does not flicker between 0 and one message.
const usageWindow = 5 * time.Second

// Stats returns current statistics. UsedBps is the average over the last 5 s.
func (b *Bucket) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	b.pruneLocked(t)
	sum := 0
	for _, e := range b.sent {
		sum += e.n
	}
	return Stats{BudgetBps: b.bps, UsedBps: uint32(float64(sum*8) / usageWindow.Seconds()), Dropped: b.dropped, SentBytes: b.sentTot}
}

// Kbps converts bits per second to kbit/s, rounding up so a non-zero budget never shows as 0.
func Kbps(bps uint32) uint32 { return (bps + 999) / 1000 }
