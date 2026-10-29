package raster

import (
	"math"
	"path/filepath"
	"testing"
)

func TestNoiseDeterministicAndBounded(t *testing.T) {
	var lo, hi float64 = 1, -1
	for i := 0; i < 20000; i++ {
		x, y := float64(i)*0.137, float64(i%97)*0.311
		v := FBM(42, x, y, 4)
		if v != FBM(42, x, y, 4) {
			t.Fatal("not deterministic")
		}
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if lo < -1 || hi > 1 || hi-lo < 0.8 {
		t.Fatalf("FBM range %.2f..%.2f", lo, hi)
	}
	if FBM(1, 3.3, 4.4, 3) == FBM(2, 3.3, 4.4, 3) {
		t.Fatal("seed ignored")
	}
	// Continuity: tiny steps give tiny changes.
	if d := math.Abs(Value(7, 10.5, 3.25) - Value(7, 10.5001, 3.25)); d > 0.001 {
		t.Fatalf("value noise jumps by %v", d)
	}
}

func TestBlursPreserveMeanAndConstant(t *testing.T) {
	b := NewF32(40, 30)
	for i := range b.Pix {
		b.Pix[i] = 5
	}
	b.Pix[15*40+20] = 905
	for name, o := range map[string]*F32{"box": b.BoxBlur(3), "gauss": b.GaussBlur(1.2)} {
		if o.Pix[0] < 4.999 || o.Pix[0] > 5.001 {
			t.Errorf("%s changed a constant region: %v", name, o.Pix[0])
		}
		var s float64
		for _, v := range o.Pix {
			s += float64(v)
		}
		if math.Abs(s-(5*1200+900)) > 1 {
			t.Errorf("%s sum %v", name, s)
		}
		if o.Pix[15*40+20] >= 905 || o.Pix[15*40+20] <= 5 {
			t.Errorf("%s did not spread the peak", name)
		}
	}
}

func TestPNGRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rgb := NewRGB(17, 9)
	u8 := NewU8(17, 9)
	u16 := NewU16(17, 9)
	for i := 0; i < 17*9; i++ {
		rgb.Pix[3*i], rgb.Pix[3*i+1], rgb.Pix[3*i+2] = uint8(i), uint8(i*7), uint8(255-i)
		u8.Pix[i] = uint8(i * 3)
		u16.Pix[i] = uint16(i * 401)
	}
	if err := SavePNG(filepath.Join(dir, "a.png"), rgb.Image()); err != nil {
		t.Fatal(err)
	}
	SavePNG(filepath.Join(dir, "b.png"), u8.Image())
	SavePNG(filepath.Join(dir, "c.png"), u16.Image())
	r2, err := LoadRGB(filepath.Join(dir, "a.png"))
	if err != nil || string(r2.Pix) != string(rgb.Pix) {
		t.Fatalf("rgb round trip %v", err)
	}
	u2, err := LoadU8(filepath.Join(dir, "b.png"))
	if err != nil || string(u2.Pix) != string(u8.Pix) {
		t.Fatalf("u8 round trip %v", err)
	}
	w2, err := LoadU16(filepath.Join(dir, "c.png"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range w2.Pix {
		if w2.Pix[i] != u16.Pix[i] {
			t.Fatalf("u16 round trip at %d", i)
		}
	}
	if _, err := LoadU16(filepath.Join(dir, "b.png")); err == nil {
		t.Fatal("8-bit file accepted as 16-bit")
	}
	h1, _ := FileSHA256(filepath.Join(dir, "a.png"))
	if len(h1) != 64 {
		t.Fatal("sha")
	}
	c := u8.Crop(Rect{2, 3, 5, 7})
	if c.W != 3 || c.H != 4 || c.Pix[0] != u8.Pix[3*17+2] {
		t.Fatal("crop")
	}
}

func TestRowsCoversAll(t *testing.T) {
	seen := make([]int32, 1001)
	Rows(1001, func(i int) { seen[i]++ })
	for i, v := range seen {
		if v != 1 {
			t.Fatalf("row %d visited %d times", i, v)
		}
	}
}
