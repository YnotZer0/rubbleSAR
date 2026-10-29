package dispatch

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/roads"
	"rubblesar/internal/scenario"
)

// Extrication: how long it takes a team on scene to free a casualty from the
// rubble. The simulation knows how each victim is trapped (the scenario: the
// kind of collapse, the depth of the pile, what shows above it and how much
// of that is under debris); a real team would learn it on arrival.
//
// Illustrative times, in the spirit of INSARAG's light / medium / heavy
// rescue, awaiting review by USAR practitioners like the triage rules (D9):
//
//	base           pancake collapse 90 min, rubble mound 50 min,
//	               no collapse known here 20 min
//	pile depth     x (0.6 + 0.15 per metre), 0.7..1.5
//	what shows     head or back uncovered x0.6; a whole arm x0.8;
//	               only a hand, foot or lower leg x1.3
//	under debris   x (1 + the share hidden)
//	team           vehicle teams carry heavier tools x0.8
//	variation      x0.75..1.35 per casualty and run
//
// clamped to 15 min..6 h and rounded to 5 min. A casualty no longer Red or
// Yellow on arrival (Black) takes 10 min to confirm and mark.

// extrication is the estimate for one casualty.
type extrication struct {
	Dur   time.Duration // real time
	Class string        // "light rescue", "medium rescue", "heavy rescue", "confirm and mark"
	Why   string        // "pancake collapse under 2.6 m of rubble; only a forearm and hand showing, 7% under debris"
}

// Words is the estimate as one phrase: "heavy rescue, about 2 h 10 min (pancake ...)".
func (e extrication) Words() string {
	if e.Why == "" {
		return fmt.Sprintf("%s, about %s", e.Class, hmin(e.Dur))
	}
	return fmt.Sprintf("%s, about %s (%s)", e.Class, hmin(e.Dur), e.Why)
}

type sceneVictim struct {
	id      string
	pos     geom.P
	site    *sceneSite
	exposed []string
	hidden  float64
}

type sceneSite struct {
	id     string
	kind   string
	height float64
	area   geom.Poly // footprint, local metres
}

// sceneModel is what the scenario says about how victims are trapped.
type sceneModel struct {
	victims []sceneVictim
	sites   []*sceneSite
}

// newSceneModel reads the victims and collapsed buildings (nil scenario: no model).
func newSceneModel(s *scenario.Scenario, local geo.Local) *sceneModel {
	if s == nil {
		return nil
	}
	m := &sceneModel{}
	sl := geo.NewLocal(geo.Point{Lat: s.Center.Lat, Lon: s.Center.Lon})
	byID := map[string]*sceneSite{}
	for _, st := range s.Sites {
		x := &sceneSite{id: st.ID, kind: st.Kind, height: st.HeightM}
		for _, v := range st.Footprint {
			g := sl.ToPoint(v[0], v[1])
			e, n := local.ToLocal(g)
			x.area = append(x.area, geom.P{X: e, Y: n})
		}
		m.sites = append(m.sites, x)
		byID[st.ID] = x
	}
	for _, v := range s.Victims {
		e, n := local.ToLocal(geo.Point{Lat: v.Pos.Lat, Lon: v.Pos.Lon})
		x := sceneVictim{id: v.ID, pos: geom.P{X: e, Y: n}, site: byID[v.SiteID], hidden: v.OcclusionFrac}
		for _, ex := range v.Exposed {
			x.exposed = append(x.exposed, ex.Kind)
		}
		m.victims = append(m.victims, x)
	}
	return m
}

const (
	victimMatchM = 5.0  // a casualty this close to a scenario victim is that victim
	siteNearM    = 12.0 // ... or this close to a collapsed building is in its rubble
)

// estimate is the extrication of a casualty at pos for a team of mode, in run.
func (m *sceneModel) estimate(casID string, pos geom.P, mode roads.Mode, run string) extrication {
	var v *sceneVictim
	if m != nil {
		best := victimMatchM
		for i := range m.victims {
			if d := m.victims[i].pos.Dist(pos); d <= best {
				best, v = d, &m.victims[i]
			}
		}
	}
	var site *sceneSite
	switch {
	case v != nil:
		site = v.site
	case m != nil:
		best := siteNearM
		for _, s := range m.sites {
			d := s.area.SignedDist(pos)
			if d < best {
				best, site = d, s
			}
		}
	}
	var why []string
	base := 20.0
	if site != nil {
		switch site.kind {
		case "pancake":
			base = 90
		case "mound":
			base = 50
		default:
			base = 60
		}
		base *= clamp(0.6+0.15*site.height, 0.7, 1.5)
		why = append(why, siteWords(site))
	} else {
		why = append(why, "no collapsed building known here")
	}
	if v != nil {
		f, w := exposureFactor(v.exposed)
		base *= f * (1 + v.hidden)
		if w != "" {
			why = append(why, fmt.Sprintf("%s, %.0f%% covered", w, 100*v.hidden))
		}
	}
	if mode == roads.Vehicle {
		base *= 0.8
		why = append(why, "heavier tools on the vehicle")
	}
	h := fnv.New32a()
	h.Write([]byte(run + "|" + casID))
	base *= 0.75 + 0.6*float64(h.Sum32()%1000)/999
	mins := math.Round(clamp(base, 15, 360)/5) * 5
	class := "light rescue"
	switch {
	case mins >= 120:
		class = "heavy rescue"
	case mins >= 45:
		class = "medium rescue"
	}
	return extrication{Dur: time.Duration(mins) * time.Minute, Class: class, Why: strings.Join(why, "; ")}
}

// confirmAndMark is a casualty found dead on arrival (no longer Red or Yellow).
func confirmAndMark() extrication {
	return extrication{Dur: 10 * time.Minute, Class: "confirm and mark", Why: "no signs of life on arrival: marked for recovery later"}
}

func exposureFactor(kinds []string) (float64, string) {
	if len(kinds) == 0 {
		return 1, ""
	}
	has := map[string]bool{}
	var words []string
	for _, k := range kinds {
		has[k] = true
		words = append(words, exposureWord(k))
	}
	w := strings.Join(words, ", ")
	if len(words) == 2 && !strings.Contains(w, " and ") {
		w = words[0] + " and " + words[1]
	}
	switch {
	case has[scenario.ExpHead] || has[scenario.ExpHeadShoulders] || has[scenario.ExpBack]:
		return 0.6, w + " uncovered"
	case has[scenario.ExpArmFull]:
		return 0.8, w + " free"
	}
	return 1.3, "only " + w + " showing"
}

func exposureWord(k string) string {
	switch k {
	case scenario.ExpHand:
		return "a hand"
	case scenario.ExpForearmHand:
		return "a forearm and hand"
	case scenario.ExpArmFull:
		return "a whole arm"
	case scenario.ExpFoot:
		return "a foot"
	case scenario.ExpLowerLeg:
		return "a lower leg"
	case scenario.ExpHead:
		return "the head"
	case scenario.ExpHeadShoulders:
		return "head and shoulders"
	case scenario.ExpBack:
		return "part of the back"
	}
	return strings.ReplaceAll(k, "_", " ")
}

// siteWords: "pancake collapse, 2.6 m of rubble", "rubble mound 4.1 m high".
func siteWords(s *sceneSite) string {
	switch s.kind {
	case "pancake":
		return fmt.Sprintf("pancake collapse, %.1f m of rubble", s.height)
	case "mound":
		return fmt.Sprintf("rubble mound %.1f m high", s.height)
	}
	return fmt.Sprintf("%s collapse, %.1f m of rubble", s.kind, s.height)
}

func clamp(v, lo, hi float64) float64 { return math.Min(hi, math.Max(lo, v)) }

// hmin is "2 h 10 min", "45 min", "30 s".
func hmin(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("%.0f s", d.Seconds())
	}
	m := int(math.Round(d.Minutes()))
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
