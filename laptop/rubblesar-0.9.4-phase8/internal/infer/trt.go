//go:build trt

package infer

/*
#cgo CXXFLAGS: -std=c++17 -O2 -Wno-deprecated-declarations -I/usr/local/cuda/include
#cgo LDFLAGS: -L/usr/lib/aarch64-linux-gnu -L/usr/local/cuda/lib64 -lnvinfer -lcudart -lstdc++ -Wl,-rpath,/usr/local/cuda/lib64
#include <stdlib.h>
#include "trt_shim.h"
*/
import "C"

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"unsafe"

	"rubblesar/internal/diag"
)

// TRTAvailable reports whether this build includes TensorRT.
const TRTAvailable = true

// TRT runs a TensorRT engine on the GPU.
type TRT struct {
	mu   sync.Mutex
	e    *C.trts_engine
	info C.trts_info
	path string
}

// OpenTRT deserialises an engine built by trtexec (scripts/jetson/80-engine.sh).
func OpenTRT(path string) (*TRT, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, diag.Wrap("E-INF-001", err, "engine file")
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var errbuf [2048]C.char
	e := C.trts_open(cp, &errbuf[0], C.size_t(len(errbuf)))
	if e == nil {
		msg := C.GoString(&errbuf[0])
		if strings.Contains(msg, "CUDA is not usable") {
			return nil, diag.New("E-INF-006", "%s; %s", msg, whoAmI())
		}
		return nil, diag.New("E-INF-005", "%s: %s", path, msg)
	}
	t := &TRT{e: e, path: path}
	C.trts_info_get(e, &t.info)
	if t.info.height != t.info.width {
		t.Close()
		return nil, diag.New("E-INF-003", "engine input %dx%d is not square", int(t.info.width), int(t.info.height))
	}
	return t, nil
}

// Name implements Backend.
func (t *TRT) Name() string {
	v := int(t.info.trt_version)
	return fmt.Sprintf("tensorrt %d.%d.%d on %s", v/10000, v/100%100, v%100, C.GoString(&t.info.device[0]))
}

// InputSize implements Backend.
func (t *TRT) InputSize() int { return int(t.info.width) }

// MaxBatch implements Backend.
func (t *TRT) MaxBatch() int { return int(t.info.max_batch) }

// Infer implements Backend.
func (t *TRT) Infer(in []float32, batch int) (*RawOutput, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.e == nil {
		return nil, diag.New("E-INF-004", "engine is closed")
	}
	s := t.InputSize()
	if len(in) != batch*3*s*s {
		return nil, diag.New("E-INF-004", "input has %d values, expected %d", len(in), batch*3*s*s)
	}
	var out C.trts_outputs
	var errbuf [2048]C.char
	if rc := C.trts_run(t.e, (*C.float)(unsafe.Pointer(&in[0])), C.int(batch), &out, &errbuf[0], C.size_t(len(errbuf))); rc != 0 {
		return nil, diag.New("E-INF-004", "tensorrt: %s", C.GoString(&errbuf[0]))
	}
	defer C.trts_free_outputs(&out)
	n := int(t.info.n_out)
	if n < 2 {
		return nil, diag.New("E-INF-003", "engine has %d output(s); a segmentation model has 2", n)
	}
	// Outputs are matched by rank: [B,C,A] predictions and [B,NM,H,W] prototypes.
	var pi, qi = -1, -1
	for i := 0; i < n; i++ {
		switch int(out.rank[i]) {
		case 3:
			pi = i
		case 4:
			qi = i
		}
	}
	if pi < 0 || qi < 0 {
		return nil, diag.New("E-INF-003", "engine outputs are not [B,C,A] and [B,NM,H,W]")
	}
	r := &RawOutput{B: int(out.shape[pi][0]), C: int(out.shape[pi][1]), A: int(out.shape[pi][2]),
		NM: int(out.shape[qi][1]), MH: int(out.shape[qi][2]), MW: int(out.shape[qi][3])}
	n0, n1 := r.B*r.C*r.A, r.B*r.NM*r.MH*r.MW
	r.Preds = make([]float32, n0)
	r.Protos = make([]float32, n1)
	copy(r.Preds, unsafe.Slice((*float32)(unsafe.Pointer(out.data[pi])), n0))
	copy(r.Protos, unsafe.Slice((*float32)(unsafe.Pointer(out.data[qi])), n1))
	return r, nil
}

// Close implements Backend.
func (t *TRT) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.e != nil {
		C.trts_close(t.e)
		t.e = nil
	}
	return nil
}

// whoAmI names the account and its groups, for GPU permission errors.
func whoAmI() string {
	u, err := user.Current()
	if err != nil {
		return fmt.Sprintf("running as uid %d", os.Getuid())
	}
	var names []string
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if g, err := user.LookupGroupId(id); err == nil {
				names = append(names, g.Name)
			} else {
				names = append(names, id)
			}
		}
	}
	return fmt.Sprintf("running as user %s (groups: %s)", u.Username, strings.Join(names, ", "))
}
