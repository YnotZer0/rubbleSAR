package roads

import (
	"fmt"
	"math"
	"sort"

	"rubblesar/internal/geom"
)

// nearestNamed is the named street nearest to q (any street, open or closed),
// leaving out the names in skip, and how far it is.
func (n *Network) nearestNamed(q geom.P, skip map[string]bool) (string, float64) {
	best, bd := "", math.Inf(1)
	for _, e := range n.Edges {
		if e.Name == "" || skip[e.Name] {
			continue
		}
		if d, _ := geom.SegDist(q, n.Nodes[e.A], n.Nodes[e.B]); d < bd {
			best, bd = e.Name, d
		}
	}
	return best, bd
}

// SiteNames names collapse sites after the nearest named street ("Gündüz
// Caddesi"), so a map reads as a place rather than a numbered list; two
// sites by the same street get the nearest other street too ("Gündüz
// Caddesi by Atatürk Caddesi"). Sites with no named street within maxM keep
// no name (the caller uses its id). centres: id -> local metres.
func (n *Network) SiteNames(centres map[string]geom.P, maxM float64) map[string]string {
	out := map[string]string{}
	if n == nil {
		return out
	}
	ids := make([]string, 0, len(centres))
	for id := range centres {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	by := map[string][]string{}
	for _, id := range ids {
		if name, d := n.nearestNamed(centres[id], nil); name != "" && d <= maxM {
			out[id] = name
			by[name] = append(by[name], id)
		}
	}
	for name, same := range by {
		if len(same) < 2 {
			continue
		}
		seen := map[string]int{}
		for _, id := range same {
			if other, d := n.nearestNamed(centres[id], map[string]bool{name: true}); other != "" && d <= 2*maxM {
				out[id] = name + " by " + other
			}
			seen[out[id]]++
		}
		// Still the same: number them, west to east.
		for full, k := range seen {
			if k < 2 {
				continue
			}
			var dup []string
			for _, id := range same {
				if out[id] == full {
					dup = append(dup, id)
				}
			}
			sort.Slice(dup, func(i, j int) bool { return centres[dup[i]].X < centres[dup[j]].X })
			for i, id := range dup {
				if len(dup) == 2 {
					out[id] = full + []string{" (west)", " (east)"}[i]
				} else {
					out[id] = fmt.Sprintf("%s (%d)", full, i+1)
				}
			}
		}
	}
	return out
}
