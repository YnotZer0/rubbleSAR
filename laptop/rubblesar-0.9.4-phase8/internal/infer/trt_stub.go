//go:build !trt

package infer

import "rubblesar/internal/diag"

// TRTAvailable reports whether this build includes TensorRT.
const TRTAvailable = false

// TRT is unavailable in this build.
type TRT struct{ Backend }

// OpenTRT fails: the binary was built without TensorRT (Jetson builds add it).
func OpenTRT(path string) (*TRT, error) {
	return nil, diag.New("E-INF-002", "this binary was built without TensorRT (on the Jetson, scripts/jetson/20-build.sh enables it when the TensorRT headers are installed)")
}
