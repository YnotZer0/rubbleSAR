package doctor

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSys answers commands from a table; anything missing fails.
type fakeSys struct {
	arch  string
	cmds  map[string]string
	files map[string]string
	dial  error
	http  string
	free  uint64
}

func (f fakeSys) Run(_ context.Context, name string, args ...string) (string, error) {
	k := strings.Join(append([]string{name}, args...), " ")
	if v, ok := f.cmds[k]; ok {
		return v, nil
	}
	return "not found", errors.New("exit status 127")
}
func (f fakeSys) ReadFile(p string) ([]byte, error) {
	if v, ok := f.files[p]; ok {
		return []byte(v), nil
	}
	return nil, fs.ErrNotExist
}
func (f fakeSys) Stat(p string) (os.FileInfo, error) {
	if _, ok := f.files[p]; ok {
		return nil, nil
	}
	return nil, fs.ErrNotExist
}
func (f fakeSys) Dial(string, time.Duration) error { return f.dial }
func (f fakeSys) HTTPGet(string, time.Duration) (string, error) {
	if f.http == "" {
		return "", errors.New("connection refused")
	}
	return f.http, nil
}
func (f fakeSys) FreeBytes(string) (uint64, error) { return f.free, nil }
func (f fakeSys) GOARCH() string                   { return f.arch }

func healthyZBook() fakeSys {
	return fakeSys{
		arch: "amd64",
		free: 100 << 30,
		cmds: map[string]string{
			"go version":                     "go version go1.26.0 linux/amd64",
			"gcc --version":                  "gcc (Ubuntu 15.2.0) 15.2.0",
			"pkg-config --version":           "1.8.1",
			"pkg-config --modversion zenohc": "1.9.0",
			"ldconfig -p":                    "libzenohc.so (libc6,x86-64) => /usr/local/lib/libzenohc.so",
			"timedatectl show -p NTPSynchronized --value":                                                  "yes",
			"systemctl is-active chrony":                                                                   "active",
			"nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader":                "Quadro RTX 4000, 580.95, 8192 MiB",
			"docker info --format {{.ServerVersion}} runtimes={{range $k, $v := .Runtimes}}{{$k}} {{end}}": "29.0.0 runtimes=io.containerd.runc.v2 nvidia runc ",
			"systemctl is-active zenohd":                                                                   "active",
			"/usr/local/bin/zenohd --version":                                                              "zenohd v1.9.0 built with rustc 1.93.0",
			"ufw status":                                                                                   "Status: inactive",
			"python3 --version":                                                                            "Python 3.14.4",
			"pkg-config --modversion rubblesar-onnxruntime":                                                "1.22.0",
			"systemctl is-active casualty-analyst":                                                         "active",
			"systemctl is-active mission-planner":                                                          "active",
			"systemctl is-active rubblesar-taky":                                                           "active",
			"systemctl is-active rescue-dispatch":                                                          "active",
			"systemctl is-active sar-web":                                                                  "active",
		},
		files: map[string]string{
			"/etc/os-release":                                         "PRETTY_NAME=\"Ubuntu 26.04 LTS\"\nVERSION_ID=\"26.04\"\n",
			"/opt/rubblesar/bin/sar-doctor":                           "",
			"/opt/rubblesar/bin/sar-check":                            "",
			"/opt/rubblesar/bin/sar-monitor":                          "",
			"/opt/rubblesar/bin/scenario-tool":                        "",
			"/opt/rubblesar/bin/sar-infer":                            "",
			"/etc/rubblesar/sar.toml":                                 "",
			"/var/lib/rubblesar/models/current/model.json":            `{"name":"rubblesar-v1-yolo11s-seg"}`,
			"/var/lib/rubblesar/scenarios/antakya-demo/scenario.json": "",
			"/var/lib/rubblesar/scenarios/antakya-demo/sites":         "",
			"/var/lib/rubblesar/roads/antakya-demo.json":              "",
			MapPath: "",
		},
		http: `[{"key":"@/x/router","value":{"sessions":[{"whatami":"client"},{"whatami":"client"}],"version":"v1.9.0-abc"}}]`,
	}
}

func TestHealthyZBookPasses(t *testing.T) {
	var out bytes.Buffer
	r := Run(context.Background(), healthyZBook(), Options{Role: "zbook", Endpoint: "127.0.0.1:7447"}, &out)
	p, w, f, _ := r.Counts()
	if f != 0 || w != 0 {
		t.Fatalf("pass=%d warn=%d fail=%d\n%s", p, w, f, out.String())
	}
	if !strings.Contains(out.String(), "2 client and 0 peer session(s)") {
		t.Fatalf("REST summary missing:\n%s", out.String())
	}
}

func TestBrokenZBookFailsWithHints(t *testing.T) {
	s := healthyZBook()
	delete(s.cmds, "pkg-config --modversion zenohc")
	s.cmds["systemctl is-active zenohd"] = "inactive"
	s.cmds["ufw status"] = "Status: active\nTo Action From\n22/tcp ALLOW Anywhere"
	s.dial = errors.New("connection refused")
	var out bytes.Buffer
	r := Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447"}, &out)
	if r.Passed() {
		t.Fatal("must fail")
	}
	txt := out.String()
	for _, want := range []string{"[FAIL] DOC-04", "10-zenoh-c.sh", "[FAIL] DOC-Z3", "[FAIL] DOC-Z5", "ufw allow 7447/tcp", "[FAIL] DOC-07", "connection refused"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
}

func TestJetsonChecks(t *testing.T) {
	s := fakeSys{
		arch: "arm64", free: 40 << 30,
		cmds: map[string]string{
			"go version":                     "go version go1.26.0 linux/arm64",
			"gcc --version":                  "gcc (Ubuntu 11.4.0) 11.4.0",
			"pkg-config --version":           "0.29.2",
			"pkg-config --modversion zenohc": "1.9.0",
			"ldconfig -p":                    "libzenohc.so (libc6,AArch64) => /usr/local/lib/libzenohc.so",
			"timedatectl show -p NTPSynchronized --value": "yes",
			"dpkg-query -W -f=${Version} nvidia-jetpack":  "6.2.2+b24",
			"/usr/local/cuda/bin/nvcc --version":          "nvcc: NVIDIA\nCuda compilation tools, release 12.6, V12.6.68",
			"dpkg-query -W -f=${Version} libnvinfer10":    "10.3.0.30-1+cuda12.5",
			"systemctl is-active nvargus-daemon":          "active",
			"gst-inspect-1.0 nvarguscamerasrc":            "Factory Details: ...",
			"nvpmodel -q":                                 "NV Power Mode: MAXN_SUPER\n2",
		},
		files: map[string]string{
			"/etc/os-release":       "PRETTY_NAME=\"Ubuntu 22.04.5 LTS\"\nVERSION_ID=\"22.04\"\n",
			"/etc/nv_tegra_release": "# R36 (release), REVISION: 5.0, GCID: 1, BOARD: generic, EABI: aarch64",
			"/dev/video0":           "",
			"/dev/video1":           "",
			"/usr/include/aarch64-linux-gnu/NvInfer.h":   "",
			"/usr/local/cuda/include/cuda_runtime_api.h": "",
			"/usr/src/tensorrt/bin/trtexec":              "",
		},
	}
	if DetectRole(s) != "jetson" {
		t.Fatal("role detection")
	}
	var out bytes.Buffer
	r := Run(context.Background(), s, Options{Role: "jetson", Endpoint: "192.168.1.50:7447"}, &out)
	txt := out.String()
	for _, want := range []string{"[PASS] DOC-J1", "nvidia-jetpack 6.2.2", "release 12.6", "[PASS] DOC-J3", "TensorRT 10.3", "2 of 2 camera", "MAXN_SUPER", "[WARN] DOC-08",
		"[WARN] DOC-10", "12-onnxruntime.sh", "[WARN] DOC-11", "80-engine.sh", "[WARN] DOC-J6"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if !r.Passed() {
		t.Fatalf("jetson run failed:\n%s", txt)
	}
}

func TestGoVersionRules(t *testing.T) {
	s := healthyZBook()
	s.cmds["go version"] = "go version go1.22.5 linux/amd64"
	if res := checkGo(context.Background(), s); res.Status != "FAIL" {
		t.Fatalf("old Go accepted: %+v", res)
	}
	s.cmds["go version"] = "go version go1.25.3 linux/amd64"
	if res := checkGo(context.Background(), s); res.Status != "PASS" || !strings.Contains(res.Detail, "recommended") {
		t.Fatalf("1.25: %+v", res)
	}
}

// After 00-prereqs on the ZBook: zenoh-c, zenohd and the tools are not there yet.
// With --stage prereqs these are TODO, nothing fails and no copy block is printed.
func TestPrereqsStageMarksLaterStepsTodo(t *testing.T) {
	s := healthyZBook()
	delete(s.cmds, "pkg-config --modversion zenohc")
	s.cmds["systemctl is-active zenohd"] = "inactive"
	delete(s.cmds, "/usr/local/bin/zenohd --version")
	s.http = ""
	s.dial = errors.New("connection refused")
	for _, f := range []string{"/opt/rubblesar/bin/sar-doctor", "/opt/rubblesar/bin/sar-check", "/opt/rubblesar/bin/sar-monitor", "/etc/rubblesar/sar.toml", "/var/lib/rubblesar/scenarios/antakya-demo/scenario.json"} {
		delete(s.files, f)
	}
	var out bytes.Buffer
	r := Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447", Stage: "prereqs"}, &out)
	if _, err := r.Finish(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	txt := out.String()
	if !r.Passed() {
		t.Fatalf("prereqs stage must pass:\n%s", txt)
	}
	for _, want := range []string{"[TODO] DOC-04", "installed by scripts/zbook/10-zenoh-c.sh", "[TODO] DOC-Z3", "[TODO] DOC-Z4", "[TODO] DOC-07", "[TODO] DOC-08", "[TODO] DOC-09", "installed by scripts/zbook/60-scenario.sh", "8 to be installed by later steps"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "COPY FROM HERE") {
		t.Errorf("no copy block expected for TODO items:\n%s", txt)
	}

	// Without the stage the same machine fails, as before.
	out.Reset()
	if r := Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447"}, &out); r.Passed() {
		t.Fatal("full check must fail when zenoh-c is missing")
	}
}

// A real problem found at the prereqs stage (here: no gcc) still fails.
func TestPrereqsStageStillFailsRealProblems(t *testing.T) {
	s := healthyZBook()
	delete(s.cmds, "gcc --version")
	var out bytes.Buffer
	r := Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447", Stage: "prereqs"}, &out)
	if r.Passed() || !strings.Contains(out.String(), "[FAIL] DOC-03") {
		t.Fatalf("missing gcc must fail at the prereqs stage:\n%s", out.String())
	}
}

// Port 8447 answered by a different web server (your ZBook had one on 8000).
func TestRestPortTakenByAnotherProgram(t *testing.T) {
	s := healthyZBook()
	res := checkRest(errHTTP{s, "HTTP 404"}, "")
	if res.Status != "WARN" || !strings.Contains(res.Detail, "another program answers on http://127.0.0.1:8447") || !strings.Contains(res.Hint, "ss -ltnp | grep 8447") {
		t.Fatalf("got %+v", res)
	}
	s.http = "<html>hello</html>"
	if res := checkRest(s, ""); res.Status != "WARN" || !strings.Contains(res.Detail, "not zenohd") {
		t.Fatalf("got %+v", res)
	}
}

type errHTTP struct {
	fakeSys
	msg string
}

func (e errHTTP) HTTPGet(string, time.Duration) (string, error) { return "", errors.New(e.msg) }

// A foreign program on the admin port is reported even at the prereqs stage.
func TestPrereqsStageKeepsPortConflicts(t *testing.T) {
	s := healthyZBook()
	s.http = "<html>other app</html>"
	var out bytes.Buffer
	Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447", Stage: "prereqs"}, &out)
	if !strings.Contains(out.String(), "[WARN] DOC-Z4") || !strings.Contains(out.String(), "E-DOC-002") {
		t.Fatalf("port conflict hidden:\n%s", out.String())
	}
	// 7447 held by something that is not the zenohd service.
	s = healthyZBook()
	s.cmds["systemctl is-active zenohd"] = "inactive"
	out.Reset()
	Run(context.Background(), s, Options{Role: "zbook", Endpoint: "127.0.0.1:7447", Stage: "prereqs"}, &out)
	if !strings.Contains(out.String(), "[FAIL] DOC-Z3") || !strings.Contains(out.String(), "port 7447 is in use") {
		t.Fatalf("7447 conflict hidden:\n%s", out.String())
	}
}

// Phase 2 pieces: TensorRT without headers/trtexec, model without an engine, wrong ONNX Runtime.
func TestPhase2Pieces(t *testing.T) {
	s := fakeSys{cmds: map[string]string{"dpkg-query -W -f=${Version} libnvinfer10": "10.3.0.30-1+cuda12.5"}, files: map[string]string{}}
	res := checkTensorRT(context.Background(), s)
	if res.Status != "WARN" || !strings.Contains(res.Detail, "trtexec") || !strings.Contains(res.Hint, "75-tensorrt.sh") {
		t.Fatalf("missing TensorRT pieces: %+v", res)
	}
	s.files[ModelPath+"/model.json"] = `{"name":"m1"}`
	if res := checkModel(s, "jetson"); res.Status != "WARN" || !strings.Contains(res.Detail, "no TensorRT engine") {
		t.Fatalf("model without engine: %+v", res)
	}
	s.files[ModelPath+"/model.engine"] = ""
	if res := checkModel(s, "jetson"); res.Status != "PASS" || !strings.Contains(res.Detail, "m1") {
		t.Fatalf("model with engine: %+v", res)
	}
	s.files[ModelPath+"/model.json"] = `{"name":"stub"}`
	if res := checkModel(s, "zbook"); res.Status != "WARN" || !strings.Contains(res.Detail, "stand-in") {
		t.Fatalf("stub model: %+v", res)
	}
	s.files[ModelPath+"/model.json"] = `not json`
	if res := checkModel(s, "zbook"); res.Status != "WARN" {
		t.Fatalf("broken card: %+v", res)
	}
	s.cmds["pkg-config --modversion rubblesar-onnxruntime"] = "1.20.1"
	if res := checkORT(context.Background(), s); res.Status != "WARN" || !strings.Contains(res.Detail, "1.22.0 expected") {
		t.Fatalf("old ORT: %+v", res)
	}
	z := healthyZBook()
	z.cmds["docker info --format {{.ServerVersion}} runtimes={{range $k, $v := .Runtimes}}{{$k}} {{end}}"] = "29.0.0 runtimes=io.containerd.runc.v2 runc "
	if res := checkDocker(context.Background(), z); res.Status != "WARN" || !strings.Contains(res.Hint, "GPU in Docker") {
		t.Fatalf("docker without toolkit: %+v", res)
	}
	z.cmds["nvidia-ctk --version"] = "NVIDIA Container Toolkit CLI version 1.18.0"
	if res := checkDocker(context.Background(), z); res.Status != "PASS" {
		t.Fatalf("docker with nvidia-ctk: %+v", res)
	}
}

func TestPhase3Config(t *testing.T) {
	old := "[site]\nincident_id = \"x\"\n[[uav]]\nid = \"scout-1\"\nbehaviour = \"orbit\"\n"
	s := fakeSys{files: map[string]string{"/etc/rubblesar/edge-eye.toml": old}}
	if r := checkEdgeP3(s); r.Status != "WARN" {
		t.Fatalf("phase 0 config: %+v", r)
	}
	b, err := os.ReadFile("../../configs/jetson/edge-eye.toml")
	if err != nil {
		t.Fatal(err)
	}
	s.files["/etc/rubblesar/edge-eye.toml"] = string(b)
	if r := checkEdgeP3(s); r.Status != "PASS" || !strings.Contains(r.Detail, "1 survey drones") {
		t.Fatalf("release config: %+v", r)
	}
	if r := checkEdgeP4(s); r.Status != "PASS" || !strings.Contains(r.Detail, "2 drones on standby") {
		t.Fatalf("release config, phase 4: %+v", r)
	}
	s.files["/etc/rubblesar/edge-eye.toml"] = old
	if r := checkEdgeP4(s); r.Status != "WARN" {
		t.Fatalf("old config, phase 4: %+v", r)
	}
	z := healthyZBook()
	z.cmds["systemctl is-active casualty-analyst"] = "inactive"
	if r := checkAnalyst(context.Background(), z); r.Status != "WARN" {
		t.Fatalf("analyst inactive: %+v", r)
	}
	if r := checkPlanner(context.Background(), z); r.Status != "PASS" {
		t.Fatalf("planner active: %+v", r)
	}
	z.cmds["systemctl is-active mission-planner"] = "failed"
	if r := checkPlanner(context.Background(), z); r.Status != "WARN" {
		t.Fatalf("planner failed: %+v", r)
	}
	if r := checkDispatch(context.Background(), z); r.Status != "PASS" {
		t.Fatalf("dispatch: %+v", r)
	}
	delete(z.files, "/var/lib/rubblesar/roads/antakya-demo.json")
	z.cmds["systemctl is-active rubblesar-taky"] = "inactive"
	if r := checkDispatch(context.Background(), z); r.Status != "WARN" || !strings.Contains(r.Detail, "34-taky.sh") || !strings.Contains(r.Detail, "32-roads.sh") {
		t.Fatalf("dispatch missing parts: %+v", r)
	}
}
