package obs

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Fake is a stand-in for OBS's obs-websocket, for tests and `rubblesar demo
// check --fake-obs`: it answers the requests the director makes and records
// them (a recording "file" per StartRecord/StopRecord).
type Fake struct {
	URL      string
	Password string

	srv       *httptest.Server
	mu        sync.Mutex
	recording bool
	started   time.Time
	scenes    []string
	current   string
	inputs    map[string]string // name -> kind
	Requests  []string
	Chapters  []string
	Files     []string
	// NoChapters makes CreateRecordChapter fail as on OBS before 30.2.
	NoChapters bool
}

// NewFake starts a fake obs-websocket ("" password: no authentication).
func NewFake(password string) *Fake {
	f := &Fake{Password: password, scenes: []string{"Scene"}, current: "Scene", inputs: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = "ws://" + strings.TrimPrefix(f.srv.URL, "http://")
	return f
}

// Close stops it.
func (f *Fake) Close() { f.srv.Close() }

// Recording says whether a recording is running.
func (f *Fake) Recording() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recording
}

// Scene is the current scene.
func (f *Fake) Scene() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

type srvConn struct {
	c net.Conn
	r *bufio.Reader
}

func (s *srvConn) write(op byte, b []byte) error {
	hdr := []byte{0x80 | op}
	n := len(b)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 1<<16:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		var e [8]byte
		binary.BigEndian.PutUint64(e[:], uint64(n))
		hdr = append(append(hdr, 127), e[:]...)
	}
	_, err := s.c.Write(append(hdr, b...))
	return err
}

func (s *srvConn) read() ([]byte, error) {
	w := &wsConn{c: s.c, r: s.r}
	return w.readMessage()
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: obswebsocket.json\r\n\r\n", acceptKey(r.Header.Get("Sec-WebSocket-Key")))
	_ = brw.Flush()
	s := &srvConn{c: c, r: brw.Reader}
	send := func(op int, d any) error {
		b, _ := json.Marshal(map[string]any{"op": op, "d": d})
		return s.write(0x1, b)
	}
	hello := map[string]any{"obsWebSocketVersion": "5.5.2", "rpcVersion": 1}
	salt, challenge := randText(), randText()
	if f.Password != "" {
		hello["authentication"] = map[string]any{"challenge": challenge, "salt": salt}
	}
	if send(0, hello) != nil {
		return
	}
	b, err := s.read()
	if err != nil {
		return
	}
	var id struct {
		D struct {
			Auth string `json:"authentication"`
		} `json:"d"`
	}
	_ = json.Unmarshal(b, &id)
	if f.Password != "" && id.D.Auth != Auth(f.Password, salt, challenge) {
		_ = s.write(0x8, append([]byte{0x0f, 0xa9}, []byte("Authentication failed.")...)) // 4009
		return
	}
	if send(2, map[string]any{"negotiatedRpcVersion": 1}) != nil {
		return
	}
	for {
		b, err := s.read()
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
		var req struct {
			Op int `json:"op"`
			D  struct {
				Type string          `json:"requestType"`
				ID   string          `json:"requestId"`
				Data json.RawMessage `json:"requestData"`
			} `json:"d"`
		}
		if json.Unmarshal(b, &req) != nil || req.Op != 6 {
			continue
		}
		ok, code, comment, data := f.answer(req.D.Type, req.D.Data)
		resp := map[string]any{"requestType": req.D.Type, "requestId": req.D.ID, "requestStatus": map[string]any{"result": ok, "code": code, "comment": comment}}
		if data != nil {
			resp["responseData"] = data
		}
		if send(7, resp) != nil {
			return
		}
	}
}

func randText() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func (f *Fake) answer(typ string, raw json.RawMessage) (bool, int, string, any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, typ)
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch typ {
	case "GetVersion":
		return true, 100, "", map[string]any{"obsVersion": "30.2.3", "obsWebSocketVersion": "5.5.2"}
	case "GetRecordStatus":
		d := 0.0
		if f.recording {
			d = float64(time.Since(f.started).Milliseconds())
		}
		return true, 100, "", map[string]any{"outputActive": f.recording, "outputPaused": false, "outputDuration": d, "outputTimecode": "00:00:00.000", "outputBytes": d * 1000}
	case "StartRecord":
		if f.recording {
			return false, 500, "recording is already active", nil
		}
		f.recording, f.started = true, time.Now()
		return true, 100, "", nil
	case "StopRecord":
		if !f.recording {
			return false, 501, "recording is not active", nil
		}
		f.recording = false
		p := fmt.Sprintf("/fake/obs/%s.mkv", f.started.Format("2006-01-02 15-04-05.000"))
		f.Files = append(f.Files, p)
		return true, 100, "", map[string]any{"outputPath": p}
	case "CreateRecordChapter":
		if f.NoChapters {
			return false, 204, "Chapters are only supported with Hybrid MP4", nil
		}
		if !f.recording {
			return false, 501, "recording is not active", nil
		}
		f.Chapters = append(f.Chapters, str("chapterName"))
		return true, 100, "", nil
	case "GetSceneList":
		var sc []map[string]any
		for i := len(f.scenes) - 1; i >= 0; i-- {
			sc = append(sc, map[string]any{"sceneName": f.scenes[i], "sceneIndex": i})
		}
		return true, 100, "", map[string]any{"currentProgramSceneName": f.current, "scenes": sc}
	case "SetCurrentProgramScene":
		for _, s := range f.scenes {
			if s == str("sceneName") {
				f.current = s
				return true, 100, "", nil
			}
		}
		return false, 600, "No source was found by the name of `" + str("sceneName") + "`.", nil
	case "CreateScene":
		for _, s := range f.scenes {
			if s == str("sceneName") {
				return false, 601, "A source already exists by that scene name.", nil
			}
		}
		f.scenes = append(f.scenes, str("sceneName"))
		return true, 100, "", nil
	case "CreateInput":
		if _, dup := f.inputs[str("inputName")]; dup {
			return false, 601, "A source already exists by that input name.", nil
		}
		f.inputs[str("inputName")] = str("inputKind")
		return true, 100, "", nil
	case "SetInputSettings":
		if _, ok := f.inputs[str("inputName")]; !ok {
			return false, 600, "No source was found.", nil
		}
		return true, 100, "", nil
	case "GetInputKindList":
		return true, 100, "", map[string]any{"inputKinds": []string{"browser_source", "pipewire-screen-capture-source", "window_capture", "monitor_capture"}}
	case "GetRecordDirectory":
		return true, 100, "", map[string]any{"recordDirectory": "/fake/obs"}
	case "GetVideoSettings":
		return true, 100, "", map[string]any{"baseWidth": 1920, "baseHeight": 1080, "outputWidth": 1920, "outputHeight": 1080, "fpsNumerator": 30, "fpsDenominator": 1}
	}
	return false, 204, "unknown request type", nil
}
