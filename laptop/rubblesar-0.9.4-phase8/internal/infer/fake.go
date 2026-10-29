package infer

import "rubblesar/internal/diag"

// Fake is a test backend that "detects" pure-coloured blobs: every connected
// run of pixels whose colour matches one of Colours (class = index) becomes a
// candidate with a box around it and a mask prototype marking it. It lets the
// whole frame pipeline (tiling, decoding, masks, merging) be tested without a
// neural network.
type Fake struct {
	Size    int
	Batch   int
	Colours [][3]float32 // per class, 0..1
	Calls   int
}

// Name implements Backend.
func (f *Fake) Name() string { return "fake" }

// InputSize implements Backend.
func (f *Fake) InputSize() int { return f.Size }

// MaxBatch implements Backend.
func (f *Fake) MaxBatch() int { return f.Batch }

// Close implements Backend.
func (f *Fake) Close() error { return nil }

// Infer implements Backend. One anchor per class per image: the box spans
// all pixels of that colour; the mask prototype 0 is +4 on those pixels and
// -4 elsewhere (at 1/4 resolution, averaged).
func (f *Fake) Infer(in []float32, batch int) (*RawOutput, error) {
	f.Calls++
	s := f.Size
	nc := len(f.Colours)
	if len(in) != batch*3*s*s {
		return nil, diag.New("E-INF-004", "fake: input has %d values for batch %d", len(in), batch)
	}
	const nm = 4
	A := nc
	C := 4 + nc + nm
	mh, mw := s/4, s/4
	o := &RawOutput{B: batch, C: C, A: A, NM: nm, MH: mh, MW: mw, Preds: make([]float32, batch*C*A), Protos: make([]float32, batch*nm*mh*mw)}
	plane := s * s
	for b := 0; b < batch; b++ {
		img := in[b*3*plane : (b+1)*3*plane]
		cnt := make([]float32, mh*mw*nc)
		for c, col := range f.Colours {
			x0, y0, x1, y1 := s, s, -1, -1
			for y := 0; y < s; y++ {
				for x := 0; x < s; x++ {
					i := y*s + x
					if img[i] == col[0] && img[plane+i] == col[1] && img[2*plane+i] == col[2] {
						x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x), max(y1, y)
						cnt[((y/4)*mw+x/4)*nc+c]++
					}
				}
			}
			if x1 < 0 {
				continue
			}
			at := func(row int) int { return b*C*A + row*A + c }
			o.Preds[at(0)] = float32(x0+x1+1) / 2
			o.Preds[at(1)] = float32(y0+y1+1) / 2
			o.Preds[at(2)] = float32(x1 - x0 + 1)
			o.Preds[at(3)] = float32(y1 - y0 + 1)
			o.Preds[at(4+c)] = 0.9
			o.Preds[at(4+nc+c%nm)] = 1 // uses prototype c%nm
		}
		for c := 0; c < nc && c < nm; c++ {
			p := o.Protos[(b*nm+c)*mh*mw : (b*nm+c+1)*mh*mw]
			for i := range p {
				p[i] = -4
				if cnt[i*nc+c] >= 8 { // at least half of the 4x4 cell
					p[i] = 4
				}
			}
		}
	}
	return o, nil
}
