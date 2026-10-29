// Package raster holds the small image toolkit used by the renderer and the
// virtual camera: deterministic noise, compact layer buffers, blurs and PNG
// input/output. Everything is pure Go so it builds identically on the ZBook
// (amd64) and the Jetson (arm64).
package raster

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Rect is a pixel rectangle [X0,X1) x [Y0,Y1).
type Rect struct{ X0, Y0, X1, Y1 int }

// W and H return the size.
func (r Rect) W() int { return r.X1 - r.X0 }
func (r Rect) H() int { return r.Y1 - r.Y0 }

// Empty reports whether the rectangle has no pixels.
func (r Rect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }

// Intersect returns the overlap.
func (r Rect) Intersect(o Rect) Rect {
	return Rect{max(r.X0, o.X0), max(r.Y0, o.Y0), min(r.X1, o.X1), min(r.Y1, o.Y1)}
}

// Contains reports whether pixel (x, y) is inside.
func (r Rect) Contains(x, y int) bool { return x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1 }

// Grow expands by n pixels on every side.
func (r Rect) Grow(n int) Rect { return Rect{r.X0 - n, r.Y0 - n, r.X1 + n, r.Y1 + n} }

// ---------------------------------------------------------------- noise

// Hash returns a well-mixed 64-bit hash of a seed and two lattice coordinates.
func Hash(seed uint64, x, y int64) uint64 {
	h := seed ^ uint64(x)*0x9E3779B97F4A7C15 ^ uint64(y)*0xC2B2AE3D27D4EB4F
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// Unit maps a hash to [0, 1).
func Unit(h uint64) float64 { return float64(h>>11) / (1 << 53) }

// Value is smooth value noise in [-1, 1] with unit lattice spacing.
func Value(seed uint64, x, y float64) float64 {
	x0, y0 := math.Floor(x), math.Floor(y)
	fx, fy := x-x0, y-y0
	sx, sy := fx*fx*(3-2*fx), fy*fy*(3-2*fy)
	ix, iy := int64(x0), int64(y0)
	a := Unit(Hash(seed, ix, iy))
	b := Unit(Hash(seed, ix+1, iy))
	c := Unit(Hash(seed, ix, iy+1))
	d := Unit(Hash(seed, ix+1, iy+1))
	top := a + (b-a)*sx
	bot := c + (d-c)*sx
	return 2*(top+(bot-top)*sy) - 1
}

// FBM sums octaves of value noise (roughly [-1, 1]).
func FBM(seed uint64, x, y float64, octaves int) float64 {
	sum, amp, norm := 0.0, 1.0, 0.0
	for i := 0; i < octaves; i++ {
		sum += amp * Value(seed+uint64(i)*0x5851F42D4C957F2D, x, y)
		norm += amp
		amp *= 0.5
		x, y = x*2.03, y*2.03
	}
	return sum / norm
}

// ---------------------------------------------------------------- buffers

// RGB is an 8-bit colour buffer, 3 bytes per pixel.
type RGB struct {
	W, H int
	Pix  []uint8
}

// NewRGB allocates a buffer.
func NewRGB(w, h int) *RGB { return &RGB{w, h, make([]uint8, w*h*3)} }

// Set writes a pixel.
func (b *RGB) Set(x, y int, c [3]uint8) {
	i := (y*b.W + x) * 3
	b.Pix[i], b.Pix[i+1], b.Pix[i+2] = c[0], c[1], c[2]
}

// At reads a pixel.
func (b *RGB) At(x, y int) [3]uint8 {
	i := (y*b.W + x) * 3
	return [3]uint8{b.Pix[i], b.Pix[i+1], b.Pix[i+2]}
}

// U8 is an 8-bit single-channel buffer.
type U8 struct {
	W, H int
	Pix  []uint8
}

// NewU8 allocates a buffer.
func NewU8(w, h int) *U8 { return &U8{w, h, make([]uint8, w*h)} }

// U16 is a 16-bit single-channel buffer.
type U16 struct {
	W, H int
	Pix  []uint16
}

// NewU16 allocates a buffer.
func NewU16(w, h int) *U16 { return &U16{w, h, make([]uint16, w*h)} }

// F32 is a float buffer.
type F32 struct {
	W, H int
	Pix  []float32
}

// NewF32 allocates a buffer.
func NewF32(w, h int) *F32 { return &F32{w, h, make([]float32, w*h)} }

// Crop helpers copy a sub-rectangle (r must lie inside the buffer).
func (b *RGB) Crop(r Rect) *RGB {
	o := NewRGB(r.W(), r.H())
	for y := 0; y < r.H(); y++ {
		copy(o.Pix[y*o.W*3:(y+1)*o.W*3], b.Pix[((r.Y0+y)*b.W+r.X0)*3:((r.Y0+y)*b.W+r.X1)*3])
	}
	return o
}

// Crop copies a sub-rectangle.
func (b *U8) Crop(r Rect) *U8 {
	o := NewU8(r.W(), r.H())
	for y := 0; y < r.H(); y++ {
		copy(o.Pix[y*o.W:(y+1)*o.W], b.Pix[(r.Y0+y)*b.W+r.X0:(r.Y0+y)*b.W+r.X1])
	}
	return o
}

// Crop copies a sub-rectangle.
func (b *U16) Crop(r Rect) *U16 {
	o := NewU16(r.W(), r.H())
	for y := 0; y < r.H(); y++ {
		copy(o.Pix[y*o.W:(y+1)*o.W], b.Pix[(r.Y0+y)*b.W+r.X0:(r.Y0+y)*b.W+r.X1])
	}
	return o
}

// Crop copies a sub-rectangle.
func (b *F32) Crop(r Rect) *F32 {
	o := NewF32(r.W(), r.H())
	for y := 0; y < r.H(); y++ {
		copy(o.Pix[y*o.W:(y+1)*o.W], b.Pix[(r.Y0+y)*b.W+r.X0:(r.Y0+y)*b.W+r.X1])
	}
	return o
}

// BoxBlur returns a separable box blur with the given radius (edges clamp).
func (b *F32) BoxBlur(radius int) *F32 {
	if radius <= 0 {
		o := NewF32(b.W, b.H)
		copy(o.Pix, b.Pix)
		return o
	}
	tmp := NewF32(b.W, b.H)
	out := NewF32(b.W, b.H)
	n := float32(2*radius + 1)
	Rows(b.H, func(y int) {
		row := b.Pix[y*b.W : (y+1)*b.W]
		var s float32
		for k := -radius; k <= radius; k++ {
			s += row[clampi(k, 0, b.W-1)]
		}
		for x := 0; x < b.W; x++ {
			tmp.Pix[y*b.W+x] = s / n
			s += row[clampi(x+radius+1, 0, b.W-1)] - row[clampi(x-radius, 0, b.W-1)]
		}
	})
	Rows(b.W, func(x int) {
		var s float32
		for k := -radius; k <= radius; k++ {
			s += tmp.Pix[clampi(k, 0, b.H-1)*b.W+x]
		}
		for y := 0; y < b.H; y++ {
			out.Pix[y*b.W+x] = s / n
			s += tmp.Pix[clampi(y+radius+1, 0, b.H-1)*b.W+x] - tmp.Pix[clampi(y-radius, 0, b.H-1)*b.W+x]
		}
	})
	return out
}

// GaussBlur blurs with a small Gaussian kernel (sigma in pixels).
func (b *F32) GaussBlur(sigma float64) *F32 {
	r := int(math.Ceil(sigma * 3))
	if sigma <= 0 || r < 1 {
		return b.Crop(Rect{0, 0, b.W, b.H})
	}
	k := make([]float32, 2*r+1)
	var sum float32
	for i := -r; i <= r; i++ {
		k[i+r] = float32(math.Exp(-float64(i*i) / (2 * sigma * sigma)))
		sum += k[i+r]
	}
	for i := range k {
		k[i] /= sum
	}
	tmp := NewF32(b.W, b.H)
	out := NewF32(b.W, b.H)
	Rows(b.H, func(y int) {
		for x := 0; x < b.W; x++ {
			var s float32
			for i := -r; i <= r; i++ {
				s += k[i+r] * b.Pix[y*b.W+clampi(x+i, 0, b.W-1)]
			}
			tmp.Pix[y*b.W+x] = s
		}
	})
	Rows(b.H, func(y int) {
		for x := 0; x < b.W; x++ {
			var s float32
			for i := -r; i <= r; i++ {
				s += k[i+r] * tmp.Pix[clampi(y+i, 0, b.H-1)*b.W+x]
			}
			out.Pix[y*b.W+x] = s
		}
	})
	return out
}

func clampi(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Rows runs fn for every row index in parallel across the CPU cores.
func Rows(n int, fn func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	if workers <= 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	next := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next += 8
				mu.Unlock()
				if i >= n {
					return
				}
				for j := i; j < i+8 && j < n; j++ {
					fn(j)
				}
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------- PNG I/O

// ToNRGBA converts for encoders and previews.
func (b *RGB) ToNRGBA() *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, b.W, b.H))
	for i, j := 0, 0; i < len(b.Pix); i, j = i+3, j+4 {
		m.Pix[j], m.Pix[j+1], m.Pix[j+2], m.Pix[j+3] = b.Pix[i], b.Pix[i+1], b.Pix[i+2], 255
	}
	return m
}

var encoder = png.Encoder{CompressionLevel: png.BestSpeed}

// SavePNG writes any image atomically (temp file + rename).
func SavePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := encoder.Encode(w, img); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Image views for saving.
func (b *RGB) Image() image.Image { return b.ToNRGBA() }

// Image returns an 8-bit grey image sharing the buffer.
func (b *U8) Image() image.Image {
	return &image.Gray{Pix: b.Pix, Stride: b.W, Rect: image.Rect(0, 0, b.W, b.H)}
}

// Image returns a 16-bit grey image (copy, big-endian as PNG wants).
func (b *U16) Image() image.Image {
	m := image.NewGray16(image.Rect(0, 0, b.W, b.H))
	for i, v := range b.Pix {
		m.Pix[2*i], m.Pix[2*i+1] = uint8(v>>8), uint8(v)
	}
	return m
}

func decode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return img, nil
}

// LoadRGB reads an 8-bit RGB(A) PNG.
func LoadRGB(path string) (*RGB, error) {
	img, err := decode(path)
	if err != nil {
		return nil, err
	}
	r := img.Bounds()
	b := NewRGB(r.Dx(), r.Dy())
	switch m := img.(type) {
	case *image.RGBA:
		for i, j := 0, 0; j < len(m.Pix); i, j = i+3, j+4 {
			b.Pix[i], b.Pix[i+1], b.Pix[i+2] = m.Pix[j], m.Pix[j+1], m.Pix[j+2]
		}
	case *image.NRGBA:
		for i, j := 0, 0; j < len(m.Pix); i, j = i+3, j+4 {
			b.Pix[i], b.Pix[i+1], b.Pix[i+2] = m.Pix[j], m.Pix[j+1], m.Pix[j+2]
		}
	default:
		for y := 0; y < b.H; y++ {
			for x := 0; x < b.W; x++ {
				c := color.NRGBAModel.Convert(img.At(r.Min.X+x, r.Min.Y+y)).(color.NRGBA)
				b.Set(x, y, [3]uint8{c.R, c.G, c.B})
			}
		}
	}
	return b, nil
}

// LoadU8 reads an 8-bit grey PNG.
func LoadU8(path string) (*U8, error) {
	img, err := decode(path)
	if err != nil {
		return nil, err
	}
	m, ok := img.(*image.Gray)
	if !ok {
		return nil, fmt.Errorf("%s: expected 8-bit grey PNG, got %T", path, img)
	}
	b := NewU8(m.Rect.Dx(), m.Rect.Dy())
	for y := 0; y < b.H; y++ {
		copy(b.Pix[y*b.W:(y+1)*b.W], m.Pix[y*m.Stride:y*m.Stride+b.W])
	}
	return b, nil
}

// LoadU16 reads a 16-bit grey PNG.
func LoadU16(path string) (*U16, error) {
	img, err := decode(path)
	if err != nil {
		return nil, err
	}
	m, ok := img.(*image.Gray16)
	if !ok {
		return nil, fmt.Errorf("%s: expected 16-bit grey PNG, got %T", path, img)
	}
	b := NewU16(m.Rect.Dx(), m.Rect.Dy())
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			i := y*m.Stride + 2*x
			b.Pix[y*b.W+x] = uint16(m.Pix[i])<<8 | uint16(m.Pix[i+1])
		}
	}
	return b, nil
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Clamp8 converts a float to a byte with rounding and clamping.
func Clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// BoxBlurExact blurs integer samples with exact integer running sums, so the
// result for a pixel does not depend on where the buffer starts (a window
// render then matches the full render bit for bit). Edges clamp.
func BoxBlurExact(pix []int32, w, h, radius int) []float32 {
	tmp := make([]int64, w*h)
	out := make([]float32, w*h)
	Rows(h, func(y int) {
		row := pix[y*w : (y+1)*w]
		var s int64
		for k := -radius; k <= radius; k++ {
			s += int64(row[clampi(k, 0, w-1)])
		}
		for x := 0; x < w; x++ {
			tmp[y*w+x] = s
			s += int64(row[clampi(x+radius+1, 0, w-1)]) - int64(row[clampi(x-radius, 0, w-1)])
		}
	})
	n := float64((2*radius + 1) * (2*radius + 1))
	Rows(w, func(x int) {
		var s int64
		for k := -radius; k <= radius; k++ {
			s += tmp[clampi(k, 0, h-1)*w+x]
		}
		for y := 0; y < h; y++ {
			out[y*w+x] = float32(float64(s) / n)
			s += tmp[clampi(y+radius+1, 0, h-1)*w+x] - tmp[clampi(y-radius, 0, h-1)*w+x]
		}
	})
	return out
}
