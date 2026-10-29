package check

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/live"
)

// Phase7Options configure the phase 7 acceptance: the operator view checked
// against the services while the phase 6 run goes on.
type Phase7Options struct {
	P6 Phase6Options
	// Services are the APIs the picture is compared with (default from the config).
	Services live.Services
	// AnalystDir: casualty-analyst's folder for the incident, where the crops are stored (default [analyst] store_dir/<incident>).
	AnalystDir string
	// Every: how often the picture is compared during the run (default 20 s).
	Every time.Duration
	// StreamFor: how long P7-03 listens to the event stream (default 12 s).
	StreamFor time.Duration
}

// Thresholds of the phase 7 acceptance.
const (
	p7AgreePass = 0.9  // share of samples where the picture agrees with the services
	p7AgreeWarn = 0.75 // below: FAIL
	p7Retries   = 3    // a sample is retried this often (1.5 s apart) before it counts as a disagreement
	p7CropsMax  = 10   // crops compared
)

// Phase7 is the phase 7 acceptance: the operator view served and connected
// (P7-01), the whole phase 6 run (P6-01, P6-02, P5-01..06, P6-03), the live
// picture agreeing with casualty-analyst, mission-planner and rescue-dispatch
// throughout (P7-02), the event stream (P7-03), the crops (P7-04) and the
// timeline against the after-action report (P7-05).
func Phase7(ctx context.Context, o Phase7Options, out io.Writer) *Report {
	cfg := o.P6.P5.P4.Cfg
	if o.Every <= 0 {
		o.Every = 20 * time.Second
	}
	if o.StreamFor <= 0 {
		o.StreamFor = 12 * time.Second
	}
	if o.Services.Analyst == "" {
		o.Services = live.Services{Analyst: cfg.Analyst.Listen, Planner: cfg.Planner.Listen, Dispatch: cfg.Dispatch.Listen}
	}
	if o.AnalystDir == "" {
		o.AnalystDir = cfg.AnalystDir()
	}
	if o.P6.WebAddr == "" {
		o.P6.WebAddr = cfg.Web.Listen
	}
	web := localAddr(o.P6.WebAddr)
	r := NewReport("phase7", out)
	r.Share = true
	r.Run("P7-01", "operator view served and connected", func() Result {
		return safe("E-CHK-701", func() Result {
			return p7Served(web, cfg.Site.IncidentID, cfg.Check.ExpectUAVs, len(cfg.Dispatch.Teams))
		})
	})
	// Compare the picture with the services while the run goes on.
	smp := &p7Sampler{web: web, svc: o.Services}
	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(o.Every)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				smp.sample(sctx)
			}
		}
	}()
	p6 := o.P6
	p6.Report = r
	Phase6(ctx, p6, out)
	cancel()
	wg.Wait()
	r.Run("P7-02", "live picture agrees with the services during the run", func() Result {
		return safe("E-CHK-702", func() Result { smp.sample(ctx); return smp.result() })
	})
	r.Run("P7-03", "event stream to the browsers", func() Result {
		return safe("E-CHK-703", func() Result { return p7Stream(ctx, web, o.StreamFor) })
	})
	r.Run("P7-04", "the drones' crops served", func() Result {
		return safe("E-CHK-704", func() Result { return p7Crops(web, o.AnalystDir) })
	})
	r.Run("P7-05", "timeline against the after-action report", func() Result {
		return safe("E-CHK-705", func() Result { return p7Timeline(web, r.Context["report"]) })
	})
	return r
}

func httpGet(url string, timeout time.Duration) ([]byte, *http.Response, error) {
	c := http.Client{Timeout: timeout}
	resp, err := c.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return b, resp, err
}

func getPicture(web string) (*live.Picture, error) {
	b, resp, err := httpGet("http://"+web+"/v1/picture", 5*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET /v1/picture: %s", resp.Status)
	}
	var p live.Picture
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("GET /v1/picture: %v", err)
	}
	return &p, nil
}

// --- P7-01 -------------------------------------------------------------------------------

func p7Served(web, inc string, uavs []string, teams int) Result {
	var ev []string
	fail := func(detail, hint string) Result {
		return Result{Status: Fail, Code: "E-CHK-701", Detail: detail, Evidence: ev, Hint: hint}
	}
	for _, x := range []struct{ path, want string }{{"/ops/", "RubbleSAR operator view"}, {"/ops/app.js", "RubbleSAR operator view"}, {"/ops/app.css", "--signal"},
		{"/ops/fonts/Barlow-Regular.woff2", "wOF2"}} {
		b, resp, err := httpGet("http://"+web+x.path, 5*time.Second)
		if err != nil {
			return fail("sar-web is not answering: "+err.Error(), "sudo systemctl status sar-web; journalctl -u sar-web -n 50")
		}
		if resp.StatusCode != 200 || !bytes.Contains(b, []byte(x.want)) {
			return fail(fmt.Sprintf("%s: %s, %d bytes", x.path, resp.Status, len(b)), "an older sar-web is installed: scripts/zbook/30-install.sh")
		}
	}
	ev = append(ev, "/ops/ (page, script, style, fonts) served")
	b, _, err := httpGet("http://"+web+"/v1/scene", 5*time.Second)
	if err != nil {
		return fail("GET /v1/scene: "+err.Error(), "")
	}
	var sc struct {
		Incident string            `json:"incident"`
		Map      *struct{}         `json:"map"`
		Rubble   [][][2]float64    `json:"rubble"`
		Sites    []json.RawMessage `json:"sites"`
		Teams    []json.RawMessage `json:"teams"`
		Staging  [2]float64        `json:"staging"`
		Model    *struct {
			Name string `json:"name"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return fail("GET /v1/scene: "+err.Error(), "")
	}
	ev = append(ev, fmt.Sprintf("scene: incident %s, %d collapse sites, %d rubble areas, %d teams, map %v, model %v", sc.Incident, len(sc.Sites), len(sc.Rubble), len(sc.Teams), sc.Map != nil, sc.Model != nil))
	if sc.Incident != inc {
		return fail(fmt.Sprintf("the scene is for incident %q, not %q", sc.Incident, inc), "sar-web reads the same /etc/rubblesar/sar.toml: sudo systemctl restart sar-web")
	}
	if len(sc.Sites) == 0 || len(sc.Teams) != teams {
		return fail(fmt.Sprintf("the scene has %d sites and %d teams (config: %d teams)", len(sc.Sites), len(sc.Teams), teams), "the scenario must be installed (scripts/zbook/60-scenario.sh); restart sar-web after changing the teams")
	}
	var p *live.Picture
	deadline := time.Now().Add(20 * time.Second)
	for {
		p, err = getPicture(web)
		if err == nil && p.Mode == "live" && p.Bus == "connected" && len(p.Drones) >= len(uavs) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fail(err.Error(), "")
	}
	ev = append(ev, fmt.Sprintf("picture: mode %s, bus %q, %d drones, %d casualties, %d teams", p.Mode, p.Bus, len(p.Drones), len(p.Casualties), len(p.Teams)))
	if p.Mode != "live" || p.Bus != "connected" {
		return fail("the live view is not connected to zenohd: "+p.Bus, "sudo systemctl restart sar-web; is zenohd running? (systemctl status zenohd); journalctl -u sar-web -n 50")
	}
	var missing, stale []string
	for _, id := range uavs {
		found := false
		for _, d := range p.Drones {
			if d.ID == id {
				found = true
				if d.Stale {
					stale = append(stale, id)
				}
			}
		}
		if !found {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fail("the picture has no telemetry from "+strings.Join(missing, ", "), "is edge-eye running on the Jetson? rubblesar status")
	}
	if len(stale) > 0 {
		return Result{Status: Warn, Code: "E-CHK-701", Detail: "drones not heard recently: " + strings.Join(stale, ", "), Evidence: ev}
	}
	if sc.Map == nil {
		return Result{Status: Warn, Code: "E-CHK-701", Detail: "served and connected, but no offline map: the view draws the rubble on a plain background", Evidence: ev, Hint: "scripts/zbook/36-map.sh"}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("served, connected to zenohd, %d drones reporting", len(p.Drones)), Evidence: ev}
}

// --- P7-02 -------------------------------------------------------------------------------

type p7Sampler struct {
	web     string
	svc     live.Services
	mu      sync.Mutex
	samples int
	agreed  int
	errs    int
	bad     []string
	last    string
	lastOK  bool
	most    [3]int // the largest casualty, task, assignment counts compared
}

// diffStates lists what differs between the services' view (want) and the picture's (got).
func diffStates(kind string, want, got map[string]string) []string {
	var out []string
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if g, ok := got[k]; !ok {
			out = append(out, fmt.Sprintf("%s %s (%s) missing from the picture", kind, k, want[k]))
		} else if g != want[k] {
			out = append(out, fmt.Sprintf("%s %s: %s in the picture, %s in the service", kind, k, g, want[k]))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			out = append(out, fmt.Sprintf("%s %s (%s) in the picture only", kind, k, got[k]))
		}
	}
	return out
}

func (s *p7Sampler) compareOnce() ([]string, error) {
	cas, err := s.svc.CasualtyIDs()
	if err != nil {
		return nil, fmt.Errorf("casualty-analyst: %v", err)
	}
	tasks, err := s.svc.TaskStates()
	if err != nil {
		return nil, fmt.Errorf("mission-planner: %v", err)
	}
	asg, err := s.svc.AssignmentStates()
	if err != nil {
		return nil, fmt.Errorf("rescue-dispatch: %v", err)
	}
	p, err := getPicture(s.web)
	if err != nil {
		return nil, err
	}
	pc, pt, pa := map[string]string{}, map[string]string{}, map[string]string{}
	for _, c := range p.Casualties {
		pc[c.ID] = c.Triage
	}
	for _, t := range p.Tasks {
		pt[t.ID] = t.State
	}
	for _, a := range p.Assignments {
		pa[a.ID] = a.State
	}
	s.mu.Lock()
	s.most = [3]int{max(s.most[0], len(cas)), max(s.most[1], len(tasks)), max(s.most[2], len(asg))}
	s.mu.Unlock()
	d := append(diffStates("casualty", cas, pc), diffStates("task", tasks, pt)...)
	return append(d, diffStates("assignment", asg, pa)...), nil
}

func (s *p7Sampler) sample(ctx context.Context) {
	var diff []string
	var err error
	for i := 0; i < p7Retries; i++ {
		diff, err = s.compareOnce()
		if err == nil && len(diff) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errs++
		s.last, s.lastOK = err.Error(), false
		return
	}
	s.samples++
	s.lastOK = len(diff) == 0
	if s.lastOK {
		s.agreed++
		s.last = ""
		return
	}
	s.last = strings.Join(limit(diff, 4), "; ")
	if len(s.bad) < 8 {
		s.bad = append(s.bad, fmt.Sprintf("%s: %s", time.Now().Format("15:04:05"), strings.Join(limit(diff, 3), "; ")))
	}
}

func (s *p7Sampler) result() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := []string{fmt.Sprintf("%d samples, %d agreed (casualties, close-look tasks and rescue assignments, ids and states); up to %d casualties, %d tasks, %d assignments compared",
		s.samples, s.agreed, s.most[0], s.most[1], s.most[2])}
	if s.errs > 0 {
		ev = append(ev, fmt.Sprintf("%d samples could not be taken (a service not answering)", s.errs))
	}
	ev = append(ev, s.bad...)
	if s.samples == 0 {
		return Result{Status: Fail, Code: "E-CHK-702", Detail: "no sample could be taken: " + s.last, Evidence: ev, Hint: "the services and sar-web must answer on this ZBook: rubblesar status"}
	}
	share := float64(s.agreed) / float64(s.samples)
	detail := fmt.Sprintf("%d of %d samples agreed (%.0f%%)", s.agreed, s.samples, 100*share)
	switch {
	case !s.lastOK:
		return Result{Status: Fail, Code: "E-CHK-702", Detail: detail + "; the last sample, after the run, does not: " + s.last, Evidence: ev, Hint: "paste the block; restart sar-web and look again (sudo systemctl restart sar-web)"}
	case share >= p7AgreePass:
		return Result{Status: Pass, Detail: detail + ", and the picture matches the services after the run", Evidence: ev}
	case share >= p7AgreeWarn:
		return Result{Status: Warn, Code: "E-CHK-702", Detail: detail, Evidence: ev, Hint: "the picture lagged behind the services at times; paste the block"}
	}
	return Result{Status: Fail, Code: "E-CHK-702", Detail: detail, Evidence: ev, Hint: "paste the block; journalctl -u sar-web -n 100"}
}

// --- P7-03 -------------------------------------------------------------------------------

func p7Stream(ctx context.Context, web string, listen time.Duration) Result {
	sctx, cancel := context.WithTimeout(ctx, listen)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", "http://"+web+"/v1/stream?src=live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-703", Detail: "GET /v1/stream: " + err.Error(), Hint: "sudo systemctl status sar-web"}
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	gz := resp.Uncompressed // the transport asked for gzip and unpacked it
	start := time.Now()
	var pictures, drones int
	var bytesIn int
	var first time.Duration
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 32<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "picture":
			var p live.Picture
			data := strings.TrimPrefix(line, "data: ")
			bytesIn += len(data)
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				return Result{Status: Fail, Code: "E-CHK-703", Detail: "a picture on the stream is not valid JSON: " + err.Error()}
			}
			if pictures == 0 {
				first = time.Since(start)
			}
			pictures++
			drones = len(p.Drones)
		case line == "":
			event = ""
		}
	}
	took := time.Since(start)
	ev := []string{fmt.Sprintf("Content-Type %s, gzip %v; %d pictures in %.0f s (first after %.1f s), %d KB of JSON, %d drones in the last",
		ct, gz, pictures, took.Seconds(), first.Seconds(), bytesIn/1024, drones)}
	if !strings.HasPrefix(ct, "text/event-stream") {
		return Result{Status: Fail, Code: "E-CHK-703", Detail: "not an event stream: " + ct, Evidence: ev}
	}
	need := int(listen.Seconds() / 3)
	if pictures < need {
		return Result{Status: Fail, Code: "E-CHK-703", Detail: fmt.Sprintf("%d pictures in %.0f s, want at least %d (one every 3 s)", pictures, listen.Seconds(), need), Evidence: ev,
			Hint: "a proxy between the browser and the ZBook may buffer the stream; open http://<ZBook>:8090/ops/ directly"}
	}
	if !gz {
		return Result{Status: Warn, Code: "E-CHK-703", Detail: fmt.Sprintf("%d pictures in %.0f s, but not compressed", pictures, listen.Seconds()), Evidence: ev}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d pictures in %.0f s, compressed", pictures, listen.Seconds()), Evidence: ev}
}

// --- P7-04 -------------------------------------------------------------------------------

func p7Crops(web, analystDir string) Result {
	p, err := getPicture(web)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-704", Detail: err.Error()}
	}
	var ev []string
	checked, same := 0, 0
	for _, d := range p.Detections {
		if checked >= p7CropsMax || (!d.Crop && !d.Therm) {
			continue
		}
		checked++
		if d.Crop {
			b, resp, err := httpGet("http://"+web+"/v1/crop/"+d.ID, 5*time.Second)
			if err != nil || resp.StatusCode != 200 || len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
				code := ""
				if resp != nil {
					code = resp.Status
				}
				return Result{Status: Fail, Code: "E-CHK-704", Detail: fmt.Sprintf("the colour crop of %s is not served as a JPEG (%v %s)", d.ID, err, code), Evidence: ev}
			}
			if stored, err := os.ReadFile(filepath.Join(analystDir, "crops", d.UAV, d.ID+".jpg")); err == nil {
				if !bytes.Equal(stored, b) {
					return Result{Status: Fail, Code: "E-CHK-704", Detail: fmt.Sprintf("the colour crop of %s differs from the one casualty-analyst stored", d.ID), Evidence: ev}
				}
				same++
			}
		}
		if d.Therm {
			b, resp, err := httpGet("http://"+web+"/v1/thermal/"+d.ID, 5*time.Second)
			if err != nil || resp.StatusCode != 200 {
				return Result{Status: Fail, Code: "E-CHK-704", Detail: fmt.Sprintf("the thermal crop of %s is not served (%v)", d.ID, err), Evidence: ev}
			}
			if _, err := png.Decode(bytes.NewReader(b)); err != nil {
				return Result{Status: Fail, Code: "E-CHK-704", Detail: fmt.Sprintf("the thermal crop of %s is not a PNG: %v", d.ID, err), Evidence: ev}
			}
		}
		ev = append(ev, fmt.Sprintf("%s (%s, %s report, %d bytes): served", d.ID, d.Part, d.Payload, d.Bytes))
	}
	if checked == 0 {
		return Result{Status: Warn, Code: "E-CHK-704", Detail: "no recent detection with crops to check (the link may have sent metadata only)", Evidence: ev}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d detections' crops served; %d colour crops identical to casualty-analyst's", checked, same), Evidence: ev}
}

// --- P7-05 -------------------------------------------------------------------------------

func p7Timeline(web, reportDir string) Result {
	if reportDir == "" {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: "the run was not recorded (P6-03)"}
	}
	b, err := os.ReadFile(filepath.Join(reportDir, "report.json"))
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: "the after-action report is missing: " + err.Error()}
	}
	var rep aar.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: "report.json: " + err.Error()}
	}
	tb, _, err := httpGet("http://"+web+"/v1/timeline", 10*time.Second)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: "GET /v1/timeline: " + err.Error()}
	}
	var tl []*live.Item
	if err := json.Unmarshal(tb, &tl); err != nil {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: "GET /v1/timeline: " + err.Error()}
	}
	found, sent, freed, chats := map[string]bool{}, map[string]int{}, map[string]bool{}, map[string]bool{}
	for _, it := range tl {
		switch it.Kind {
		case "found":
			found[it.Ref] = true
		case "team":
			if strings.Contains(it.Text, " sent to ") {
				sent[it.Ref]++
			}
			if strings.Contains(it.Text, " finished at ") {
				freed[it.Ref] = true // done while its triage needed no team (it changed later)
			}
		case "freed":
			freed[it.Ref] = true
		case "chat":
			chats[it.Text] = true
		}
	}
	var miss []string
	for _, c := range rep.Casualties {
		if !found[c.ID] {
			miss = append(miss, c.ID+": no 'found' line")
		}
		if c.Rescued && !freed[c.ID] {
			miss = append(miss, c.ID+": rescued, no line for the team finishing there")
		}
	}
	asgPer := map[string]int{}
	for _, a := range rep.Assignments {
		asgPer[a.Cas]++
	}
	for cas, n := range asgPer {
		if sent[cas] < n {
			miss = append(miss, fmt.Sprintf("%s: %d team assignments, %d 'sent' lines", cas, n, sent[cas]))
		}
	}
	chatMiss := 0
	for _, c := range rep.Chats {
		if !chats[fmt.Sprintf("%s → %s: %s", c.From, c.To, c.Text)] {
			chatMiss++
		}
	}
	sort.Strings(miss)
	ev := []string{fmt.Sprintf("timeline: %d lines; report: %d casualty records, %d assignments, %d rescued, %d chat messages", len(tl), len(rep.Casualties), len(rep.Assignments), rep.KPI.Rescued, len(rep.Chats))}
	if chatMiss > 0 {
		ev = append(ev, fmt.Sprintf("%d chat messages of the report not in the timeline", chatMiss))
	}
	ev = append(ev, limit(miss, 10)...)
	if len(miss) > 0 {
		return Result{Status: Fail, Code: "E-CHK-705", Detail: fmt.Sprintf("%d events of the report missing from the timeline", len(miss)), Evidence: ev,
			Hint: "was sar-web restarted during the run? (it then starts its timeline anew); otherwise paste the block"}
	}
	if chatMiss > 0 {
		return Result{Status: Warn, Code: "E-CHK-705", Detail: fmt.Sprintf("every casualty, assignment and rescue in the timeline; %d chat messages missing", chatMiss), Evidence: ev}
	}
	return Result{Status: Pass, Detail: "every casualty record, team sent, rescue and chat message of the report is in the timeline", Evidence: ev}
}
