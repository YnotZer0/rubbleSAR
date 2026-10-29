package web

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServer(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("map/antakya-demo/13/5000/3000.png", "png13")
	write("map/antakya-demo/19/320000/200000.png", "png19")
	write("map/antakya-demo/19/320001/200000.png", "png19b")
	write("map/antakya-demo/antakya-demo.mbtiles", "SQLite format 3\x00")
	write("map/antakya-demo/overview.png", "ov")
	write("reports/antakya-demo/20261018T160558Z/index.html", "<html>report</html>")
	write("reports/antakya-demo/20261018T160558Z/summary.json", `{"title":"Run 10z","summary":"28 casualties, 20 rescued","started":"2026-10-18T16:05:58Z"}`)
	write("reports/antakya-demo/20261017T090000Z/index.html", "<html>older</html>")
	s := &Server{Root: root, Version: "sar-web test"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, "rubblesar-map.xml") || !strings.Contains(body, "3 tiles, zoom 13 to 19") ||
		!strings.Contains(body, `href="/reports/antakya-demo/20261018T160558Z/">Run 10z</a>`) || !strings.Contains(body, "28 casualties, 20 rescued") {
		t.Fatalf("index %d:\n%s", resp.StatusCode, body)
	}
	if strings.Index(body, "Run 10z") > strings.Index(body, "20261017T090000Z") {
		t.Fatal("reports not newest first")
	}
	_, x := get("/map/antakya-demo/rubblesar-map.xml")
	host := strings.TrimPrefix(srv.URL, "http://")
	// The layout WinTAK accepts: the root element right after the declaration,
	// the URL as plain text.
	if !strings.HasPrefix(x, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<customMapSource>\n") ||
		!strings.Contains(x, "<url>http://"+host+"/map/antakya-demo/{$z}/{$x}/{$y}.png</url>") || !strings.Contains(x, "<minZoom>0</minZoom>") ||
		!strings.Contains(x, "<maxZoom>19</maxZoom>") || strings.Contains(x, "<!--") || strings.Contains(x, "CDATA") {
		t.Fatalf("map source:\n%s", x)
	}
	if err := xml.Unmarshal([]byte(x), new(struct {
		XMLName xml.Name `xml:"customMapSource"`
		URL     string   `xml:"url"`
	})); err != nil {
		t.Fatalf("map source XML: %v", err)
	}
	if r, _ := get("/map/nowhere/rubblesar-map.xml"); r.StatusCode != 404 {
		t.Fatalf("unknown map: %d", r.StatusCode)
	}
	r, tile := get("/map/antakya-demo/19/320000/200000.png")
	if tile != "png19" || r.Header.Get("Cache-Control") == "" {
		t.Fatalf("tile %q %v", tile, r.Header)
	}
	// Tiles not drawn (WinTAK's test tile 0/0/0, the area around, low zooms)
	// get a plain tile; tiles of no map, out of range or malformed do not.
	for _, p := range []string{"/map/antakya-demo/0/0/0.png", "/map/antakya-demo/13/0/0.png", "/map/antakya-demo/19/320002/200000.png"} {
		r, b := get(p)
		im, err := png.Decode(bytes.NewReader([]byte(b)))
		if r.StatusCode != 200 || err != nil || im.Bounds().Dx() != 256 || r.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %v", p, r.StatusCode, err)
		}
		if c := im.At(10, 10); func() bool { r, g, b, _ := c.RGBA(); return r>>8 != 233 || g>>8 != 230 || b>>8 != 224 }() {
			t.Fatalf("%s: colour %v", p, c)
		}
	}
	for _, p := range []string{"/map/nowhere/0/0/0.png", "/map/antakya-demo/1/2/0.png", "/map/antakya-demo/23/0/0.png", "/map/antakya-demo/x/0/0.png", "/map/antakya-demo/13/0.png"} {
		if r, _ := get(p); r.StatusCode != 404 {
			t.Fatalf("%s: %d, want 404", p, r.StatusCode)
		}
	}
	if r, _ := get("/../../etc/passwd"); r.StatusCode == 200 {
		t.Fatal("path escape")
	}
	_, st := get("/v1/status")
	var v struct {
		Maps    []Map `json:"maps"`
		Reports int   `json:"reports"`
	}
	if json.Unmarshal([]byte(st), &v) != nil || len(v.Maps) != 1 || v.Maps[0].Tiles != 3 || v.Reports != 2 || v.Maps[0].MBTiles != "antakya-demo.mbtiles" {
		t.Fatalf("status %s", st)
	}
}

// The operator view and the presenter page appear in the demo video, which
// names the command post's computer generically, never by its make.
func TestOpsPagesDoNotNameTheLaptop(t *testing.T) {
	for _, f := range []string{"ops/index.html", "ops/app.js", "ops/app.css"} {
		b, err := opsFS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// (the panel's internal id "zbook" is not shown)
		for _, w := range []string{"ZBook", "Zbook", "ZBOOK", "HP "} {
			if bytes.Contains(b, []byte(w)) {
				t.Errorf("%s names the laptop (%q)", f, w)
			}
		}
	}
}
