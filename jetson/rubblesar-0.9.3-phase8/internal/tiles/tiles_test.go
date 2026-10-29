package tiles

import "testing"

func TestPlanCoversFrame(t *testing.T) {
	for _, c := range []struct{ w, h, size, want int }{
		{1920, 1080, 640, 6}, {640, 512, 640, 1}, {1280, 720, 640, 4}, {2000, 700, 640, 8}, {300, 200, 640, 1},
	} {
		p := Plan(c.w, c.h, c.size)
		if len(p) != c.want {
			t.Errorf("%dx%d: %d tiles, want %d", c.w, c.h, len(p), c.want)
		}
		cover := make([]bool, c.w*c.h)
		for _, tl := range p {
			if tl.X < 0 || tl.Y < 0 || (c.w >= c.size && tl.X+c.size > c.w) || (c.h >= c.size && tl.Y+c.size > c.h) {
				t.Errorf("%dx%d: tile %+v outside the frame", c.w, c.h, tl)
			}
			for y := tl.Y; y < tl.Y+c.size && y < c.h; y++ {
				for x := tl.X; x < tl.X+c.size && x < c.w; x++ {
					cover[y*c.w+x] = true
				}
			}
		}
		for i, v := range cover {
			if !v {
				t.Fatalf("%dx%d: pixel %d not covered", c.w, c.h, i)
			}
		}
	}
	p := Plan(1920, 1080, 640)
	if p[0] != (Tile{0, 0, 640}) || p[2].X != 1280 || p[3].Y != 440 {
		t.Fatalf("1080p plan %+v", p)
	}
	if Describe(1920, 1080, 640) != "6 tiles of 640 px (3 x 2) for 1920x1080" {
		t.Fatal(Describe(1920, 1080, 640))
	}
}
