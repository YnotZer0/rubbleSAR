package obs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The auth string from obs-websocket's protocol document example.
func TestAuthExample(t *testing.T) {
	// password "supersecretpassword", salt and challenge from the protocol docs.
	got := Auth("supersecretpassword", "lM1GncleQOaCu9lT1yeUZhFYnqhsLLP1G5lAGo3ixaI=", "+IxH4CnCiqpX1rM9scsNynZzbOe4KhDeYcTNS3PDaeY=")
	if got != "1Ct943GAT+6YQUUX47Ia/ncufilbe6+oD6lY+5kaCu4=" {
		t.Fatalf("auth %q", got)
	}
}

func TestClientAgainstFake(t *testing.T) {
	f := NewFake("s3cret")
	defer f.Close()
	ctx := context.Background()
	if _, err := Connect(f.URL, "wrong", 2*time.Second); err == nil || !strings.Contains(err.Error(), "refused the password") {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := Connect(f.URL, "", 2*time.Second); err == nil || !strings.Contains(err.Error(), "asks for a password") {
		t.Fatalf("no password: %v", err)
	}
	c, err := Connect(f.URL, "s3cret", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.OBSVersion != "30.2.3" {
		t.Errorf("version %q", c.OBSVersion)
	}
	if err := c.CreateScene(ctx, "RubbleSAR presenter"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateInput(ctx, "RubbleSAR presenter", "Presenter page", "browser_source", map[string]any{"url": "http://127.0.0.1:8090/ops/?present=1", "width": 1920, "height": 1080}); err != nil {
		t.Fatal(err)
	}
	scenes, cur, err := c.Scenes(ctx)
	if err != nil || len(scenes) != 2 || cur != "Scene" {
		t.Fatalf("scenes %v %q %v", scenes, cur, err)
	}
	if err := c.SetScene(ctx, "RubbleSAR presenter"); err != nil || f.Scene() != "RubbleSAR presenter" {
		t.Fatalf("set scene: %v (%s)", err, f.Scene())
	}
	var re *RequestError
	if err := c.SetScene(ctx, "nope"); !errors.As(err, &re) || re.Code != 600 {
		t.Fatalf("missing scene: %v", err)
	}
	if err := c.StartRecord(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	st, err := c.RecordStatus(ctx)
	if err != nil || !st.Active || st.Duration <= 0 {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := c.Chapter(ctx, "The incident"); err != nil {
		t.Fatal(err)
	}
	p, err := c.StopRecord(ctx)
	if err != nil || !strings.HasPrefix(p, "/fake/obs/") {
		t.Fatalf("stop %q %v", p, err)
	}
	if len(f.Chapters) != 1 || f.Chapters[0] != "The incident" {
		t.Errorf("chapters %v", f.Chapters)
	}
	kinds, err := c.InputKinds(ctx)
	if err != nil || len(kinds) == 0 {
		t.Errorf("kinds %v %v", kinds, err)
	}
	// A long message (over 64 KB) goes through in one piece.
	long := strings.Repeat("x", 70000)
	if err := c.Chapter(ctx, long); err == nil {
		t.Error("a chapter while not recording should fail")
	}
}
