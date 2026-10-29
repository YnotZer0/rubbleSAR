//go:build !ort

package infer

import "rubblesar/internal/diag"

// ORTAvailable reports whether this build includes ONNX Runtime.
const ORTAvailable = false

// ORTVersion returns the linked ONNX Runtime version.
func ORTVersion() string { return "none" }

// ORT is unavailable in this build.
type ORT struct{ Backend }

// OpenORT fails: the binary was built without ONNX Runtime.
func OpenORT(path string, size, maxBatch int) (*ORT, error) {
	return nil, diag.New("E-INF-002", "this binary was built without ONNX Runtime (install it with scripts/<machine>/12-onnxruntime.sh, then rebuild)")
}
