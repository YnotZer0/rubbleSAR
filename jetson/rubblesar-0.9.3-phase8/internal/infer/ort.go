//go:build ort

package infer

/*
#cgo pkg-config: rubblesar-onnxruntime
#include <stdlib.h>
#include "ort_shim.h"
*/
import "C"

import (
	"os"
	"runtime"
	"sync"
	"unsafe"

	"rubblesar/internal/diag"
)

// ORTAvailable reports whether this build includes ONNX Runtime.
const ORTAvailable = true

// ORTVersion returns the linked ONNX Runtime version.
func ORTVersion() string { return C.GoString(C.orts_version()) }

// ORT runs an ONNX model on the CPU with ONNX Runtime.
type ORT struct {
	mu    sync.Mutex
	s     *C.orts_session
	size  int
	batch int
	path  string
}

// OpenORT loads an ONNX model. size is the square input size the model was
// exported for; maxBatch limits how many tiles go into one call.
func OpenORT(path string, size, maxBatch int) (*ORT, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, diag.Wrap("E-INF-001", err, "model file")
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var errbuf [512]C.char
	s := C.orts_open(cp, C.int(runtime.NumCPU()), &errbuf[0], C.size_t(len(errbuf)))
	if s == nil {
		return nil, diag.New("E-INF-001", "%s: %s", path, C.GoString(&errbuf[0]))
	}
	if maxBatch < 1 {
		maxBatch = 1
	}
	return &ORT{s: s, size: size, batch: maxBatch, path: path}, nil
}

// Name implements Backend.
func (o *ORT) Name() string { return "onnxruntime-cpu " + ORTVersion() }

// InputSize implements Backend.
func (o *ORT) InputSize() int { return o.size }

// MaxBatch implements Backend.
func (o *ORT) MaxBatch() int { return o.batch }

// Infer implements Backend.
func (o *ORT) Infer(in []float32, batch int) (*RawOutput, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.s == nil {
		return nil, diag.New("E-INF-004", "onnxruntime session is closed")
	}
	if len(in) != batch*3*o.size*o.size {
		return nil, diag.New("E-INF-004", "input has %d values, expected %d", len(in), batch*3*o.size*o.size)
	}
	shape := [4]C.int64_t{C.int64_t(batch), 3, C.int64_t(o.size), C.int64_t(o.size)}
	var out C.orts_outputs
	var errbuf [512]C.char
	// The input slice is pinned for the duration of the call (cgo rules allow
	// passing a Go pointer to memory without Go pointers).
	rc := C.orts_run(o.s, (*C.float)(unsafe.Pointer(&in[0])), &shape[0], 4, &out, &errbuf[0], C.size_t(len(errbuf)))
	if rc != 0 {
		return nil, diag.New("E-INF-004", "onnxruntime: %s", C.GoString(&errbuf[0]))
	}
	defer C.orts_free_outputs(&out)
	return rawFromC(&out)
}

func rawFromC(out *C.orts_outputs) (*RawOutput, error) {
	if out.n < 2 {
		return nil, diag.New("E-INF-003", "model has %d output(s); a segmentation model has 2 (output0, output1)", int(out.n))
	}
	shape := func(i int) []int {
		s := make([]int, int(out.rank[i]))
		for k := range s {
			s[k] = int(out.shape[i][k])
		}
		return s
	}
	s0, s1 := shape(0), shape(1)
	if len(s0) != 3 || len(s1) != 4 {
		return nil, diag.New("E-INF-003", "output shapes %v and %v; expected [B,C,A] and [B,32,H,W]", s0, s1)
	}
	r := &RawOutput{B: s0[0], C: s0[1], A: s0[2], NM: s1[1], MH: s1[2], MW: s1[3]}
	n0 := r.B * r.C * r.A
	n1 := s1[0] * r.NM * r.MH * r.MW
	r.Preds = make([]float32, n0)
	r.Protos = make([]float32, n1)
	copy(r.Preds, unsafe.Slice((*float32)(unsafe.Pointer(out.data[0])), n0))
	copy(r.Protos, unsafe.Slice((*float32)(unsafe.Pointer(out.data[1])), n1))
	return r, nil
}

// Close implements Backend.
func (o *ORT) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.s != nil {
		C.orts_close(o.s)
		o.s = nil
	}
	return nil
}
