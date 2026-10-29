package check

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/cot"
)

// TakServer checks a TAK server the way rescue-dispatch and ATAK use it:
// three clients connect, position reports and a marker reach the others, an
// addressed chat reaches only its addressee, and a delete passes.
func TakServer(ctx context.Context, addr string, out io.Writer) *Report {
	r := NewReport("tak", out)
	r.Context["tak_server"] = addr
	type inbox struct {
		mu sync.Mutex
		ev []*cot.Event
	}
	has := func(in *inbox, f func(*cot.Event) bool) bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		for _, e := range in.ev {
			if f(e) {
				return true
			}
		}
		return false
	}
	wait := func(d time.Duration, f func() bool) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if f() {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return f()
	}
	tag := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	names := []string{"SARCHK-A", "SARCHK-B", "SARCHK-C"}
	var boxes [3]inbox
	var cls [3]*cot.Client
	for i, n := range names {
		i, n := i, n
		uid := "rubblesar-tak-check." + tag + "." + strings.ToLower(n)
		cls[i] = cot.NewClient(cot.ClientConfig{Addr: addr, MaxBackoff: time.Second,
			OnEvent: func(e *cot.Event) { boxes[i].mu.Lock(); boxes[i].ev = append(boxes[i].ev, e); boxes[i].mu.Unlock() },
			Hello: func(now time.Time) []*cot.Event {
				return []*cot.Event{cot.PLI(uid, n, "Cyan", "Team Member", 36.2025, 36.16056+float64(i)*1e-4, 0, 0, "sar-check tak", now, time.Minute)}
			}})
		go cls[i].Run(cctx)
		defer cls[i].Close()
	}
	uidOf := func(i int) string { return "rubblesar-tak-check." + tag + "." + strings.ToLower(names[i]) }
	res := r.Run("T-01", "three TAK clients connect", func() Result {
		ok := wait(10*time.Second, func() bool { return cls[0].Connected() && cls[1].Connected() && cls[2].Connected() })
		if !ok {
			return Result{Status: Fail, Code: "E-TAK-001", Detail: "cannot connect to the TAK server " + addr, Evidence: []string{cls[0].Stats().LastError},
				Hint: "'sudo systemctl status rubblesar-taky'; 'docker logs rubblesar-taky'; re-run scripts/zbook/34-taky.sh"}
		}
		return Result{Status: Pass, Detail: "connected to " + addr + " (plain TCP)"}
	})
	if res.Status == Fail {
		for i, id := range []string{"T-02", "T-03", "T-04", "T-05"} {
			r.Skip(id, []string{"position reports reach the other clients", "a casualty marker reaches the others",
				"a chat addressed to one callsign reaches only it", "a delete reaches the others"}[i], "needs T-01")
		}
		return r
	}
	// Send the position reports again now that all three are connected.
	resend := func() {
		for i := range cls {
			cls[i].Send(cot.PLI(uidOf(i), names[i], "Cyan", "Team Member", 36.2025, 36.16056+float64(i)*1e-4, 0, 0, "sar-check tak", time.Now(), time.Minute))
		}
	}
	r.Run("T-02", "position reports reach the other clients", func() Result {
		resend()
		ok := wait(10*time.Second, func() bool {
			for i := range cls {
				for j := range cls {
					if i != j && !has(&boxes[j], func(e *cot.Event) bool { return e.UID == uidOf(i) && e.IsPLI() }) {
						return false
					}
				}
			}
			return true
		})
		if !ok {
			return Result{Status: Fail, Code: "E-TAK-001", Detail: "position reports did not reach every other client within 10 s",
				Hint: "the TAK server accepts connections but does not route: 'docker logs rubblesar-taky'"}
		}
		return Result{Status: Pass, Detail: "each client sees the other two"}
	})
	// Give the server a moment to learn every callsign before the addressed chat.
	time.Sleep(500 * time.Millisecond)
	r.Run("T-03", "a casualty marker reaches the others", func() Result {
		m := cot.Marker("rubblesar-tak-check."+tag+".marker", "SAR-CHECK RED", cot.Red, 36.2025, 36.1606, 80, 2, "sar-check tak test marker", time.Now(), 30*time.Second)
		cls[0].Send(m)
		ok := wait(5*time.Second, func() bool {
			return has(&boxes[1], func(e *cot.Event) bool { c, _ := e.MarkerColor(); return e.UID == m.UID && c == cot.Red }) &&
				has(&boxes[2], func(e *cot.Event) bool { return e.UID == m.UID })
		})
		if !ok {
			return Result{Status: Fail, Code: "E-TAK-001", Detail: "the marker did not reach the other clients", Hint: "'docker logs rubblesar-taky'"}
		}
		return Result{Status: Pass, Detail: "red spot marker received with its colour"}
	})
	r.Run("T-04", "a chat addressed to one callsign reaches only it", func() Result {
		text := "sar-check tak direct " + tag
		cls[0].Send(cot.GeoChat(uidOf(0), names[0], cot.ChatTo{UID: uidOf(1), Callsign: names[1]}, text, 36.2025, 36.16056, time.Now()))
		all := "sar-check tak all " + tag
		cls[0].Send(cot.GeoChat(uidOf(0), names[0], cot.ChatTo{}, all, 36.2025, 36.16056, time.Now()))
		gotB := wait(5*time.Second, func() bool {
			return has(&boxes[1], func(e *cot.Event) bool { return e.IsChat() && e.ChatText() == text })
		})
		gotAll := wait(5*time.Second, func() bool {
			return has(&boxes[1], func(e *cot.Event) bool { return e.IsChat() && e.ChatText() == all }) &&
				has(&boxes[2], func(e *cot.Event) bool { return e.IsChat() && e.ChatText() == all })
		})
		time.Sleep(time.Second)
		leakC := has(&boxes[2], func(e *cot.Event) bool { return e.IsChat() && e.ChatText() == text })
		switch {
		case !gotAll:
			return Result{Status: Fail, Code: "E-TAK-001", Detail: "an All Chat Rooms message did not reach every client", Hint: "'docker logs rubblesar-taky'"}
		case !gotB:
			return Result{Status: Warn, Code: "E-TAK-001", Detail: "All Chat Rooms works, but a message addressed to one callsign did not arrive",
				Hint: "rescue-dispatch also posts every message to All Chat Rooms, so teams still see them"}
		case leakC:
			return Result{Status: Warn, Code: "E-TAK-001", Detail: "a message addressed to one callsign also reached another client (the server broadcasts it)",
				Hint: "harmless for the demo: the message names its team"}
		}
		return Result{Status: Pass, Detail: "All Chat Rooms reaches everyone; an addressed message only its callsign"}
	})
	r.Run("T-05", "a delete reaches the others", func() Result {
		uid := "rubblesar-tak-check." + tag + ".marker"
		cls[0].Send(cot.Delete(uid, time.Now()))
		ok := wait(5*time.Second, func() bool {
			return has(&boxes[1], func(e *cot.Event) bool { return e.IsDelete() && e.DeleteUID() == uid })
		})
		if !ok {
			return Result{Status: Fail, Code: "E-TAK-001", Detail: "the delete did not arrive", Hint: "'docker logs rubblesar-taky'"}
		}
		return Result{Status: Pass, Detail: "marker removed on the other clients"}
	})
	return r
}
