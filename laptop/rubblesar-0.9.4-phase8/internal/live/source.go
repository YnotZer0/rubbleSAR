package live

import (
	"context"
	"fmt"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Options configure a live feed.
type Options struct {
	Incident string
	Bus      bus.Bus
	Hub      *Hub
	// Crops receives the detections' crops (nil: not kept).
	Crops *Crops
	// Chats reads rescue-dispatch's chat (nil: no chat); polled every ChatEvery (default 3 s).
	Chats     func() ([]*Chat, error)
	ChatEvery time.Duration
	// Now is the receive clock (default time.Now).
	Now func() time.Time
}

// Live keeps a Hub up to date from the bus.
type Live struct {
	o      Options
	subs   []bus.Subscription
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// Counters for the status page.
	mu       sync.Mutex
	received map[string]uint64
	rejected uint64
}

// Start subscribes to everything the picture shows.
func Start(o Options) (*Live, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ChatEvery <= 0 {
		o.ChatEvery = 3 * time.Second
	}
	l := &Live{o: o, received: map[string]uint64{}}
	inc := o.Incident
	for _, x := range []struct {
		key string
		h   bus.Handler
	}{
		{keys.TelemetryAll(inc), l.onTelemetry},
		{keys.Casualties(inc), l.onCasualties},
		{keys.CasualtyAll(inc), l.onCasualty},
		{keys.Tasks(inc), l.onTasks},
		{keys.Dispatch(inc), l.onDispatch},
		{keys.DetectionAll(inc), l.onDetection},
	} {
		s, err := o.Bus.Subscribe(x.key, x.h)
		if err != nil {
			l.closeSubs()
			return nil, fmt.Errorf("E-WEB-002: subscribing to %s: %w", x.key, err)
		}
		l.subs = append(l.subs, s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.wg.Add(1)
	go l.loop(ctx)
	return l, nil
}

func (l *Live) closeSubs() {
	for _, s := range l.subs {
		_ = s.Close()
	}
	l.subs = nil
}

// Close stops the feed.
func (l *Live) Close() {
	l.closeSubs()
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
}

// Received counts the messages taken in, by kind.
func (l *Live) Received() (map[string]uint64, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.received))
	for k, v := range l.received {
		out[k] = v
	}
	return out, l.rejected
}

func (l *Live) count(kind string, ok bool) {
	l.mu.Lock()
	if ok {
		l.received[kind]++
	} else {
		l.rejected++
	}
	l.mu.Unlock()
}

func (l *Live) loop(ctx context.Context) {
	defer l.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastChat := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := l.o.Now()
		l.o.Hub.Tick(now)
		if l.o.Chats != nil && now.Sub(lastChat) >= l.o.ChatEvery {
			lastChat = now
			if lines, err := l.o.Chats(); err == nil {
				l.o.Hub.AddChats(lines)
			}
		}
	}
}

func (l *Live) onTelemetry(x bus.Sample) {
	var t sarpb.Telemetry
	if msg.Unmarshal(x.Payload, &t) != nil || msg.CheckHeader(t.GetHeader(), l.o.Incident) != nil || t.UavId == "" || t.Position == nil {
		l.count("telemetry", false)
		return
	}
	l.count("telemetry", true)
	now := l.o.Now()
	l.o.Hub.SetDrone(FromTelemetry(&t, now.UnixMilli()), now)
}

func (l *Live) onCasualties(x bus.Sample) {
	var cl sarpb.CasualtyList
	if msg.Unmarshal(x.Payload, &cl) != nil || msg.CheckHeader(cl.GetHeader(), l.o.Incident) != nil {
		l.count("casualties", false)
		return
	}
	l.count("casualties", true)
	list := make([]*Casualty, 0, len(cl.Casualties))
	for _, c := range cl.Casualties {
		list = append(list, FromCasualty(c))
	}
	l.o.Hub.SetCasualties(list, l.o.Now())
}

func (l *Live) onCasualty(x bus.Sample) {
	var c sarpb.Casualty
	if msg.Unmarshal(x.Payload, &c) != nil || msg.CheckHeader(c.GetHeader(), l.o.Incident) != nil {
		return
	}
	if c.MergedInto != "" {
		l.o.Hub.SetMerged(c.CasualtyId, c.MergedInto)
	}
}

func (l *Live) onTasks(x bus.Sample) {
	var tl sarpb.TaskList
	if msg.Unmarshal(x.Payload, &tl) != nil || msg.CheckHeader(tl.GetHeader(), l.o.Incident) != nil {
		l.count("tasks", false)
		return
	}
	l.count("tasks", true)
	list := make([]*Task, 0, len(tl.Tasks))
	for _, t := range tl.Tasks {
		list = append(list, FromTask(t))
	}
	l.o.Hub.SetTasks(list, l.o.Now())
}

func (l *Live) onDispatch(x bus.Sample) {
	var st sarpb.DispatchState
	if msg.Unmarshal(x.Payload, &st) != nil || msg.CheckHeader(st.GetHeader(), l.o.Incident) != nil {
		l.count("dispatch", false)
		return
	}
	l.count("dispatch", true)
	l.o.Hub.SetDispatch(FromDispatch(&st), l.o.Now())
}

func (l *Live) onDetection(x bus.Sample) {
	var d sarpb.Detection
	if msg.Unmarshal(x.Payload, &d) != nil || msg.CheckHeader(d.GetHeader(), l.o.Incident) != nil || d.DetectionId == "" {
		l.count("detection", false)
		return
	}
	l.count("detection", true)
	if l.o.Crops != nil {
		l.o.Crops.Put(d.DetectionId, d.GetVisibleCrop().GetData(), d.GetThermalCrop().GetData())
	}
	l.o.Hub.AddDetection(FromDetection(&d, len(x.Payload), l.o.Now().UnixMilli()))
}
