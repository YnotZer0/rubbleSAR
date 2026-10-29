package director

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"rubblesar/internal/obs"
)

// Recorder records one screen: OBS on a machine, or a program (scrcpy).
type Recorder interface {
	Name() string
	// Check connects and says what it is ("OBS 30.2.3 on ws://..., scene ... present").
	Check(ctx context.Context, take string) (string, error)
	// Start starts recording for a take (into dir, for programs) and says when the recording's first frame was.
	Start(ctx context.Context, take, dir string) (time.Time, error)
	// Scene switches what is recorded (OBS); Chapter marks a chapter (OBS 30.2+, Hybrid MP4). Both best effort.
	Scene(ctx context.Context, name string) error
	Chapter(ctx context.Context, name string) error
	// Stop ends the recording and says where it is (on the recorder's own machine).
	Stop(ctx context.Context) (string, error)
	Close()
}

// NewRecorder makes a recorder from its settings.
func NewRecorder(rc RecorderConfig) Recorder {
	if rc.Kind == "command" {
		return &cmdRecorder{rc: rc}
	}
	return &obsRecorder{rc: rc}
}

// --- OBS ------------------------------------------------------------------------------------------

type obsRecorder struct {
	rc       RecorderConfig
	mu       sync.Mutex
	c        *obs.Client
	chapters bool
}

func (r *obsRecorder) Name() string { return r.rc.Name }

func (r *obsRecorder) conn() (*obs.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c != nil {
		return r.c, nil
	}
	c, err := obs.Connect(r.rc.URL, r.rc.Password, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("E-DIR-003: %s (%s): %w%s", r.rc.Name, r.rc.URL, err, connHint(err))
	}
	r.c, r.chapters = c, true
	return c, nil
}

// connHint says what a failed connection to OBS most likely means.
func connHint(err error) string {
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "connection refused"):
		return " (that machine answered, but nothing listens on the port: OBS is not running there, or Tools > WebSocket Server Settings > Enable WebSocket server is off, or the port is not 4455)"
	case strings.Contains(m, "timeout") || strings.Contains(m, "no route") || strings.Contains(m, "unreachable"):
		return " (no answer: the address is wrong, the machine is off or on another network, or its firewall blocks the port; on Windows allow OBS on private networks)"
	case strings.Contains(m, "4009"):
		return " (wrong password: OBS's Tools > WebSocket Server Settings > Show Connect Info, copy it into demo.toml)"
	}
	return ""
}

func (r *obsRecorder) sceneFor(take string) string {
	if take == "live" {
		return r.rc.SceneLive
	}
	return r.rc.SceneReplay
}

func (r *obsRecorder) Check(ctx context.Context, take string) (string, error) {
	c, err := r.conn()
	if err != nil {
		return "", err
	}
	desc := fmt.Sprintf("OBS %s (obs-websocket %s) at %s", c.OBSVersion, c.WSVersion, r.rc.URL)
	st, err := c.RecordStatus(ctx)
	if err != nil {
		return desc, fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
	}
	if st.Active {
		return desc, fmt.Errorf("E-DIR-003: %s is already recording: stop it in OBS first", r.rc.Name)
	}
	if v, err := c.Video(ctx); err == nil {
		desc += fmt.Sprintf(", %dx%d at %d fps", v.OutputWidth, v.OutputHeight, v.FPSNum/max(1, v.FPSDen))
		if v.OutputWidth < 1920 || v.OutputHeight < 1080 {
			desc += " (1920x1080 recommended: Settings > Video)"
		}
	}
	if d, err := c.RecordDirectory(ctx); err == nil {
		desc += ", recording to " + d
	}
	if s := r.sceneFor(take); s != "" {
		scenes, _, err := c.Scenes(ctx)
		if err != nil {
			return desc, fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
		}
		found := false
		for _, x := range scenes {
			found = found || x == s
		}
		if !found {
			return desc, fmt.Errorf("E-DIR-003: %s has no scene %q (scenes: %s); rubblesar demo obs-setup --recorder %s creates it", r.rc.Name, s, strings.Join(scenes, ", "), r.rc.Name)
		}
		desc += ", scene " + s
	}
	return desc, nil
}

func (r *obsRecorder) Start(ctx context.Context, take, _ string) (time.Time, error) {
	c, err := r.conn()
	if err != nil {
		return time.Time{}, err
	}
	if s := r.sceneFor(take); s != "" {
		if err := c.SetScene(ctx, s); err != nil {
			return time.Time{}, fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
		}
	}
	before := time.Now()
	if err := c.StartRecord(ctx); err != nil {
		return time.Time{}, fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
	}
	// When the recording's first frame was: now minus how long OBS says it has recorded.
	for i := 0; i < 40; i++ {
		st, err := c.RecordStatus(ctx)
		if err == nil && st.Active {
			at := time.Now().Add(-time.Duration(st.Duration * float64(time.Millisecond)))
			if at.Before(before) {
				at = before
			}
			return at, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return before, nil
}

func (r *obsRecorder) Scene(ctx context.Context, name string) error {
	c, err := r.conn()
	if err != nil {
		return err
	}
	return c.SetScene(ctx, name)
}

func (r *obsRecorder) Chapter(ctx context.Context, name string) error {
	c, err := r.conn()
	if err != nil {
		return err
	}
	r.mu.Lock()
	ok := r.chapters
	r.mu.Unlock()
	if !ok {
		return nil
	}
	err = c.Chapter(ctx, name)
	var re *obs.RequestError
	if errors.As(err, &re) {
		// Older OBS, or not recording to Hybrid MP4: stop trying.
		r.mu.Lock()
		r.chapters = false
		r.mu.Unlock()
	}
	return err
}

func (r *obsRecorder) Stop(ctx context.Context) (string, error) {
	c, err := r.conn()
	if err != nil {
		return "", err
	}
	p, err := c.StopRecord(ctx)
	if err != nil {
		return "", fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
	}
	return p, nil
}

func (r *obsRecorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c != nil {
		_ = r.c.Close()
		r.c = nil
	}
}

// --- A program (scrcpy) -------------------------------------------------------------------------

type cmdRecorder struct {
	rc   RecorderConfig
	cmd  *exec.Cmd
	file string
	done chan error
}

func (r *cmdRecorder) Name() string { return r.rc.Name }

func (r *cmdRecorder) program() string {
	f := strings.Fields(r.rc.Start)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func (r *cmdRecorder) Check(context.Context, string) (string, error) {
	p, err := exec.LookPath(r.program())
	if err != nil {
		return "", fmt.Errorf("E-DIR-003: %s: %s is not installed (%v)", r.rc.Name, r.program(), err)
	}
	return "runs " + p, nil
}

func (r *cmdRecorder) Start(_ context.Context, take, dir string) (time.Time, error) {
	name := r.rc.File
	if name == "" {
		name = r.rc.Name + ".mkv"
	}
	r.file = filepath.Join(dir, name)
	line := strings.ReplaceAll(r.rc.Start, "{file}", shellQuote(r.file))
	r.cmd = exec.Command("/bin/sh", "-c", "exec "+line)
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logf, err := os.Create(filepath.Join(dir, r.rc.Name+".log"))
	if err == nil {
		r.cmd.Stdout, r.cmd.Stderr = logf, logf
	}
	if err := r.cmd.Start(); err != nil {
		return time.Time{}, fmt.Errorf("E-DIR-003: %s: %w", r.rc.Name, err)
	}
	at := time.Now()
	r.done = make(chan error, 1)
	go func() { r.done <- r.cmd.Wait(); _ = logf.Close() }()
	// A program that ends at once did not start recording.
	select {
	case err := <-r.done:
		return time.Time{}, fmt.Errorf("E-DIR-003: %s ended at once (%v): see %s", r.rc.Name, err, filepath.Join(dir, r.rc.Name+".log"))
	case <-time.After(1500 * time.Millisecond):
	}
	return at, nil
}

func (r *cmdRecorder) Scene(context.Context, string) error   { return nil }
func (r *cmdRecorder) Chapter(context.Context, string) error { return nil }

func (r *cmdRecorder) Stop(context.Context) (string, error) {
	if r.cmd == nil || r.cmd.Process == nil {
		return "", errors.New("not started")
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGINT)
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		<-r.done
	}
	if _, err := os.Stat(r.file); err != nil {
		return r.file, fmt.Errorf("E-DIR-003: %s wrote no %s", r.rc.Name, r.file)
	}
	return r.file, nil
}

func (r *cmdRecorder) Close() {}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
