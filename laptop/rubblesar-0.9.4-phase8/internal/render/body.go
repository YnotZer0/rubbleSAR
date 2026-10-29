package render

import (
	"math"
	"math/rand"

	"rubblesar/internal/geom"
	"rubblesar/internal/scenario"
)

// prim is one body primitive (a capsule or an ellipse) with its looks.
type prim struct {
	sh     shape
	class  int
	mat    uint8
	col    [3]float64
	couple uint8
	lift   float64 // height of the primitive's top above the body base, metres
	// Head: hair where the along-axis coordinate is below hairX (ellipse units).
	hair    bool
	hairX   float64
	hairCol [3]float64
	exp     int // index of the exposure in the victim
}

// exposureAxis describes the exposed region for occluder placement:
// it starts at P0 (where it leaves the rubble) and runs along U for L metres.
type exposureAxis struct {
	P0     geom.P
	U      geom.P
	L      float64
	Radius float64
	Anchor geom.P
	Swing  float64 // degrees for pose B
}

// victimGeom is everything the renderer draws for one victim.
type victimGeom struct {
	v      scenario.Victim
	z0     float64 // base height of the body (rubble surface at the victim)
	primsA []prim
	primsB []prim // nil for still victims
	axes   []exposureAxis
	occl   []occluder
}

type occluder struct {
	sh  shape
	mat uint8
	col [3]float64
	z   float64
}

func vecOf(deg float64) geom.P { return dirVec(deg) }

// buildVictim creates the body primitives for both poses.
func buildVictim(v scenario.Victim, seed int64) *victimGeom {
	rng := rand.New(rand.NewSource(seed))
	s := v.Look.Scale
	if s <= 0 {
		s = 1
	}
	skin := skinTones[v.Look.SkinTone%len(skinTones)]
	top := clothColours[v.Look.ClothColor%len(clothColours)]
	trousers := clothColours[(v.Look.ClothColor+3)%len(clothColours)]
	hair := hairColours[v.Look.HairColor%len(hairColours)]
	shoe := materials[matShoe].rgb
	if rng.Float64() < 0.5 {
		shoe = [3]float64{92, 64, 44}
	}
	bareFoot := rng.Float64() < 0.3
	g := &victimGeom{v: v}

	addCap := func(ei int, a, b geom.P, r float64, class int, mat uint8, col [3]float64, cp uint8) {
		g.primsA = append(g.primsA, prim{sh: capsule{a, b, r}, class: class, mat: mat, col: col, couple: cp, lift: r, exp: ei})
	}
	addEll := func(ei int, c, u geom.P, ax, ay float64, class int, mat uint8, col [3]float64, cp uint8, lift float64) {
		g.primsA = append(g.primsA, prim{sh: ellipse{c, u, ax, ay}, class: class, mat: mat, col: col, couple: cp, lift: lift, exp: ei})
	}
	// hand draws a palm and fingers starting at a (wrist) along u.
	hand := func(ei int, a, u geom.P, side string) {
		w := rightOf(u)
		fist := rng.Float64() < 0.3
		addEll(ei, a.Add(u.Scale(0.045*s)), u, 0.05*s, 0.042*s, scenario.PartHand, matSkin, skin, coupleSkin, 0.025*s)
		lengths := []float64{0.07, 0.08, 0.075, 0.06}
		for k := 0; k < 4; k++ {
			l := lengths[k] * s * (0.9 + 0.2*rng.Float64())
			if fist {
				l *= 0.35
			}
			base := a.Add(u.Scale(0.085 * s)).Add(w.Scale((-0.027 + 0.018*float64(k)) * s))
			fan := (-9 + 6*float64(k)) + 6*(rng.Float64()-0.5)
			tip := base.Add(u.Rot(-fan * math.Pi / 180).Scale(l))
			addCap(ei, base, tip, 0.0085*s, scenario.PartHand, matSkin, skin, coupleSkin)
		}
		sgn := 1.0
		if side == "left" {
			sgn = -1
		}
		tb := a.Add(u.Scale(0.035 * s)).Add(w.Scale(sgn * 0.035 * s))
		td := u.Rot(-sgn * (45 + 15*rng.Float64()) * math.Pi / 180)
		addCap(ei, tb, tb.Add(td.Scale(0.055*s)), 0.01*s, scenario.PartHand, matSkin, skin, coupleSkin)
	}
	// forearm from a along u; returns the wrist.
	forearm := func(ei int, a, u geom.P) geom.P {
		addCap(ei, a, a.Add(u.Scale(0.16*s)), 0.042*s, scenario.PartArm, matCloth, top, coupleCloth)
		addCap(ei, a.Add(u.Scale(0.15*s)), a.Add(u.Scale(0.25*s)), 0.032*s, scenario.PartArm, matSkin, skin, coupleSkin)
		return a.Add(u.Scale(0.25 * s))
	}
	headAt := func(ei int, c, u geom.P) {
		addEll(ei, c, u, 0.11*s, 0.085*s, scenario.PartHead, matHair, hair, coupleHair, 0.09*s)
		p := &g.primsA[len(g.primsA)-1]
		p.hair = true
		p.hairX = 0.35
		if rng.Float64() < 0.3 {
			p.hairX = -0.15 // more face visible
		}
		p.hairCol = hair
		p.col = skin
		p.mat = matSkin
		p.couple = coupleSkin
	}

	for ei, e := range v.Exposed {
		a := geom.P{X: e.AnchorE, Y: e.AnchorN}
		u := vecOf(e.DirDeg)
		ax := exposureAxis{P0: a, U: u, Anchor: a, Swing: v.Movement.SwingDeg}
		switch e.Kind {
		case scenario.ExpHand:
			hand(ei, a, u, e.Side)
			ax.L, ax.Radius = 0.17*s, 0.045*s
		case scenario.ExpForearmHand:
			wr := forearm(ei, a, u)
			hand(ei, wr, u, e.Side)
			ax.L, ax.Radius = 0.42*s, 0.045*s
		case scenario.ExpArmFull:
			el := a.Add(u.Scale(0.29 * s))
			addCap(ei, a, el, 0.05*s, scenario.PartArm, matCloth, top, coupleCloth)
			bend := (15 + 45*rng.Float64())
			if rng.Intn(2) == 0 {
				bend = -bend
			}
			u2 := u.Rot(-bend * math.Pi / 180)
			wr := forearm(ei, el, u2)
			hand(ei, wr, u2, e.Side)
			ax.L, ax.Radius = 0.3*s, 0.05*s
		case scenario.ExpLowerLeg, scenario.ExpFoot:
			l0 := 0.0
			if e.Kind == scenario.ExpLowerLeg {
				addCap(ei, a, a.Add(u.Scale(0.40*s)), 0.055*s, scenario.PartLeg, matCloth, trousers, coupleCloth)
				l0 = 0.36 * s
				ax.L, ax.Radius = 0.62*s, 0.055*s
			} else {
				ax.L, ax.Radius = 0.27*s, 0.048*s
			}
			ank := a.Add(u.Scale(l0))
			addCap(ei, ank, ank.Add(u.Scale(0.08*s)), 0.04*s, scenario.PartLeg, matSkin, skin, coupleSkin)
			fs, fe := ank.Add(u.Scale(0.06*s)), ank.Add(u.Scale(0.27*s))
			if bareFoot {
				addCap(ei, fs, fe, 0.045*s, scenario.PartLeg, matSkin, skin, coupleSkin)
			} else {
				addCap(ei, fs, fe, 0.05*s, scenario.PartLeg, matShoe, shoe, coupleShoe)
			}
		case scenario.ExpHead:
			headAt(ei, a.Add(u.Scale(0.02*s)), u)
			ax.P0, ax.L, ax.Radius = a.Sub(u.Scale(0.09*s)), 0.22*s, 0.09*s
			ax.Swing = v.Movement.SwingDeg / 3
		case scenario.ExpHeadShoulders:
			addEll(ei, a.Sub(u.Scale(0.10*s)), u, 0.14*s, 0.24*s, scenario.PartTorso, matCloth, top, coupleCloth, 0.12*s)
			addCap(ei, a.Add(u.Scale(0.02*s)), a.Add(u.Scale(0.08*s)), 0.05*s, scenario.PartHead, matSkin, skin, coupleSkin)
			headAt(ei, a.Add(u.Scale(0.17*s)), u)
			ax.P0, ax.L, ax.Radius = a.Sub(u.Scale(0.24*s)), 0.52*s, 0.24*s
			ax.Swing = v.Movement.SwingDeg / 3
		case scenario.ExpBack:
			addEll(ei, a, u, 0.24*s, 0.19*s, scenario.PartTorso, matCloth, top, coupleCloth, 0.13*s)
			ax.P0, ax.L, ax.Radius = a.Sub(u.Scale(0.24*s)), 0.48*s, 0.19*s
			ax.Swing = v.Movement.SwingDeg / 6
		}
		g.axes = append(g.axes, ax)
	}

	// Pose B: every exposure swings about its anchor.
	if v.Movement.Moves {
		g.primsB = make([]prim, len(g.primsA))
		for i, p := range g.primsA {
			ax := g.axes[p.exp]
			q := p
			switch sh := p.sh.(type) {
			case capsule:
				q.sh = capsule{rotAbout(sh.a, ax.Anchor, ax.Swing), rotAbout(sh.b, ax.Anchor, ax.Swing), sh.r}
			case ellipse:
				q.sh = ellipse{rotAbout(sh.c, ax.Anchor, ax.Swing), sh.u.Rot(-ax.Swing * math.Pi / 180), sh.ax, sh.ay}
			}
			g.primsB[i] = q
		}
	}

	// Occluding debris: hides the proximal part of each exposure so it
	// emerges from the rubble, plus a few pebbles on top.
	occMats := []uint8{matConcrete, matConcreteDark, matBrick, matDust, matPlaster}
	for _, ax := range g.axes {
		f := v.OcclusionFrac
		l := math.Max(0.05*s, f*ax.L)
		c := ax.P0.Add(ax.U.Scale(l/2 - 0.02*s))
		m := occMats[rng.Intn(len(occMats))]
		heading := math.Atan2(ax.U.X, ax.U.Y) * 180 / math.Pi
		poly := roughPoly(rng.Float64, c, 2.8*ax.Radius+0.04, l+0.06, heading, 7)
		g.occl = append(g.occl, occluder{sh: newPolygon(poly), mat: m, col: jitterColour(rng, materials[m])})
		for k := rng.Intn(3); k > 0; k-- {
			pc := ax.P0.Add(ax.U.Scale(ax.L * (0.3 + 0.7*rng.Float64()))).Add(rightOf(ax.U).Scale((rng.Float64() - 0.5) * 2 * ax.Radius))
			r := 0.012 + 0.02*rng.Float64()
			m := occMats[rng.Intn(len(occMats))]
			g.occl = append(g.occl, occluder{sh: newPolygon(roughPoly(rng.Float64, pc, 2*r, 2*r, rng.Float64()*180, 5)), mat: m, col: jitterColour(rng, materials[m])})
		}
	}
	return g
}

func jitterColour(rng *rand.Rand, m material) [3]float64 {
	k := 1 + m.vary*(2*rng.Float64()-1)
	return [3]float64{m.rgb[0] * k, m.rgb[1] * k, m.rgb[2] * k}
}

// primsBounds returns the box around a set of primitives.
func primsBounds(ps []prim) geom.Box {
	b := geom.Box{Min: geom.P{X: math.Inf(1), Y: math.Inf(1)}, Max: geom.P{X: math.Inf(-1), Y: math.Inf(-1)}}
	for _, p := range ps {
		pb := p.sh.bounds()
		b.Min.X, b.Min.Y = math.Min(b.Min.X, pb.Min.X), math.Min(b.Min.Y, pb.Min.Y)
		b.Max.X, b.Max.Y = math.Max(b.Max.X, pb.Max.X), math.Max(b.Max.Y, pb.Max.Y)
	}
	return b
}
