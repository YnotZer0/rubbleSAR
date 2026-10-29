package evalseg

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/scenario"
)

// Evaluation method version. 1 = phase 2.0 (every visible piece of a part is
// its own object); 2 adds the merged view below. Reports keep the method-1
// numbers so older evaluations stay comparable.
const Method = 2

// MergedTruth extracts one region per visible body part of each victim: all
// the pieces of a part that debris splits apart count as one object, which is
// how a rescuer (and the tracker) sees it. Parts smaller than minPx in total
// are not expected.
func MergedTruth(parts, inst *raster.U8, minPx int) []Truth {
	key := func(i int) uint32 {
		p := parts.Pix[i]
		if p == 0 || int(p) >= scenario.NumParts {
			return 0
		}
		return uint32(inst.Pix[i])<<8 | uint32(p)
	}
	groups := map[uint32][]masks.Component{}
	var order []uint32
	for _, c := range masks.Components(parts.W, parts.H, key) {
		if _, ok := groups[c.Key]; !ok {
			order = append(order, c.Key)
		}
		groups[c.Key] = append(groups[c.Key], c)
	}
	var out []Truth
	for _, k := range order {
		c := masks.Merge(groups[k])
		if c.Area() < minPx {
			continue
		}
		out = append(out, Truth{Class: int(k&0xff) - 1, Victim: int(k >> 8), Mask: c.Bitmap(parts.W), Area: c.Area(), CX: c.CX, CY: c.CY})
	}
	return out
}

// SizeBands are the visible-area bands (pixels) recall is broken down by.
var SizeBands = []int{0, 25, 100, 400}

func bandName(i int) string {
	if i == len(SizeBands)-1 {
		return fmt.Sprintf(">= %d px", SizeBands[i])
	}
	return fmt.Sprintf("%d-%d px", SizeBands[i], SizeBands[i+1])
}

func bandOf(area int) int {
	b := 0
	for i, lo := range SizeBands {
		if area >= lo {
			b = i
		}
	}
	return b
}

// SizeReport is recall for parts of one size band (merged truth).
type SizeReport struct {
	Band        string  `json:"band"`
	Truth       int     `json:"truth"`
	Found       int     `json:"found_any_class"`
	RightClass  int     `json:"found_right_class"`
	RecallAny   float64 `json:"recall_any_class"`
	RecallClass float64 `json:"recall_right_class"`
}

// VictimReport is person-level detection for one kind of view: a victim
// counts as visible when at least MinVictimPx of its body shows, and as found
// when any detection (any class) touches it.
type VictimReport struct {
	Kind    string  `json:"kind"`
	Visible int     `json:"visible"`
	Found   int     `json:"found"`
	Recall  float64 `json:"recall"`
}

// MinVictimPx is the visible body area (pixels) below which a person is not
// expected to be found in a frame.
const MinVictimPx = 25

// Merged is the method-2 view of an evaluation.
type Merged struct {
	Classes   []ClassReport  `json:"classes"`
	Overall   ClassReport    `json:"overall"`
	Kinds     []KindReport   `json:"kinds"`
	Sizes     []SizeReport   `json:"sizes"`
	Victims   []VictimReport `json:"victims"`
	Confusion Confusion      `json:"confusion"`
}

// Confusion counts, for every merged true part, which class the best
// overlapping detection gave it ("missed" when none overlaps).
type Confusion struct {
	Labels []string `json:"labels"` // truth rows and detection columns: classes then "missed"
	Counts [][]int  `json:"counts"` // [truth class][detected class or missed]
	// FalseAlarms counts detections touching no labelled body pixel, per detected class.
	FalseAlarms []int `json:"false_alarms"`
}

func newConfusion() Confusion {
	c := Confusion{Labels: append(append([]string{}, infer.ClassNames...), "missed"), FalseAlarms: make([]int, len(infer.ClassNames))}
	c.Counts = make([][]int, len(infer.ClassNames))
	for i := range c.Counts {
		c.Counts[i] = make([]int, len(infer.ClassNames)+1)
	}
	return c
}

// mergedAcc accumulates the method-2 results over frames.
type mergedAcc struct {
	per   [5]acc
	kinds map[string]*KindReport
	sizes []SizeReport
	vic   map[string]*VictimReport
	conf  Confusion
}

func newMergedAcc() *mergedAcc {
	m := &mergedAcc{kinds: map[string]*KindReport{}, vic: map[string]*VictimReport{}, conf: newConfusion()}
	for i := range SizeBands {
		m.sizes = append(m.sizes, SizeReport{Band: bandName(i)})
	}
	return m
}

// touches reports whether a detection overlaps any pixel of the mask, or
// its centre lies within r pixels of it.
func touches(d *infer.Detection, m *masks.Bitmap, r float64) bool {
	x0, y0 := max(d.Mask.X0, m.X0), max(d.Mask.Y0, m.Y0)
	x1, y1 := min(d.Mask.X0+d.Mask.W, m.X0+m.W), min(d.Mask.Y0+d.Mask.H, m.Y0+m.H)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if d.Mask.At(x, y) && m.At(x, y) {
				return true
			}
		}
	}
	return near(m, d.CX, d.CY, r)
}

func (a *mergedAcc) add(kind string, ds []infer.Detection, ts []Truth, o Options) {
	flags, _, _ := MatchFrame(ds, ts, o)
	k := a.kinds[kind]
	if k == nil {
		k = &KindReport{Kind: kind}
		a.kinds[kind] = k
	}
	k.Frames++
	k.Truth += len(ts)
	k.Detections += len(ds)
	for i, d := range ds {
		if d.Class < 0 || d.Class > 4 {
			continue
		}
		a.per[d.Class].flags = append(a.per[d.Class].flags, flags[i])
		a.per[d.Class].det++
		if flags[i].strict {
			a.per[d.Class].tp++
			k.TP++
		}
		if flags[i].loose {
			a.per[d.Class].tpl++
			k.TPLenient++
		}
	}
	// Per true part: size band, any-class and right-class finding, confusion.
	for _, t := range ts {
		a.per[t.Class].truth++
		best, bestIoU := -1, 0.0
		for i := range ds {
			d := &ds[i]
			if !touches(d, &t.Mask, o.LenientPx) {
				continue
			}
			v := masks.IoU(&d.Mask, &t.Mask) + 1e-6 // touching counts even at IoU 0
			if d.Class == t.Class {
				v += 1 // prefer a detection of the right class when several touch
			}
			if v > bestIoU {
				best, bestIoU = i, v
			}
		}
		s := &a.sizes[bandOf(t.Area)]
		s.Truth++
		if best >= 0 {
			s.Found++
			if ds[best].Class == t.Class {
				s.RightClass++
			}
			if c := ds[best].Class; c >= 0 && c < 5 {
				a.conf.Counts[t.Class][c]++
			}
		} else {
			a.conf.Counts[t.Class][len(infer.ClassNames)]++
		}
	}
	// Detections touching no labelled pixel at all.
	for i := range ds {
		d := &ds[i]
		hit := false
		for j := range ts {
			if touches(d, &ts[j].Mask, 0) {
				hit = true
				break
			}
		}
		if !hit && d.Class >= 0 && d.Class < 5 {
			a.conf.FalseAlarms[d.Class]++
		}
	}
	// Person level.
	area := map[int]int{}
	for _, t := range ts {
		area[t.Victim] += t.Area
	}
	v := a.vic[kind]
	if v == nil {
		v = &VictimReport{Kind: kind}
		a.vic[kind] = v
	}
	for vid, px := range area {
		if px < MinVictimPx {
			continue
		}
		v.Visible++
		found := false
		for _, t := range ts {
			if t.Victim != vid {
				continue
			}
			for i := range ds {
				if touches(&ds[i], &t.Mask, o.LenientPx) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			v.Found++
		}
	}
}

func (a *mergedAcc) finish() *Merged {
	m := &Merged{Confusion: a.conf}
	m.Classes, m.Overall = classReports(a.per)
	for _, k := range a.kinds {
		finishKind(k)
		m.Kinds = append(m.Kinds, *k)
	}
	sort.Slice(m.Kinds, func(i, j int) bool { return m.Kinds[i].Kind < m.Kinds[j].Kind })
	for _, s := range a.sizes {
		s.RecallAny, s.RecallClass = ratio(s.Found, s.Truth), ratio(s.RightClass, s.Truth)
		m.Sizes = append(m.Sizes, s)
	}
	var all VictimReport
	all.Kind = "all"
	for _, v := range a.vic {
		v.Recall = ratio(v.Found, v.Visible)
		m.Victims = append(m.Victims, *v)
		all.Visible += v.Visible
		all.Found += v.Found
	}
	sort.Slice(m.Victims, func(i, j int) bool { return m.Victims[i].Kind < m.Victims[j].Kind })
	all.Recall = ratio(all.Found, all.Visible)
	m.Victims = append(m.Victims, all)
	return m
}

// Kind returns the merged report for one kind of view (zero value if absent).
func (m *Merged) Kind(k string) KindReport {
	for _, x := range m.Kinds {
		if x.Kind == k {
			return x
		}
	}
	return KindReport{Kind: k}
}

// VictimsFor returns the person-level result for one kind ("all" for every view).
func (m *Merged) VictimsFor(k string) VictimReport {
	for _, x := range m.Victims {
		if x.Kind == k {
			return x
		}
	}
	return VictimReport{Kind: k}
}

// VictimsPrefix sums the person-level results of every kind starting with p
// (e.g. "investigator" or "scout").
func (m *Merged) VictimsPrefix(p string) VictimReport {
	r := VictimReport{Kind: p}
	for _, x := range m.Victims {
		if x.Kind != "all" && strings.HasPrefix(x.Kind, p) {
			r.Visible += x.Visible
			r.Found += x.Found
		}
	}
	r.Recall = ratio(r.Found, r.Visible)
	return r
}

// Text renders the merged view.
func (m *Merged) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Each visible body part counted once (pieces split by debris merged):\n")
	fmt.Fprintf(&b, "%-7s %6s %6s %5s %9s %7s %8s %9s %9s\n", "class", "truth", "dets", "TP", "precision", "recall", "AP50", "P(len.)", "R(len.)")
	for _, c := range append(m.Classes, m.Overall) {
		fmt.Fprintf(&b, "%-7s %6d %6d %5d %9.3f %7.3f %8.3f %9.3f %9.3f\n", c.Class, c.Truth, c.Detections, c.TP, c.Precision, c.Recall, c.AP50, c.PrecisionLenient, c.RecallLenient)
	}
	fmt.Fprintf(&b, "\n%-22s %6s %6s %7s %9s %12s\n", "view", "frames", "truth", "recall", "R(len.)", "false/frame")
	for _, k := range m.Kinds {
		fmt.Fprintf(&b, "%-22s %6d %6d %7.3f %9.3f %12.2f\n", k.Kind, k.Frames, k.Truth, k.Recall, k.RecallLenient, k.FalsePerFrame)
	}
	fmt.Fprintf(&b, "\nby visible size        truth  found (any class)  right class\n")
	for _, s := range m.Sizes {
		fmt.Fprintf(&b, "  %-18s %6d  %5d  %5.0f%%       %5.0f%%\n", s.Band, s.Truth, s.Found, s.RecallAny*100, s.RecallClass*100)
	}
	fmt.Fprintf(&b, "\npeople found (any detection touching a visible person, %d px or more):\n", MinVictimPx)
	for _, v := range m.Victims {
		fmt.Fprintf(&b, "  %-22s %3d of %3d  (%3.0f%%)\n", v.Kind, v.Found, v.Visible, v.Recall*100)
	}
	b.WriteString("\n" + m.Confusion.Text())
	return b.String()
}

// Text renders the confusion table.
func (c Confusion) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "true part -> detected as (best overlapping detection)\n%-7s", "")
	for _, l := range c.Labels {
		fmt.Fprintf(&b, " %7s", l)
	}
	b.WriteString("\n")
	for i, row := range c.Counts {
		fmt.Fprintf(&b, "%-7s", c.Labels[i])
		for _, v := range row {
			fmt.Fprintf(&b, " %7d", v)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%-7s", "no part")
	for _, v := range c.FalseAlarms {
		fmt.Fprintf(&b, " %7d", v)
	}
	b.WriteString("       -   (false alarms: detections touching no body part)\n")
	return b.String()
}

// Rows renders the confusion table as one line per true class (for check evidence).
func (c Confusion) Rows() []string {
	var out []string
	for i, row := range c.Counts {
		var parts []string
		for j, v := range row {
			if v > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", c.Labels[j], v))
			}
		}
		out = append(out, fmt.Sprintf("true %-5s -> %s", c.Labels[i], strings.Join(parts, ", ")))
	}
	var fa []string
	for j, v := range c.FalseAlarms {
		if v > 0 {
			fa = append(fa, fmt.Sprintf("%s %d", c.Labels[j], v))
		}
	}
	if len(fa) > 0 {
		out = append(out, "no part   -> "+strings.Join(fa, ", "))
	}
	return out
}

func finishKind(k *KindReport) {
	k.Precision, k.Recall, k.RecallLenient = ratio(k.TP, k.Detections), ratio(k.TP, k.Truth), ratio(k.TPLenient, k.Truth)
	if k.Frames > 0 {
		k.FalsePerFrame = math.Round(float64(k.Detections-k.TPLenient)/float64(k.Frames)*100) / 100
	}
}

// classReports turns per-class accumulators into reports plus the overall row
// (AP: mean over classes with truth).
func classReports(per [5]acc) ([]ClassReport, ClassReport) {
	var out []ClassReport
	all := ClassReport{Class: "all"}
	for c := 0; c < 5; c++ {
		a := per[c]
		cr := ClassReport{Class: infer.ClassNames[c], Truth: a.truth, Detections: a.det, TP: a.tp, FP: a.det - a.tp, FN: a.truth - a.tp,
			Precision: ratio(a.tp, a.det), Recall: ratio(a.tp, a.truth), AP50: AP(a.flags, a.truth, true),
			TPLenient: a.tpl, PrecisionLenient: ratio(a.tpl, a.det), RecallLenient: ratio(a.tpl, a.truth)}
		out = append(out, cr)
		all.Truth += a.truth
		all.Detections += a.det
		all.TP += a.tp
		all.TPLenient += a.tpl
	}
	all.FP, all.FN = all.Detections-all.TP, all.Truth-all.TP
	all.Precision, all.Recall = ratio(all.TP, all.Detections), ratio(all.TP, all.Truth)
	all.PrecisionLenient, all.RecallLenient = ratio(all.TPLenient, all.Detections), ratio(all.TPLenient, all.Truth)
	n, sum := 0, 0.0
	for _, cr := range out {
		if cr.Truth > 0 {
			sum += cr.AP50
			n++
		}
	}
	if n > 0 {
		all.AP50 = math.Round(sum/float64(n)*1000) / 1000
	}
	return out, all
}
