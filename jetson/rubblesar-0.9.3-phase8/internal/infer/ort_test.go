//go:build ort

package infer

import (
	"math"
	"path/filepath"
	"testing"

	"rubblesar/internal/onnxgen"
	"rubblesar/internal/raster"
)

// The stub model returns fixed predictions and prototypes shifted by the mean
// of the input: this checks names, dynamic batch, shapes and data transfer.
func TestORTRunsStubModel(t *testing.T) {
	const size, nc, nm = 64, 5, 32
	A := 20
	C := 4 + nc + nm
	mh, mw := size/4, size/4
	spec := onnxgen.Spec{C: C, A: A, NM: nm, MH: mh, MW: mw, Preds: make([]float32, C*A), Protos: make([]float32, nm*mh*mw)}
	// One candidate: class 2, box 20..40 x 10..30, mask coefficient 0 = 1.
	spec.Preds[0*A+3], spec.Preds[1*A+3], spec.Preds[2*A+3], spec.Preds[3*A+3] = 30, 20, 20, 20
	spec.Preds[(4+2)*A+3] = 0.8
	spec.Preds[(4+nc)*A+3] = 1
	for i := range spec.Protos[:mh*mw] {
		spec.Protos[i] = -0.25 // plus the image mean (0.5 below) = +0.25: mask on
	}
	path := filepath.Join(t.TempDir(), "stub.onnx")
	if err := onnxgen.Write(path, spec); err != nil {
		t.Fatal(err)
	}
	o, err := OpenORT(path, size, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	t.Logf("backend %s", o.Name())
	in := make([]float32, 3*3*size*size)
	for i := range in {
		in[i] = 0.5
	}
	out, err := o.Infer(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.B != 3 || out.C != C || out.A != A || out.NM != nm || out.MH != mh || out.MW != mw {
		t.Fatalf("shapes %+v", out)
	}
	if err := out.Check(nc); err != nil {
		t.Fatal(err)
	}
	if v := out.Protos[2*nm*mh*mw]; math.Abs(float64(v)-0.25) > 1e-6 {
		t.Fatalf("image 2 proto = %v, want 0.25 (input mean not applied?)", v)
	}
	ds := Decode(out, 1, DefaultParams(), size)
	if len(ds) != 1 || ds[0].Class != 2 || ds[0].Box != [4]float32{20, 10, 40, 30} || ds[0].Area != 400 {
		t.Fatalf("decoded %+v", ds)
	}
	// Whole-frame path with a small frame (one tile).
	m := raster.NewRGB(size, size)
	for i := range m.Pix {
		m.Pix[i] = 128
	}
	det := &Detector{B: o, P: DefaultParams()}
	fd, tm, err := det.DetectFrame(m)
	if err != nil || len(fd) != 1 || tm.Tiles != 1 {
		t.Fatalf("frame: %v %+v %+v", err, fd, tm)
	}
	if _, err := OpenORT(filepath.Join(t.TempDir(), "missing.onnx"), size, 1); err == nil {
		t.Fatal("missing model opened")
	}
}
