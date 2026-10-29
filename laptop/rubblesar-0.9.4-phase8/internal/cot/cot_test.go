package cot

import (
	"bytes"
	"context"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMarkerRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	e := Marker("rubblesar.inc.cas.C-007", "C-007 RED", Red, 36.2040721, 36.1611707, 82.5, 1.8, "warm, no movement <&>", now, 5*time.Minute)
	b, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`<event version="2.0" uid="rubblesar.inc.cas.C-007" type="b-m-p-s-m"`, `<point lat="36.2040721" lon="36.1611707" hae="82.5" ce="1.8" le="9999999.0"`,
		`<contact callsign="C-007 RED">`, `<color argb="-65536">`, `iconsetpath="COT_MAPPING_SPOTMAP/b-m-p-s-m/-65536"`, `warm, no movement &lt;&amp;&gt;`,
		`stale="2026-02-10T09:05:00.000Z"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if strings.Contains(s, "<?xml") || strings.Contains(s, "__chat") || strings.Contains(s, "marti") {
		t.Errorf("unexpected content:\n%s", s)
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := p.MarkerColor(); !ok || c != Red || p.Callsign() != "C-007 RED" || p.Point.Lat != 36.2040721 || p.Detail.Remarks.Text != "warm, no movement <&>" {
		t.Fatalf("parsed %+v", p)
	}
	if !p.StaleTime().Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("stale %v", p.StaleTime())
	}
}

// What ATAK sends: an XML declaration, unknown detail elements, a GeoChat.
const atakChat = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><event version="2.0" uid="GeoChat.ANDROID-abc.SAR-OPS-uid.d1e2" type="b-t-f" time="2026-02-10T09:10:00.123Z" start="2026-02-10T09:10:00.123Z" stale="2026-02-11T09:10:00.123Z" how="h-g-i-g-o"><point lat="36.2" lon="36.16" hae="9999999.0" ce="9999999.0" le="9999999.0"/><detail><__chat parent="RootContactGroup" groupOwner="false" messageId="d1e2" chatroom="SAR-OPS" id="SAR-OPS-uid" senderCallsign="ALPHA"><chatgrp uid0="ANDROID-abc" uid1="SAR-OPS-uid" id="SAR-OPS-uid"/></__chat><link uid="ANDROID-abc" type="a-f-G-U-C" relation="p-p"/><__serverdestination destinations="192.168.1.20:4242:tcp:ANDROID-abc"/><remarks source="BAO.F.ATAK.ANDROID-abc" to="SAR-OPS-uid" time="2026-02-10T09:10:00.123Z">  done </remarks><marti><dest callsign="SAR-OPS"/></marti></detail></event>`

const atakPLI = `<?xml version='1.0' encoding='UTF-8' standalone='yes'?>
<event version='2.0' uid='ANDROID-abc' type='a-f-G-U-C' time='2026-02-10T09:10:01Z' start='2026-02-10T09:10:01Z' stale='2026-02-10T09:16:01Z' how='h-e'><point lat='36.2031' lon='36.1602' hae='130.2' ce='9.9' le='9999999.0'/><detail><takv os='34' version='5.1.0.7' device='SAMSUNG SM-G991B' platform='ATAK-CIV'/><contact endpoint='*:-1:stcp' callsign='ALPHA'/><uid Droid='ALPHA'/><precisionlocation altsrc='GPS' geopointsrc='GPS'/><__group role='Team Member' name='Cyan'/><status battery='88'/><track course='93.1' speed='1.2'/></detail></event>`

func TestSplitterAndATAK(t *testing.T) {
	stream := "\n" + atakPLI + "\r\n   " + atakChat + `<eventual/>` + `<event version="2.0" uid="x" type="t-x-c-t" how="h-g-i-g-o" time="2026-02-10T09:10:00Z" start="2026-02-10T09:10:00Z" stale="2026-02-10T09:10:20Z"/>` + "<event broken"
	// Deliver it a few bytes at a time.
	sp := NewSplitter(&trickle{data: []byte(stream), n: 7})
	pli, err := sp.ReadEvent()
	if err != nil {
		t.Fatal(err)
	}
	if !pli.IsPLI() || pli.Callsign() != "ALPHA" || pli.Detail.Group.Name != "Cyan" || pli.Detail.Track.Speed != 1.2 || pli.Point.Lat != 36.2031 {
		t.Fatalf("PLI %+v", pli)
	}
	chat, err := sp.ReadEvent()
	if err != nil {
		t.Fatal(err)
	}
	if !chat.IsChat() || chat.ChatText() != "done" || chat.ChatSender() != "ANDROID-abc" || chat.Detail.Chat.SenderCallsign != "ALPHA" ||
		len(chat.Dests()) != 1 || chat.Dests()[0] != "SAR-OPS" || chat.Detail.Chat.ID != "SAR-OPS-uid" {
		t.Fatalf("chat %+v", chat.Detail.Chat)
	}
	ping, err := sp.ReadEvent()
	if err != nil || ping.Type != "t-x-c-t" {
		t.Fatalf("ping %v %v", ping, err)
	}
	if _, err := sp.ReadEvent(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

type trickle struct {
	data []byte
	n    int
}

func (r *trickle) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.n, len(p), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestBuilders(t *testing.T) {
	now := time.Now()
	chat := GeoChat("sar-ops-uid", "SAR-OPS", ChatTo{}, "TEAM-2 -> C-007 RED, 420 m, about 6 min", 36.2, 36.16, now)
	if chat.Detail.Chat.Chatroom != AllChat || chat.Detail.Marti != nil || !strings.HasPrefix(chat.UID, "GeoChat.sar-ops-uid.All Chat Rooms.") {
		t.Fatalf("broadcast chat %+v", chat)
	}
	dm := GeoChat("sar-ops-uid", "SAR-OPS", ChatTo{UID: "ANDROID-abc", Callsign: "ALPHA"}, "go to C-007", 36.2, 36.16, now)
	if dm.Detail.Chat.Chatroom != "ALPHA" || dm.Detail.Chat.ID != "ANDROID-abc" || dm.Dests()[0] != "ALPHA" || dm.ChatSender() != "sar-ops-uid" {
		t.Fatalf("direct chat %+v", dm.Detail)
	}
	line := Line("r1", "TEAM-1 to C-001", Cyan, 3, [][2]float64{{36.2, 36.16}, {36.2001, 36.1602}, {36.2003, 36.1603}}, "", now, time.Minute)
	b, _ := line.Marshal()
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if pts := back.LinePoints(); len(pts) != 3 || math.Abs(pts[2][1]-36.1603) > 1e-9 {
		t.Fatalf("line points %v", pts)
	}
	del := Delete("r1", now)
	b, _ = del.Marshal()
	if !bytes.Contains(b, []byte("<__forcedelete></__forcedelete>")) {
		t.Fatalf("delete %s", b)
	}
	back, _ = Parse(b)
	if !back.IsDelete() || back.DeleteUID() != "r1" {
		t.Fatalf("delete %+v", back)
	}
	d := Drone("u", "scout-1", 36.2, 36.16, 120, 91, 5, 80, "", now, time.Minute)
	b, _ = d.Marshal()
	if !bytes.Contains(b, []byte(`<status battery="80">`)) || !bytes.Contains(b, []byte(`<track course="91.0" speed="5.0">`)) {
		t.Fatalf("drone %s", b)
	}
}

// Two clients through the router: broadcast, addressed chat, reconnection
// with the hello sent again.
func TestClientsThroughServer(t *testing.T) {
	srv, err := ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type inbox struct {
		mu sync.Mutex
		ev []*Event
	}
	got := func(in *inbox, f func(*Event) bool) int {
		in.mu.Lock()
		defer in.mu.Unlock()
		n := 0
		for _, e := range in.ev {
			if f(e) {
				n++
			}
		}
		return n
	}
	mk := func(uid, cs string, in *inbox) *Client {
		c := NewClient(ClientConfig{Addr: srv.Addr(), PingUID: uid, PingEvery: 200 * time.Millisecond, MaxBackoff: 300 * time.Millisecond,
			Hello: func(now time.Time) []*Event {
				return []*Event{PLI(uid, cs, "Cyan", "Team Member", 36.2, 36.16, 0, 0, "", now, time.Minute)}
			},
			OnEvent: func(e *Event) { in.mu.Lock(); in.ev = append(in.ev, e); in.mu.Unlock() }})
		go c.Run(ctx)
		return c
	}
	var ia, ib, ic inbox
	a := mk("uid-a", "OPS", &ia)
	mk("uid-b", "TEAM-1", &ib)
	mk("uid-c", "TEAM-2", &ic)
	waitFor(t, "three clients known", func() bool { return len(srv.Callsigns()) == 3 })
	a.Send(Marker("m1", "C-001 RED", Red, 36.2, 36.16, 0, 2, "", time.Now(), time.Minute))
	a.Send(GeoChat("uid-a", "OPS", ChatTo{UID: "uid-b", Callsign: "TEAM-1"}, "only for team 1", 36.2, 36.16, time.Now()))
	waitFor(t, "marker at both, chat at TEAM-1", func() bool {
		return got(&ib, func(e *Event) bool { return e.UID == "m1" }) == 1 && got(&ic, func(e *Event) bool { return e.UID == "m1" }) == 1 &&
			got(&ib, (*Event).IsChat) == 1
	})
	time.Sleep(300 * time.Millisecond)
	if n := got(&ic, (*Event).IsChat); n != 0 {
		t.Fatalf("TEAM-2 got %d addressed chats", n)
	}
	if n := got(&ib, func(e *Event) bool { return e.Type == "t-x-c-t" }); n != 0 {
		t.Fatalf("pings were routed: %d", n)
	}
	before := a.Stats().Reconnects
	srv.DropClients()
	// A new connection is known only by the position report in its hello, so
	// the server knowing all three callsigns again proves every hello arrived
	// (whether the others were back in time to receive it is a race).
	waitFor(t, "reconnected with hellos", func() bool {
		return a.Stats().Reconnects > before && strings.Join(srv.Callsigns(), ",") == "OPS,TEAM-1,TEAM-2"
	})
	a.Send(Marker("m2", "C-002 YELLOW", Yellow, 36.2, 36.16, 0, 2, "", time.Now(), time.Minute))
	waitFor(t, "marker after reconnect", func() bool { return got(&ic, func(e *Event) bool { return e.UID == "m2" }) == 1 })
	if st := a.Stats(); !st.Connected || st.Sent < 4 {
		t.Fatalf("stats %+v", st)
	}
}

func TestClientQueuesWhileDown(t *testing.T) {
	// Nothing listens yet: events queue, then go out on connection.
	ln, err := ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewClient(ClientConfig{Addr: addr, MaxBackoff: 200 * time.Millisecond, QueueLen: 3})
	go c.Run(ctx)
	for i := 0; i < 5; i++ {
		c.Send(Ping("q", time.Now()))
	}
	time.Sleep(300 * time.Millisecond)
	if st := c.Stats(); st.Connected || st.Queued != 3 || st.Dropped != 2 || st.LastError == "" || !strings.Contains(st.LastError, "E-TAK-001") {
		t.Fatalf("stats while down %+v", st)
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
