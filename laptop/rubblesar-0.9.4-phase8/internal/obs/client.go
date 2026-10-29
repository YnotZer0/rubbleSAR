package obs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// Client is a connection to obs-websocket 5.
type Client struct {
	URL string
	// Version: OBS Studio's and obs-websocket's.
	OBSVersion, WSVersion string

	ws      *wsConn
	mu      sync.Mutex
	pending map[string]chan response
	next    int
	done    chan struct{}
	err     error
}

type message struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type response struct {
	Type   string `json:"requestType"`
	ID     string `json:"requestId"`
	Status struct {
		Result  bool   `json:"result"`
		Code    int    `json:"code"`
		Comment string `json:"comment"`
	} `json:"requestStatus"`
	Data json.RawMessage `json:"responseData"`
}

// CloseError is obs-websocket closing the connection (4009: wrong password).
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	switch e.Code {
	case 4009:
		return "OBS refused the password (Tools → WebSocket Server Settings → Show Connect Info)"
	case 4010:
		return "OBS's obs-websocket speaks another protocol version (OBS 28 or later is needed)"
	}
	if e.Reason != "" {
		return fmt.Sprintf("OBS closed the connection: %s (%d)", e.Reason, e.Code)
	}
	return fmt.Sprintf("OBS closed the connection (%d)", e.Code)
}

// Connect opens and authenticates a session.
func Connect(url, password string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ws, err := dialWS(url, timeout)
	if err != nil {
		return nil, err
	}
	c := &Client{URL: url, ws: ws, pending: map[string]chan response{}, done: make(chan struct{})}
	_ = ws.c.SetReadDeadline(time.Now().Add(timeout))
	m, err := c.read()
	if err != nil {
		ws.close()
		return nil, fmt.Errorf("waiting for OBS's hello: %w", err)
	}
	if m.Op != 0 {
		ws.close()
		return nil, fmt.Errorf("expected OBS's hello, got op %d", m.Op)
	}
	var hello struct {
		Version string `json:"obsWebSocketVersion"`
		RPC     int    `json:"rpcVersion"`
		Auth    *struct {
			Challenge string `json:"challenge"`
			Salt      string `json:"salt"`
		} `json:"authentication"`
	}
	if err := json.Unmarshal(m.D, &hello); err != nil {
		ws.close()
		return nil, fmt.Errorf("OBS's hello: %w", err)
	}
	c.WSVersion = hello.Version
	id := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if hello.Auth != nil {
		if password == "" {
			ws.close()
			return nil, errors.New("OBS asks for a password: set it for this recorder in demo.toml (OBS: Tools → WebSocket Server Settings → Show Connect Info)")
		}
		id["authentication"] = Auth(password, hello.Auth.Salt, hello.Auth.Challenge)
	}
	if err := c.send(1, id); err != nil {
		ws.close()
		return nil, err
	}
	m, err = c.read()
	if err != nil {
		ws.close()
		return nil, err
	}
	if m.Op != 2 {
		ws.close()
		return nil, fmt.Errorf("expected Identified, got op %d", m.Op)
	}
	_ = ws.c.SetReadDeadline(time.Time{})
	go c.loop()
	var v struct {
		OBS string `json:"obsVersion"`
		WS  string `json:"obsWebSocketVersion"`
	}
	if err := c.Call(context.Background(), "GetVersion", nil, &v); err == nil {
		c.OBSVersion, c.WSVersion = v.OBS, v.WS
	}
	return c, nil
}

// Auth is obs-websocket's authentication string for a password.
func Auth(password, salt, challenge string) string {
	s := sha256.Sum256([]byte(password + salt))
	secret := base64.StdEncoding.EncodeToString(s[:])
	a := sha256.Sum256([]byte(secret + challenge))
	return base64.StdEncoding.EncodeToString(a[:])
}

func (c *Client) send(op int, d any) error {
	b, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	return c.ws.writeText(b)
}

func (c *Client) read() (*message, error) {
	b, err := c.ws.readMessage()
	if err != nil {
		var ce *closeFrame
		if errors.As(err, &ce) {
			return nil, &CloseError{Code: ce.code, Reason: ce.reason}
		}
		return nil, err
	}
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *Client) loop() {
	defer close(c.done)
	for {
		m, err := c.read()
		if err != nil {
			c.mu.Lock()
			c.err = err
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = map[string]chan response{}
			c.mu.Unlock()
			return
		}
		if m.Op != 7 {
			continue // events (op 5) are not subscribed to; others are not expected
		}
		var r response
		if json.Unmarshal(m.D, &r) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[r.ID]
		delete(c.pending, r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

// Call sends a request and decodes its answer into out (may be nil).
func (c *Client) Call(ctx context.Context, requestType string, data any, out any) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := requestType + "-" + strconv.Itoa(c.next)
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	d := map[string]any{"requestType": requestType, "requestId": id}
	if data != nil {
		d["requestData"] = data
	}
	if err := c.send(6, d); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: no answer from OBS: %w", requestType, ctx.Err())
	case r, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			return fmt.Errorf("%s: connection to OBS lost: %v", requestType, err)
		}
		if !r.Status.Result {
			return &RequestError{Type: requestType, Code: r.Status.Code, Comment: r.Status.Comment}
		}
		if out != nil && len(r.Data) > 0 {
			return json.Unmarshal(r.Data, out)
		}
		return nil
	}
}

// RequestError is a request OBS refused.
type RequestError struct {
	Type    string
	Code    int
	Comment string
}

func (e *RequestError) Error() string {
	if e.Comment != "" {
		return fmt.Sprintf("%s: %s (code %d)", e.Type, e.Comment, e.Code)
	}
	return fmt.Sprintf("%s: refused (code %d)", e.Type, e.Code)
}

// Close ends the session.
func (c *Client) Close() error {
	err := c.ws.close()
	<-c.done
	return err
}

// --- The requests the director uses ---------------------------------------------------------------

// RecordStatus is GetRecordStatus.
type RecordStatus struct {
	Active   bool    `json:"outputActive"`
	Paused   bool    `json:"outputPaused"`
	Timecode string  `json:"outputTimecode"`
	Duration float64 `json:"outputDuration"` // ms
	Bytes    float64 `json:"outputBytes"`
}

// RecordStatus says whether OBS is recording and for how long.
func (c *Client) RecordStatus(ctx context.Context) (RecordStatus, error) {
	var s RecordStatus
	err := c.Call(ctx, "GetRecordStatus", nil, &s)
	return s, err
}

// StartRecord starts recording.
func (c *Client) StartRecord(ctx context.Context) error { return c.Call(ctx, "StartRecord", nil, nil) }

// StopRecord stops recording and returns the file OBS wrote (on its own machine).
func (c *Client) StopRecord(ctx context.Context) (string, error) {
	var out struct {
		Path string `json:"outputPath"`
	}
	err := c.Call(ctx, "StopRecord", nil, &out)
	return out.Path, err
}

// Chapter adds a chapter marker to the recording (OBS 30.2 and later, Hybrid MP4 only).
func (c *Client) Chapter(ctx context.Context, name string) error {
	return c.Call(ctx, "CreateRecordChapter", map[string]any{"chapterName": name}, nil)
}

// SetScene switches the program scene.
func (c *Client) SetScene(ctx context.Context, name string) error {
	return c.Call(ctx, "SetCurrentProgramScene", map[string]any{"sceneName": name}, nil)
}

// Scenes lists the scene names and the current one.
func (c *Client) Scenes(ctx context.Context) ([]string, string, error) {
	var out struct {
		Current string `json:"currentProgramSceneName"`
		Scenes  []struct {
			Name string `json:"sceneName"`
		} `json:"scenes"`
	}
	if err := c.Call(ctx, "GetSceneList", nil, &out); err != nil {
		return nil, "", err
	}
	var names []string
	for i := len(out.Scenes) - 1; i >= 0; i-- { // OBS lists them bottom-up
		names = append(names, out.Scenes[i].Name)
	}
	return names, out.Current, nil
}

// RecordDirectory is where OBS writes recordings.
func (c *Client) RecordDirectory(ctx context.Context) (string, error) {
	var out struct {
		Dir string `json:"recordDirectory"`
	}
	err := c.Call(ctx, "GetRecordDirectory", nil, &out)
	return out.Dir, err
}

// VideoSettings is the canvas and output size and frame rate.
type VideoSettings struct {
	BaseWidth    int `json:"baseWidth"`
	BaseHeight   int `json:"baseHeight"`
	OutputWidth  int `json:"outputWidth"`
	OutputHeight int `json:"outputHeight"`
	FPSNum       int `json:"fpsNumerator"`
	FPSDen       int `json:"fpsDenominator"`
}

// Video reads the video settings.
func (c *Client) Video(ctx context.Context) (VideoSettings, error) {
	var v VideoSettings
	err := c.Call(ctx, "GetVideoSettings", nil, &v)
	return v, err
}

// InputKinds lists the source kinds this OBS has (browser_source, window_capture, ...).
func (c *Client) InputKinds(ctx context.Context) ([]string, error) {
	var out struct {
		Kinds []string `json:"inputKinds"`
	}
	err := c.Call(ctx, "GetInputKindList", map[string]any{"unversioned": true}, &out)
	return out.Kinds, err
}

// CreateScene adds a scene.
func (c *Client) CreateScene(ctx context.Context, name string) error {
	return c.Call(ctx, "CreateScene", map[string]any{"sceneName": name}, nil)
}

// CreateInput adds a source to a scene.
func (c *Client) CreateInput(ctx context.Context, scene, name, kind string, settings map[string]any) error {
	return c.Call(ctx, "CreateInput", map[string]any{"sceneName": scene, "inputName": name, "inputKind": kind, "inputSettings": settings, "sceneItemEnabled": true}, nil)
}

// SetInputSettings changes a source's settings.
func (c *Client) SetInputSettings(ctx context.Context, name string, settings map[string]any) error {
	return c.Call(ctx, "SetInputSettings", map[string]any{"inputName": name, "inputSettings": settings, "overlay": true}, nil)
}

// closeFrame is a close frame's code and reason.
type closeFrame struct {
	code   int
	reason string
}

func (e *closeFrame) Error() string { return fmt.Sprintf("closed (%d %s)", e.code, e.reason) }

func parseClose(p []byte) error {
	if len(p) >= 2 {
		return &closeFrame{code: int(binary.BigEndian.Uint16(p[:2])), reason: string(p[2:])}
	}
	return io.EOF
}
