// Package doctor checks that a machine is ready to build and run RubbleSAR.
// It is pure Go (no cgo, no zenoh) so it runs before anything else is installed.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rubblesar/internal/check"
)

// Pinned versions (keep in sync with VERSIONS.env).
const (
	WantZenohC     = "1.9.0"
	WantGo         = "1.26.0"
	MinGoMinor     = 24
	WantL4TRelease = "R36"
	WantORT        = "1.22.0"
)

// Sys abstracts the operating system so checks can be unit-tested.
type Sys interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	ReadFile(path string) ([]byte, error)
	Stat(path string) (os.FileInfo, error)
	Dial(addr string, timeout time.Duration) error
	HTTPGet(url string, timeout time.Duration) (string, error)
	FreeBytes(path string) (uint64, error)
	GOARCH() string
}

// RealSys is the production implementation.
type RealSys struct{}

func (RealSys) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
func (RealSys) ReadFile(p string) ([]byte, error)  { return os.ReadFile(p) }
func (RealSys) Stat(p string) (os.FileInfo, error) { return os.Stat(p) }
func (RealSys) GOARCH() string                     { return runtime.GOARCH }
func (RealSys) Dial(addr string, t time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, t)
	if err != nil {
		return err
	}
	return c.Close()
}
func (RealSys) HTTPGet(url string, t time.Duration) (string, error) {
	cl := http.Client{Timeout: t}
	resp, err := cl.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return string(b), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), err
}
func (RealSys) FreeBytes(p string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Options select what to check.
type Options struct {
	Role     string // jetson | zbook
	Endpoint string // zenoh router host:port to test, e.g. 192.168.1.50:7447
	RestURL  string // zenohd REST admin (ZBook only)
	// Stage "prereqs" reports items installed by later setup steps as TODO
	// instead of FAIL/WARN. Empty means a full check.
	Stage string
}

// DetectRole guesses the role from the hardware.
func DetectRole(s Sys) string {
	if _, err := s.Stat("/etc/nv_tegra_release"); err == nil {
		return "jetson"
	}
	return "zbook"
}

func pass(d string, ev ...string) check.Result {
	return check.Result{Status: check.Pass, Detail: d, Evidence: ev}
}
func warn(d, hint string, ev ...string) check.Result {
	return check.Result{Status: check.Warn, Code: "E-DOC-001", Detail: d, Hint: hint, Evidence: ev}
}
func fail(d, hint string, ev ...string) check.Result {
	return check.Result{Status: check.Fail, Code: "E-DOC-001", Detail: d, Hint: hint, Evidence: ev}
}

func conflict(st check.Status, d, hint string) check.Result {
	return check.Result{Status: st, Code: "E-DOC-002", Detail: d, Hint: hint}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Run executes every check for the role and returns the report.
// DefaultRestURL is where zenohd's read-only REST admin API listens (ZBook).
const DefaultRestURL = "http://127.0.0.1:8447"

// laterSteps names the setup step that installs what a check looks for.
func laterSteps(role string) map[string]string {
	if role == "jetson" {
		return map[string]string{
			"DOC-04": "scripts/jetson/10-zenoh-c.sh",
			"DOC-07": "scripts/jetson/30-install.sh (and zenohd on the ZBook)",
			"DOC-08": "scripts/jetson/30-install.sh",
			"DOC-09": "scripts/jetson/60-scenario.sh",
			"DOC-J3": "scripts/jetson/75-tensorrt.sh",
			"DOC-10": "scripts/jetson/12-onnxruntime.sh",
			"DOC-11": "scripts/jetson/80-engine.sh (phase 2)",
			"DOC-J6": "scripts/jetson/30-install.sh --force-config (phase 3)",
			"DOC-J7": "scripts/jetson/30-install.sh --force-config (phase 4)",
		}
	}
	return map[string]string{
		"DOC-04":  "scripts/zbook/10-zenoh-c.sh",
		"DOC-Z3":  "scripts/zbook/15-zenohd.sh",
		"DOC-Z4":  "scripts/zbook/15-zenohd.sh",
		"DOC-07":  "scripts/zbook/15-zenohd.sh",
		"DOC-08":  "scripts/zbook/30-install.sh",
		"DOC-09":  "scripts/zbook/60-scenario.sh",
		"DOC-10":  "scripts/zbook/12-onnxruntime.sh",
		"DOC-11":  "scripts/zbook/82-evaluate.sh (phase 2)",
		"DOC-Z7":  "scripts/zbook/30-install.sh (phase 3)",
		"DOC-Z8":  "scripts/zbook/30-install.sh (phase 4)",
		"DOC-Z9":  "scripts/zbook/32-roads.sh, 34-taky.sh and 30-install.sh (phase 5)",
		"DOC-Z10": "scripts/zbook/30-install.sh and 36-map.sh (phase 6)",
	}
}

// Run executes every check for the role and returns the report.
func Run(ctx context.Context, s Sys, o Options, out io.Writer) *check.Report {
	r := check.NewToolReport("sar-doctor", "doctor-"+o.Role, out)
	r.Context["role"] = o.Role
	r.Context["endpoint"] = o.Endpoint
	if o.Stage != "" {
		r.Context["stage"] = o.Stage
	}
	jetson := o.Role == "jetson"
	later := map[string]string{}
	if o.Stage == "prereqs" {
		later = laterSteps(o.Role)
	}
	run := func(id, name string, fn func() check.Result) {
		r.Run(id, name, func() check.Result {
			res := fn()
			// Port conflicts (E-DOC-002) are real problems even before the later steps run.
			if step, ok := later[id]; ok && res.Code != "E-DOC-002" && (res.Status == check.Fail || res.Status == check.Warn) {
				return check.Result{Status: check.Todo, Detail: "installed by " + step + " (now: " + res.Detail + ")"}
			}
			return res
		})
	}

	run("DOC-01", "Operating system", func() check.Result { return checkOS(s, jetson) })
	run("DOC-02", "Go toolchain", func() check.Result { return checkGo(ctx, s) })
	run("DOC-03", "C compiler and pkg-config", func() check.Result { return checkCC(ctx, s) })
	run("DOC-04", "zenoh-c library "+WantZenohC, func() check.Result { return checkZenohC(ctx, s) })
	run("DOC-05", "Clock synchronisation", func() check.Result { return checkTime(ctx, s) })
	run("DOC-06", "Free disk space", func() check.Result { return checkDisk(s) })
	if jetson {
		run("DOC-J1", "JetPack / Jetson Linux", func() check.Result { return checkL4T(ctx, s) })
		run("DOC-J2", "CUDA toolkit", func() check.Result { return checkCUDA(ctx, s) })
		run("DOC-J3", "TensorRT", func() check.Result { return checkTensorRT(ctx, s) })
		run("DOC-J4", "CSI cameras", func() check.Result { return checkCameras(ctx, s) })
		run("DOC-J5", "Power mode", func() check.Result { return checkPower(ctx, s) })
		run("DOC-J6", "Phase 3 configuration", func() check.Result { return checkEdgeP3(s) })
		run("DOC-J7", "Phase 4 configuration", func() check.Result { return checkEdgeP4(s) })
	} else {
		run("DOC-Z1", "NVIDIA GPU", func() check.Result { return checkGPU(ctx, s) })
		run("DOC-Z2", "Docker engine", func() check.Result { return checkDocker(ctx, s) })
		run("DOC-Z3", "zenohd service", func() check.Result { return checkZenohd(ctx, s) })
		run("DOC-Z4", "zenohd admin API", func() check.Result { return checkRest(s, o.RestURL) })
		run("DOC-Z5", "Firewall allows 7447/tcp", func() check.Result { return checkFirewall(ctx, s) })
		run("DOC-Z6", "Python 3", func() check.Result { return checkPython(ctx, s) })
		run("DOC-Z7", "casualty-analyst service", func() check.Result { return checkAnalyst(ctx, s) })
		run("DOC-Z8", "mission-planner service", func() check.Result { return checkPlanner(ctx, s) })
		run("DOC-Z9", "TAK server and rescue-dispatch (phase 5)", func() check.Result { return checkDispatch(ctx, s) })
		run("DOC-Z10", "sar-web and the offline map (phase 6)", func() check.Result { return checkWeb(ctx, s) })
	}
	run("DOC-07", "Zenoh router reachable", func() check.Result { return checkReach(s, o.Endpoint, jetson) })
	run("DOC-08", "RubbleSAR installation", func() check.Result { return checkInstall(s, jetson) })
	run("DOC-09", "Simulated scenario (phase 1)", func() check.Result { return checkScenario(s, o.Role) })
	run("DOC-10", "ONNX Runtime "+WantORT, func() check.Result { return checkORT(ctx, s) })
	run("DOC-11", "Body-part model (phase 2)", func() check.Result { return checkModel(s, o.Role) })
	return r
}

func osRelease(s Sys) map[string]string {
	m := map[string]string{}
	b, err := s.ReadFile("/etc/os-release")
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}

func checkOS(s Sys, jetson bool) check.Result {
	m := osRelease(s)
	name, ver, arch := m["PRETTY_NAME"], m["VERSION_ID"], s.GOARCH()
	detail := fmt.Sprintf("%s, %s", name, arch)
	wantVer, wantArch := "26.04", "amd64"
	if jetson {
		wantVer, wantArch = "22.04", "arm64"
	}
	if arch != wantArch {
		return fail(detail, fmt.Sprintf("this role expects %s; use --role to override if the guess is wrong", wantArch))
	}
	if ver != wantVer {
		return warn(detail, fmt.Sprintf("tested on Ubuntu %s; other versions may work", wantVer))
	}
	return pass(detail)
}

var goVerRE = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

func checkGo(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "go", "version")
	if err != nil {
		for _, p := range []string{"/usr/local/go/bin/go"} {
			if o2, e2 := s.Run(ctx, p, "version"); e2 == nil {
				return warn(o2, "Go is installed but not on PATH: add 'export PATH=$PATH:/usr/local/go/bin' to ~/.profile and log in again")
			}
		}
		return fail("go not found", "run scripts/<machine>/00-prereqs.sh (installs Go "+WantGo+")", err.Error())
	}
	m := goVerRE.FindStringSubmatch(out)
	if m == nil {
		return warn(out, "could not parse the Go version")
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major != 1 || minor < MinGoMinor {
		return fail(out, fmt.Sprintf("Go 1.%d or newer is required; install %s with 00-prereqs.sh", MinGoMinor, WantGo))
	}
	if !strings.Contains(out, "go"+WantGo) {
		return pass(out + " (recommended " + WantGo + ")")
	}
	return pass(out)
}

func checkCC(ctx context.Context, s Sys) check.Result {
	gcc, err := s.Run(ctx, "gcc", "--version")
	if err != nil {
		return fail("gcc not found", "sudo apt install build-essential")
	}
	pc, err := s.Run(ctx, "pkg-config", "--version")
	if err != nil {
		return fail("pkg-config not found", "sudo apt install pkg-config")
	}
	return pass(firstLine(gcc) + "; pkg-config " + pc)
}

func checkZenohC(ctx context.Context, s Sys) check.Result {
	v, err := s.Run(ctx, "pkg-config", "--modversion", "zenohc")
	if err != nil {
		return fail("pkg-config cannot find zenohc", "run scripts/<machine>/10-zenoh-c.sh", v)
	}
	ld, _ := s.Run(ctx, "ldconfig", "-p")
	if !strings.Contains(ld, "libzenohc.so") {
		return fail("libzenohc.so is not in the dynamic linker cache", "sudo ldconfig (or re-run 10-zenoh-c.sh)")
	}
	if v != WantZenohC {
		return fail("zenohc "+v+" installed, "+WantZenohC+" required", "re-run 10-zenoh-c.sh; zenohd and zenoh-c must be the same version")
	}
	return pass("zenohc " + v + " (pkg-config and linker cache)")
}

func checkTime(ctx context.Context, s Sys) check.Result {
	sync, _ := s.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	chrony, _ := s.Run(ctx, "systemctl", "is-active", "chrony")
	detail := fmt.Sprintf("NTPSynchronized=%s, chrony=%s", orDash(sync), orDash(chrony))
	if sync != "yes" {
		return warn(detail, "clocks must agree to within 250 ms for latency checks: sudo apt install chrony && sudo systemctl enable --now chrony (see docs/ENVIRONMENT.md, Time)")
	}
	return pass(detail)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return firstLine(s)
}

func checkDisk(s Sys) check.Result {
	free, err := s.FreeBytes("/")
	if err != nil {
		return warn("could not read free space", "", err.Error())
	}
	gb := float64(free) / (1 << 30)
	d := fmt.Sprintf("%.1f GB free on /", gb)
	switch {
	case gb < 5:
		return fail(d, "at least 5 GB is needed to build; later phases need 20 GB or more")
	case gb < 20:
		return warn(d, "phase 0 fits, but later phases (imagery, models) need 20 GB or more")
	}
	return pass(d)
}

func checkL4T(ctx context.Context, s Sys) check.Result {
	b, err := s.ReadFile("/etc/nv_tegra_release")
	if err != nil {
		return fail("/etc/nv_tegra_release missing", "is this a Jetson? use --role zbook otherwise")
	}
	line := firstLine(strings.TrimSpace(string(b)))
	jp, _ := s.Run(ctx, "dpkg-query", "-W", "-f=${Version}", "nvidia-jetpack")
	detail := line
	if jp != "" && !strings.Contains(jp, "no packages") {
		detail += "; nvidia-jetpack " + jp
	}
	if !strings.Contains(line, WantL4TRelease) {
		return fail(detail, "JetPack 6.2.x (Jetson Linux R36) is required")
	}
	return pass(detail)
}

func checkCUDA(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "/usr/local/cuda/bin/nvcc", "--version")
	if err != nil {
		return warn("nvcc not found (needed from phase 2)", "sudo apt install nvidia-jetpack")
	}
	lines := strings.Split(out, "\n")
	return pass(strings.TrimSpace(lines[len(lines)-1]))
}

func checkTensorRT(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "dpkg-query", "-W", "-f=${Version}", "libnvinfer10")
	if err != nil || out == "" {
		out, err = s.Run(ctx, "dpkg-query", "-W", "-f=${Version}", "tensorrt")
	}
	if err != nil || out == "" {
		return warn("TensorRT not found (needed from phase 2)", "sudo apt install nvidia-jetpack")
	}
	// The pieces the build (headers) and the engine builder (trtexec) need.
	var missing []string
	for _, p := range []string{"/usr/include/aarch64-linux-gnu/NvInfer.h", "/usr/local/cuda/include/cuda_runtime_api.h", "/usr/src/tensorrt/bin/trtexec"} {
		if _, err := s.Stat(p); err != nil {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return warn("TensorRT "+out+" but missing "+strings.Join(missing, ", ")+" (needed from phase 2)", "run scripts/jetson/75-tensorrt.sh")
	}
	return pass("TensorRT " + out + " (headers, trtexec, CUDA runtime headers)")
}

func checkCameras(ctx context.Context, s Sys) check.Result {
	var found []string
	for _, d := range []string{"/dev/video0", "/dev/video1"} {
		if _, err := s.Stat(d); err == nil {
			found = append(found, d)
		}
	}
	argus, _ := s.Run(ctx, "systemctl", "is-active", "nvargus-daemon")
	gst, gerr := s.Run(ctx, "gst-inspect-1.0", "nvarguscamerasrc")
	detail := fmt.Sprintf("%d of 2 camera devices (%s), nvargus-daemon=%s", len(found), strings.Join(found, ", "), orDash(argus))
	if len(found) < 2 || argus != "active" || gerr != nil {
		ev := []string{}
		if gerr != nil {
			ev = append(ev, "gst-inspect-1.0 nvarguscamerasrc: "+firstLine(gst))
		}
		return warn(detail, "cameras are optional until phase 3; test them with scripts/jetson/cam-test.sh", ev...)
	}
	return pass(detail)
}

func checkPower(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "nvpmodel", "-q")
	if err != nil {
		return pass("not readable without sudo (informational): sudo nvpmodel -q")
	}
	return pass(strings.ReplaceAll(out, "\n", " "))
}

func checkGPU(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader")
	if err != nil {
		return warn("nvidia-smi failed (GPU needed from phase 1)", "check the NVIDIA driver installation", firstLine(out))
	}
	return pass(firstLine(out))
}

func checkDocker(ctx context.Context, s Sys) check.Result {
	v, err := s.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}} runtimes={{range $k, $v := .Runtimes}}{{$k}} {{end}}")
	if err != nil {
		return warn("docker not usable by this user (needed for training in phase 2)", "sudo usermod -aG docker $USER, then log out and in", firstLine(v))
	}
	// --gpus needs the NVIDIA Container Toolkit; a registered 'nvidia' runtime or nvidia-ctk shows it is installed.
	if !strings.Contains(v, "nvidia") {
		if ctk, err := s.Run(ctx, "nvidia-ctk", "--version"); err == nil {
			return pass("Docker " + v + "; " + firstLine(ctk))
		}
		return warn("Docker "+v+" without the NVIDIA Container Toolkit", "needed for GPU training (phase 2): see docs/PHASE2.md, 'GPU in Docker'")
	}
	return pass("Docker " + v)
}

func checkZenohd(ctx context.Context, s Sys) check.Result {
	act, _ := s.Run(ctx, "systemctl", "is-active", "zenohd")
	listening := s.Dial("127.0.0.1:7447", 2*time.Second) == nil
	ver, _ := s.Run(ctx, "/usr/local/bin/zenohd", "--version")
	detail := fmt.Sprintf("service=%s, listening on 7447=%v, %s", orDash(act), listening, orDash(ver))
	if act != "active" && listening {
		return conflict(check.Fail, "port 7447 is in use but the zenohd service is not running", "another program holds 7447 (an old zenohd or a container?): sudo ss -ltnp | grep 7447")
	}
	if act != "active" || !listening {
		return fail(detail, "run scripts/zbook/15-zenohd.sh, then: systemctl status zenohd; journalctl -u zenohd -n 50")
	}
	if !strings.Contains(ver, "v"+WantZenohC) {
		return warn(detail, "zenohd should be "+WantZenohC+" to match zenoh-c")
	}
	return pass(detail)
}

func checkRest(s Sys, url string) check.Result {
	if url == "" {
		url = DefaultRestURL
	}
	body, err := s.HTTPGet(url+"/@/*/router", 3*time.Second)
	if err != nil {
		if strings.HasPrefix(err.Error(), "HTTP ") {
			return conflict(check.Warn, "another program answers on "+url+" ("+err.Error()+"), so zenohd's REST plugin cannot use it",
				"find it with: sudo ss -ltnp | grep "+strings.TrimPrefix(url[strings.LastIndex(url, ":"):], ":")+" ; or move zenohd's http_port in /etc/rubblesar/zenohd.json5 and pass --rest to sar-doctor")
		}
		return warn("zenohd REST admin not reachable at "+url, "optional, used for diagnostics: check 'journalctl -u zenohd' for a REST plugin error", err.Error())
	}
	if !strings.Contains(body, `"@/`) {
		return conflict(check.Warn, "the service at "+url+" is not zenohd", "another program uses this port; move zenohd's http_port in /etc/rubblesar/zenohd.json5")
	}
	clients := strings.Count(body, `"whatami":"client"`)
	peers := strings.Count(body, `"whatami":"peer"`)
	ver := regexp.MustCompile(`"version":"(v[0-9.]+)`).FindStringSubmatch(body)
	v := "?"
	if ver != nil {
		v = ver[1]
	}
	return pass(fmt.Sprintf("zenohd %s, %d client and %d peer session(s) connected", v, clients, peers))
}

func checkFirewall(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "ufw", "status")
	if err != nil {
		if strings.Contains(out, "root") || strings.Contains(out, "permission") {
			return pass("ufw needs sudo to read (informational): sudo ufw status")
		}
		return pass("ufw not installed (no host firewall to open)")
	}
	if strings.Contains(out, "inactive") {
		return pass("ufw inactive")
	}
	if strings.Contains(out, "7447") {
		return pass("ufw active and 7447 is listed")
	}
	return fail("ufw active and 7447/tcp not allowed", "sudo ufw allow 7447/tcp (scripts/zbook/15-zenohd.sh does this)")
}

func checkPython(ctx context.Context, s Sys) check.Result {
	out, err := s.Run(ctx, "python3", "--version")
	if err != nil {
		return warn("python3 not found (needed from phase 1)", "sudo apt install python3 python3-venv")
	}
	return pass(out)
}

func checkReach(s Sys, ep string, jetson bool) check.Result {
	if ep == "" {
		return warn("no endpoint given", "pass --endpoint <zbook-ip>:7447 or --config")
	}
	if err := s.Dial(ep, 3*time.Second); err != nil {
		hint := "start zenohd: scripts/zbook/15-zenohd.sh"
		if jetson {
			hint = "check the ZBook IP in /etc/rubblesar/edge-eye.env, that zenohd runs on the ZBook, both are on the same Wi-Fi, and the ZBook firewall allows 7447/tcp"
		}
		return fail("cannot open TCP to "+ep, hint, err.Error())
	}
	return pass("TCP connection to " + ep + " succeeded")
}

func checkInstall(s Sys, jetson bool) check.Result {
	bins := []string{"sar-doctor", "sar-check", "sar-monitor", "scenario-tool", "sar-infer"}
	cfg := "/etc/rubblesar/sar.toml"
	if jetson {
		bins = []string{"sar-doctor", "sar-check", "edge-eye", "scenario-tool", "sar-infer"}
		cfg = "/etc/rubblesar/edge-eye.toml"
	}
	var missing []string
	for _, b := range bins {
		if _, err := s.Stat("/opt/rubblesar/bin/" + b); err != nil {
			missing = append(missing, "/opt/rubblesar/bin/"+b)
		}
	}
	if _, err := s.Stat(cfg); err != nil {
		missing = append(missing, cfg)
	}
	if len(missing) > 0 {
		return warn("not installed yet: "+strings.Join(missing, ", "), "expected before 30-install.sh; run it after a successful build")
	}
	return pass("binaries in /opt/rubblesar/bin, config " + cfg)
}

// ScenarioPath is where the setup scripts install the demo scenario.
const ScenarioPath = "/var/lib/rubblesar/scenarios/antakya-demo"

func checkScenario(s Sys, role string) check.Result {
	p := ScenarioPath + "/scenario.json"
	if _, err := s.Stat(p); err != nil {
		return warn("no scenario at "+ScenarioPath, "run scripts/"+role+"/60-scenario.sh (phase 1)")
	}
	if _, err := s.Stat(ScenarioPath + "/sites"); err != nil {
		return warn("scenario.json present but no rendered layers", "run scripts/"+role+"/60-scenario.sh again")
	}
	return pass(ScenarioPath + " (check it with: sar-check phase1)")
}

func checkORT(ctx context.Context, s Sys) check.Result {
	v, err := s.Run(ctx, "pkg-config", "--modversion", "rubblesar-onnxruntime")
	if err != nil {
		return warn("ONNX Runtime not installed (needed from phase 2)", "run scripts/<machine>/12-onnxruntime.sh, then 20-build.sh")
	}
	if v != WantORT {
		return warn("ONNX Runtime "+v+" installed, "+WantORT+" expected", "re-run scripts/<machine>/12-onnxruntime.sh, then 20-build.sh")
	}
	return pass("ONNX Runtime " + v + " (pkg-config rubblesar-onnxruntime)")
}

// ModelPath is where the phase 2 scripts install the current model.
const ModelPath = "/var/lib/rubblesar/models/current"

func checkModel(s Sys, role string) check.Result {
	b, err := s.ReadFile(ModelPath + "/model.json")
	if err != nil {
		if role == "jetson" {
			return warn("no model at "+ModelPath, "copy the model from the ZBook (85-push-model.sh) and run scripts/jetson/80-engine.sh --from <folder>")
		}
		return warn("no model at "+ModelPath, "run scripts/zbook/80-dataset.sh, 81-train.sh and 82-evaluate.sh")
	}
	var card struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &card) != nil || card.Name == "" {
		return warn(ModelPath+"/model.json is not readable", "install the model again (82-evaluate.sh / 80-engine.sh)")
	}
	if card.Name == "stub" {
		return warn("the installed model is the stand-in (stub), not a trained model", "install the trained model: scripts/zbook/82-evaluate.sh (ZBook) or scripts/jetson/80-engine.sh --from <folder> (Jetson)")
	}
	if role == "jetson" {
		if _, err := s.Stat(ModelPath + "/model.engine"); err != nil {
			return warn("model "+card.Name+" has no TensorRT engine", "run scripts/jetson/80-engine.sh --from <model folder>")
		}
		return pass("model " + card.Name + " with TensorRT engine (check it with: sar-check phase2)")
	}
	return pass("model " + card.Name + " (check it with: sar-check phase2)")
}

// checkEdgeP3 looks for an enabled [perception] section and survey drones in
// the installed edge-eye configuration.
func checkEdgeP3(s Sys) check.Result {
	b, err := s.ReadFile("/etc/rubblesar/edge-eye.toml")
	if err != nil {
		return warn("/etc/rubblesar/edge-eye.toml not readable", "installed by scripts/jetson/30-install.sh")
	}
	section, enabled, surveys := "", false, 0
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		switch {
		case strings.HasPrefix(l, "["):
			section = l
		case section == "[perception]" && strings.ReplaceAll(l, " ", "") == "enabled=true":
			enabled = true
		case section == "[[uav]]" && strings.ReplaceAll(l, " ", "") == `behaviour="survey"`:
			surveys++
		}
	}
	if !enabled {
		return warn("no enabled [perception] section: the drones fly but do not look for anyone (needed from phase 3)",
			"scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config (your file is backed up)")
	}
	return pass(fmt.Sprintf("perception enabled, %d survey drones", surveys))
}

// checkEdgeP4: phase 4 needs investigators that wait for tasks.
func checkEdgeP4(s Sys) check.Result {
	b, err := s.ReadFile("/etc/rubblesar/edge-eye.toml")
	if err != nil {
		return warn("/etc/rubblesar/edge-eye.toml not readable", "installed by scripts/jetson/30-install.sh")
	}
	standby := 0
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		if strings.ReplaceAll(l, " ", "") == `behaviour="standby"` {
			standby++
		}
	}
	if standby == 0 {
		return warn("no drone waits for investigation tasks (behaviour = \"standby\"; needed from phase 4)",
			"scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config (your file is backed up)")
	}
	return pass(fmt.Sprintf("%d drones on standby for investigations", standby))
}

func checkPlanner(ctx context.Context, s Sys) check.Result {
	act, _ := s.Run(ctx, "systemctl", "is-active", "mission-planner")
	if act != "active" {
		return warn("mission-planner service is "+orDash(act)+" (needed from phase 4: it sends the investigators)",
			"scripts/zbook/30-install.sh installs and starts it; 'journalctl -u mission-planner -n 30'")
	}
	return pass("active")
}

// RoadsPath is where scripts/zbook/32-roads.sh puts the demo incident's streets.
const RoadsPath = "/var/lib/rubblesar/roads/antakya-demo.json"

// MapPath is where scripts/zbook/36-map.sh puts the demo incident's offline map.
const MapPath = "/var/lib/rubblesar/www/map/antakya-demo/antakya-demo.mbtiles"

// checkWeb: sar-web serving, the offline map drawn.
func checkWeb(ctx context.Context, s Sys) check.Result {
	var miss []string
	if act, _ := s.Run(ctx, "systemctl", "is-active", "sar-web"); act != "active" {
		miss = append(miss, "sar-web service is "+orDash(act)+" (scripts/zbook/30-install.sh; 'journalctl -u sar-web -n 30')")
	} else if err := s.Dial("127.0.0.1:8090", 2*time.Second); err != nil {
		miss = append(miss, "sar-web not answering on 127.0.0.1:8090")
	}
	if _, err := s.Stat(MapPath); err != nil {
		miss = append(miss, "no offline map "+MapPath+" (scripts/zbook/36-map.sh)")
	}
	if len(miss) > 0 {
		return warn(strings.Join(miss, "; ")+" (needed from phase 6)", "docs/PHASE6.md, Steps")
	}
	return pass("sar-web on 8090, offline map installed")
}

// checkDispatch: taky (Docker), the street file and rescue-dispatch.
func checkDispatch(ctx context.Context, s Sys) check.Result {
	var miss []string
	if act, _ := s.Run(ctx, "systemctl", "is-active", "rubblesar-taky"); act != "active" {
		miss = append(miss, "TAK server rubblesar-taky is "+orDash(act)+" (scripts/zbook/34-taky.sh)")
	} else if err := s.Dial("127.0.0.1:8087", 2*time.Second); err != nil {
		miss = append(miss, "TAK server not answering on 127.0.0.1:8087 ('docker logs rubblesar-taky')")
	}
	if _, err := s.Stat(RoadsPath); err != nil {
		miss = append(miss, "no street file "+RoadsPath+" (scripts/zbook/32-roads.sh)")
	}
	if act, _ := s.Run(ctx, "systemctl", "is-active", "rescue-dispatch"); act != "active" {
		miss = append(miss, "rescue-dispatch service is "+orDash(act)+" (scripts/zbook/30-install.sh; 'journalctl -u rescue-dispatch -n 30')")
	}
	if len(miss) > 0 {
		return warn(strings.Join(miss, "; ")+" (needed from phase 5)", "docs/PHASE5.md, Install on the ZBook")
	}
	return pass("taky on 8087, streets installed, rescue-dispatch active")
}

func checkAnalyst(ctx context.Context, s Sys) check.Result {
	act, _ := s.Run(ctx, "systemctl", "is-active", "casualty-analyst")
	if act != "active" {
		return warn("casualty-analyst service is "+orDash(act)+" (needed from phase 3: it stores and acknowledges detections)",
			"scripts/zbook/30-install.sh installs and starts it; 'journalctl -u casualty-analyst -n 30'")
	}
	return pass("active")
}
