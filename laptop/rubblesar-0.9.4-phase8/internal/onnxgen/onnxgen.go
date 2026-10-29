// Package onnxgen writes a tiny ONNX model with the same inputs and outputs
// as a YOLO11-seg export (images -> output0, output1). Its outputs are fixed
// tensors plus the mean of the input image, so tests and installation checks
// can verify an inference runtime end to end without the real model.
package onnxgen

import (
	"encoding/binary"
	"math"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

// Spec sets the output sizes.
type Spec struct {
	C, A       int       // output0 is [batch, C, A]
	NM, MH, MW int       // output1 is [batch, NM, MH, MW]
	Preds      []float32 // C*A values (one image)
	Protos     []float32 // NM*MH*MW values (one image); the input mean is added
}

type msg []byte

func (m msg) str(num protowire.Number, s string) msg {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendString(m, s)
}
func (m msg) bytes(num protowire.Number, b []byte) msg {
	m = protowire.AppendTag(m, num, protowire.BytesType)
	return protowire.AppendBytes(m, b)
}
func (m msg) varint(num protowire.Number, v uint64) msg {
	m = protowire.AppendTag(m, num, protowire.VarintType)
	return protowire.AppendVarint(m, v)
}

func tensorF32(name string, dims []int, vals []float32) msg {
	var t msg
	for _, d := range dims {
		t = t.varint(1, uint64(d))
	}
	t = t.varint(2, 1) // FLOAT
	t = t.str(8, name)
	raw := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(v))
	}
	return t.bytes(9, raw)
}

func tensorI64(name string, vals []int64) msg {
	var t msg
	t = t.varint(1, uint64(len(vals)))
	t = t.varint(2, 7) // INT64
	t = t.str(8, name)
	raw := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(raw[8*i:], uint64(v))
	}
	return t.bytes(9, raw)
}

// dims: positive = fixed size, negative = symbolic (named by the map).
func valueInfo(name string, dims []int, params map[int]string) msg {
	var shape msg
	for i, d := range dims {
		var dim msg
		if p, ok := params[i]; ok {
			dim = dim.str(2, p)
		} else {
			dim = dim.varint(1, uint64(d))
		}
		shape = shape.bytes(1, dim)
	}
	var tt msg
	tt = tt.varint(1, 1) // FLOAT
	tt = tt.bytes(2, shape)
	var tp msg
	tp = tp.bytes(1, tt)
	var vi msg
	vi = vi.str(1, name)
	return vi.bytes(2, tp)
}

func node(op string, in, out []string, attrs ...msg) msg {
	var n msg
	for _, s := range in {
		n = n.str(1, s)
	}
	for _, s := range out {
		n = n.str(2, s)
	}
	n = n.str(3, op+"_"+out[0])
	n = n.str(4, op)
	for _, a := range attrs {
		n = n.bytes(5, a)
	}
	return n
}

func attrInt(name string, v int64) msg {
	var a msg
	a = a.str(1, name)
	a = a.varint(3, uint64(v))
	return a.varint(20, 2)
}

func attrInts(name string, vs []int64) msg {
	var a msg
	a = a.str(1, name)
	for _, v := range vs {
		a = a.varint(8, uint64(v))
	}
	return a.varint(20, 7)
}

// Build returns the serialised model (opset 17, IR version 8).
func Build(s Spec) []byte {
	var g msg
	nodes := []msg{
		node("Shape", []string{"images"}, []string{"shp"}),
		node("Slice", []string{"shp", "zero", "one"}, []string{"bdim"}),
		node("Concat", []string{"bdim", "tail0"}, []string{"s0"}, attrInt("axis", 0)),
		node("Concat", []string{"bdim", "tail1"}, []string{"s1"}, attrInt("axis", 0)),
		node("Expand", []string{"p0", "s0"}, []string{"e0"}),
		node("Identity", []string{"e0"}, []string{"output0"}),
		node("ReduceMean", []string{"images"}, []string{"mean"}, attrInts("axes", []int64{1, 2, 3}), attrInt("keepdims", 1)),
		node("Expand", []string{"p1", "s1"}, []string{"e1"}),
		node("Add", []string{"e1", "mean"}, []string{"output1"}),
	}
	for _, n := range nodes {
		g = g.bytes(1, n)
	}
	g = g.str(2, "rubblesar-yoloseg-stub")
	inits := []msg{
		tensorF32("p0", []int{1, s.C, s.A}, s.Preds),
		tensorF32("p1", []int{1, s.NM, s.MH, s.MW}, s.Protos),
		tensorI64("zero", []int64{0}),
		tensorI64("one", []int64{1}),
		tensorI64("tail0", []int64{int64(s.C), int64(s.A)}),
		tensorI64("tail1", []int64{int64(s.NM), int64(s.MH), int64(s.MW)}),
	}
	for _, t := range inits {
		g = g.bytes(5, t)
	}
	g = g.bytes(11, valueInfo("images", []int{-1, 3, -1, -1}, map[int]string{0: "batch", 2: "height", 3: "width"}))
	g = g.bytes(12, valueInfo("output0", []int{-1, s.C, s.A}, map[int]string{0: "batch"}))
	g = g.bytes(12, valueInfo("output1", []int{-1, s.NM, s.MH, s.MW}, map[int]string{0: "batch"}))

	var opset msg
	opset = opset.str(1, "")
	opset = opset.varint(2, 17)
	var m msg
	m = m.varint(1, 8) // IR version
	m = m.str(2, "rubblesar-onnxgen")
	m = m.bytes(7, g)
	m = m.bytes(8, opset)
	return m
}

// Write saves a model file.
func Write(path string, s Spec) error { return os.WriteFile(path, Build(s), 0o644) }
