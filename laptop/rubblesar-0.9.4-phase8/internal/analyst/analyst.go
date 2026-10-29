// Package analyst is casualty-analyst's service: it stores and acknowledges
// detection reports (package intake), merges them into casualty records with
// a triage colour (package casualty) and publishes those records for
// mission-planner, sar-monitor and sar-check.
//
//	sar/{incident}/casualty/{id}   each casualty when it changes (and once
//	                               more with merged_into when it is merged away)
//	sar/{incident}/casualties      the whole list, on every change and every
//	                               publish_interval
//
// It listens to mission-planner's task list (sar/{incident}/tasks) for the
// outcome of investigations.
package analyst

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/intake"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Config configures the service.
type Config struct {
	Incident     string
	Dir          string // the incident's folder
	AckInterval  time.Duration
	SaveCrops    bool
	Source       string // header source id
	Fusion       config.Fusion
	Triage       config.Triage
	Centre       geo.Point
	GroundAltMSL float64
	// UpdateInterval: how often the casualties are recomputed (default 1 s).
	UpdateInterval time.Duration
}

// FromZBook builds the service config from the ZBook config.
func FromZBook(c *config.ZBook, source string) Config {
	return Config{Incident: c.Site.IncidentID, Dir: c.AnalystDir(), AckInterval: c.Analyst.AckInterval.Duration, SaveCrops: c.Analyst.SaveCrops,
		Source: source, Fusion: c.Analyst.Fusion, Triage: c.Analyst.Triage, Centre: geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon},
		GroundAltMSL: c.Site.GroundAltMSLm}
}

// Analyst is safe for concurrent use.
type Analyst struct {
	cfg     Config
	b       bus.Bus
	log     *slog.Logger
	in      *intake.Intake
	eng     *casualty.Engine
	list    bus.Publisher
	taskSub bus.Subscription

	mu        sync.Mutex
	pubs      map[string]bus.Publisher
	seq       msg.Seq
	st        CasualtyStats
	tasksSeen time.Time
	kick      chan struct{}
}

// CasualtyStats is the casualty side of the status endpoint.
type CasualtyStats struct {
	Count       int            `json:"count"`
	ByTriage    map[string]int `json:"by_triage"`
	Provisional int            `json:"provisional"`
	NeedLook    int            `json:"need_investigation"`
	Merged      uint64         `json:"merged"`
	Published   uint64         `json:"published"`
	Updates     uint64         `json:"updates"`
	LastUpdate  time.Time      `json:"last_update"`
	UpdateMS    float64        `json:"last_update_ms"`
	TaskLists   uint64         `json:"task_lists_received"`
	Resets      int            `json:"resets"`
	LastArchive string         `json:"last_archive,omitempty"`
	List        []Row          `json:"casualties"`
}

// Row is one casualty in the status listing.
type Row struct {
	ID          string   `json:"id"`
	Triage      string   `json:"triage"`
	Provisional bool     `json:"provisional"`
	Status      string   `json:"status"`
	CEPm        float32  `json:"cep_m"`
	UAVs        []string `json:"uavs"`
	Parts       int      `json:"parts"`
	Looks       uint32   `json:"investigations"`
	Need        string   `json:"needs_investigation,omitempty"`
	Summary     string   `json:"summary"`
}

// Open rebuilds the casualties from the stored reports and starts receiving.
func Open(cfg Config, b bus.Bus, log *slog.Logger) (*Analyst, error) {
	if cfg.UpdateInterval <= 0 {
		cfg.UpdateInterval = time.Second
	}
	if cfg.Source == "" {
		cfg.Source = "zbook/casualty-analyst"
	}
	a := &Analyst{cfg: cfg, b: b, log: log, pubs: map[string]bus.Publisher{}, kick: make(chan struct{}, 1)}
	a.eng = casualty.New(cfg.Fusion, cfg.Triage, cfg.Centre, cfg.GroundAltMSL, cfg.Dir)
	recs, err := intake.ReadRecords(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if err := a.eng.Load(recs); err != nil {
		return nil, err
	}
	if a.list, err = b.DeclarePublisher(keys.Casualties(cfg.Incident), bus.QoSCasualty); err != nil {
		return nil, err
	}
	if a.taskSub, err = b.Subscribe(keys.Tasks(cfg.Incident), a.onTasks); err != nil {
		return nil, err
	}
	a.in, err = intake.Open(intake.Config{Incident: cfg.Incident, Dir: cfg.Dir, AckInterval: cfg.AckInterval, SaveCrops: cfg.SaveCrops,
		Source: cfg.Source, OnRecord: func(r intake.Record, _ *sarpb.Detection) { a.eng.Add(r) }}, b, log)
	if err != nil {
		_ = a.taskSub.Close()
		return nil, err
	}
	return a, nil
}

func (a *Analyst) onTasks(s bus.Sample) {
	var tl sarpb.TaskList
	if msg.Unmarshal(s.Payload, &tl) != nil || msg.CheckHeader(tl.GetHeader(), a.cfg.Incident) != nil {
		return
	}
	a.eng.ApplyTasks(tl.Tasks)
	a.mu.Lock()
	a.st.TaskLists++
	a.tasksSeen = time.Now()
	a.mu.Unlock()
}

// Intake is the report store (for its statistics).
func (a *Analyst) Intake() *intake.Intake { return a.in }

// Casualties returns the current list.
func (a *Analyst) Casualties() []*sarpb.Casualty { return a.eng.List() }

// Run acknowledges and stores reports, recomputes the casualties every
// UpdateInterval and publishes them, until ctx ends.
func (a *Analyst) Run(ctx context.Context, publishEvery time.Duration) {
	if publishEvery <= 0 {
		publishEvery = 10 * time.Second
	}
	done := make(chan struct{})
	go func() { a.in.Run(ctx); close(done) }()
	up := time.NewTicker(a.cfg.UpdateInterval)
	defer up.Stop()
	pub := time.NewTicker(publishEvery)
	defer pub.Stop()
	a.update(true)
	for {
		select {
		case <-ctx.Done():
			<-done
			a.update(false)
			return
		case <-up.C:
			a.update(false)
		case <-a.kick:
			a.update(true)
		case <-pub.C:
			a.publishList()
		}
	}
}

// update recomputes; it publishes what changed (and the list, if anything did or force).
func (a *Analyst) update(force bool) {
	t0 := time.Now()
	changed, removed := a.eng.Update(a.cfg.Incident, a.cfg.Source)
	ms := float64(time.Since(t0).Microseconds()) / 1000
	for _, c := range removed {
		a.log.Info("casualty merged", "id", c.CasualtyId, "into", c.MergedInto)
		a.put(c)
	}
	for _, c := range changed {
		lvl := slog.LevelDebug
		if c.Revision == 1 {
			lvl = slog.LevelInfo
		}
		a.log.Log(context.Background(), lvl, "casualty", "id", c.CasualtyId, "rev", c.Revision, "triage", casualty.TriageName(c.Triage),
			"provisional", c.Provisional, "needs_investigation", c.NeedsInvestigation, "summary", c.Summary)
		a.put(c)
	}
	a.mu.Lock()
	a.st.Merged += uint64(len(removed))
	if len(changed)+len(removed) > 0 {
		a.st.Updates++
		a.st.LastUpdate, a.st.UpdateMS = time.Now(), ms
	}
	a.mu.Unlock()
	if force || len(changed)+len(removed) > 0 {
		a.publishList()
	}
}

func (a *Analyst) put(c *sarpb.Casualty) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pubs[c.CasualtyId]
	if p == nil {
		var err error
		if p, err = a.b.DeclarePublisher(keys.Casualty(a.cfg.Incident, c.CasualtyId), bus.QoSCasualty); err != nil {
			a.log.Warn("publishing a casualty", "id", c.CasualtyId, "err", err)
			return
		}
		a.pubs[c.CasualtyId] = p
	}
	b, err := msg.Marshal(c)
	if err == nil {
		err = p.Put(b)
	}
	if err != nil {
		a.log.Warn("publishing a casualty", "id", c.CasualtyId, "err", err)
		return
	}
	a.st.Published++
}

func (a *Analyst) publishList() {
	cs := a.eng.List()
	a.mu.Lock()
	h := msg.NewHeader(a.cfg.Incident, a.cfg.Source, a.seq.Next())
	a.mu.Unlock()
	b, err := msg.Marshal(&sarpb.CasualtyList{Header: h, Casualties: cs})
	if err == nil {
		err = a.list.Put(b)
	}
	if err != nil {
		a.log.Warn("publishing the casualty list", "err", err)
	}
}

// Reset archives the incident folder (intake.Reset) and forgets every
// casualty, for a new acceptance run. It returns the archive folder.
func (a *Analyst) Reset(stamp string) (string, error) {
	dst, err := a.in.Reset(stamp, a.eng.Reset)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.st.Resets++
	a.st.LastArchive = dst
	a.st.Merged, a.st.TaskLists = 0, 0
	a.mu.Unlock()
	a.log.Info("reset: previous run archived", "archive", dst)
	select {
	case a.kick <- struct{}{}:
	default:
	}
	return dst, nil
}

// Stats is the casualty summary.
func (a *Analyst) Stats() CasualtyStats {
	cs := a.eng.List()
	a.mu.Lock()
	s := a.st
	a.mu.Unlock()
	s.Count, s.ByTriage, s.Provisional, s.NeedLook, s.List = len(cs), map[string]int{}, 0, 0, nil
	for _, c := range cs {
		s.ByTriage[casualty.TriageName(c.Triage)]++
		if c.Provisional {
			s.Provisional++
		}
		if c.NeedsInvestigation {
			s.NeedLook++
		}
		s.List = append(s.List, Row{ID: c.CasualtyId, Triage: casualty.TriageName(c.Triage), Provisional: c.Provisional,
			Status: strings.ToLower(strings.TrimPrefix(c.Status.String(), "CASUALTY_STATUS_")), CEPm: c.CepM, UAVs: c.UavIds,
			Parts: len(c.Parts), Looks: c.Investigations, Need: c.InvestigationReason, Summary: c.Summary})
	}
	return s
}

// Text renders the casualty summary as a table.
func (s CasualtyStats) Text() string {
	var b strings.Builder
	var tri []string
	for _, k := range []string{"Red", "Yellow", "Green", "Black", "Grey"} {
		if n := s.ByTriage[k]; n > 0 {
			tri = append(tri, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(tri) == 0 {
		tri = []string{"none yet"}
	}
	fmt.Fprintf(&b, "casualties: %d (%s), %d provisional, %d need a close look, %d merged away, %d task lists received\n",
		s.Count, strings.Join(tri, ", "), s.Provisional, s.NeedLook, s.Merged, s.TaskLists)
	if len(s.List) == 0 {
		return b.String()
	}
	rows := append([]Row(nil), s.List...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	fmt.Fprintf(&b, "%-6s %-8s %-18s %6s %5s %-22s %s\n", "ID", "TRIAGE", "STATUS", "CEP m", "LOOKS", "SEEN BY", "WHY")
	for _, r := range rows {
		t := r.Triage
		if r.Provisional {
			t += "?"
		}
		why := r.Summary
		if i := strings.Index(why, "-> "); i >= 0 {
			why = why[i+3:]
		}
		if r.Need != "" {
			why += " [needs a look]"
		}
		fmt.Fprintf(&b, "%-6s %-8s %-18s %6.1f %5d %-22s %s\n", r.ID, t, strings.ReplaceAll(r.Status, "_", " "), r.CEPm, r.Looks, strings.Join(r.UAVs, ","), why)
	}
	b.WriteString("(? = provisional: may change after a close look)\n")
	return b.String()
}

// Close stops receiving.
func (a *Analyst) Close() {
	if a.taskSub != nil {
		_ = a.taskSub.Close()
	}
	a.in.Close()
}
