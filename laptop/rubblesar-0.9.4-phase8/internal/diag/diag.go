// Package diag defines RubbleSAR's error codes and the copy-and-paste report
// format used by every tool. Each error carries a stable code (E-AREA-NNN),
// a plain message and a hint, so a pasted error can be matched to its cause.
package diag

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Code is a stable error identifier, e.g. "E-BUS-010".
type Code string

// Catalogue entry: what the code means and what to try first.
type entry struct {
	Title string
	Hint  string
}

var catalogue = map[Code]entry{
	// Configuration
	"E-CFG-001": {"config file not found", "Check the --config path. Installed configs live in /etc/rubblesar/."},
	"E-CFG-002": {"config file is not valid TOML", "Fix the syntax at the line shown; compare with configs/ in the repository."},
	"E-CFG-003": {"config value invalid", "Correct the field named in the message; docs/ENVIRONMENT.md lists allowed values."},

	// Zenoh bus
	"E-BUS-001": {"zenoh session could not be opened", "Run sar-doctor; check that libzenohc 1.9.0 is installed (pkg-config --modversion zenohc)."},
	"E-BUS-002": {"binary built without zenoh support", "Rebuild with 'make build' (cgo + libzenohc); binaries built with -tags nozenoh cannot talk to the network."},
	"E-BUS-003": {"invalid key expression", "Incident and drone ids may only use a-z, 0-9 and '-', max 32 characters."},
	"E-BUS-004": {"zenoh publisher could not be declared", "Usually follows a closed or broken session; restart the program and check zenohd."},
	"E-BUS-005": {"zenoh subscriber could not be declared", "Usually follows a closed or broken session; restart the program and check zenohd."},
	"E-BUS-006": {"zenoh publish failed", "The session may be closing or the router unreachable; check zenohd and the network."},
	"E-BUS-010": {"cannot reach the zenoh router", "On the ZBook: 'systemctl status zenohd' and 'sudo ss -ltnp' (look for 7447). From the Jetson: 'nc -zv <zbook-ip> 7447'. Check the IP in the config and the ZBook firewall (ufw allow 7447/tcp)."},
	"E-BUS-011": {"zenoh rejected the session configuration", "The generated zenoh config is printed in the message; check [zenoh] in the TOML config."},
	"E-BUS-012": {"bus is closed", "The program is shutting down or the session failed earlier; see the first error in the log."},

	// Messages
	"E-MSG-001": {"message could not be decoded", "Sender and receiver are probably built from different versions; rebuild both from the same release."},
	"E-MSG-002": {"unsupported schema version", "Rebuild sender and receiver from the same release."},
	"E-MSG-003": {"incident id mismatch", "Both machines must use the same [site] incident_id."},
	"E-MSG-004": {"key and message disagree", "A drone id in the key differs from the payload; usually a configuration copy error."},

	// edge-eye
	"E-EDGE-001": {"admin API could not start", "Another program is using the admin port; change [admin] listen or stop the other edge-eye."},
	"E-EDGE-002": {"unknown drone id", "Use one of the ids listed in [[uav]] in edge-eye.toml."},
	"E-EDGE-003": {"invalid control request", "The control message was malformed; rebuild both sides from the same release."},
	"E-EDGE-004": {"edge-eye admin API not answering", "Is edge-eye running? 'systemctl status edge-eye' or start it in the foreground; check [admin] listen in edge-eye.toml."},
	"E-EDGE-005": {"scenario or survey setting not usable", "Check [site] scenario_dir and the [[uav]] survey_sites in edge-eye.toml against the installed scenario ('scenario-tool info --scenario /var/lib/rubblesar/scenarios/antakya-demo'); the scenario must match [site]."},
	"E-EDGE-006": {"detection queue folder not usable", "edge-eye stores unacknowledged detections in [outbox] dir; check free space ('df -h /var/lib/rubblesar') and that the rubblesar account can write there."},
	"E-EDGE-007": {"investigation task not accepted", "The task from mission-planner was invalid (target outside the incident area, height outside 5..120 m) or named a task the drone is not flying; see the message. Both sides must run the same release."},
	"E-ANL-001":  {"detection store not writable", "casualty-analyst writes to [analyst] store_dir/<incident>; check free space and ownership ('ls -ld /var/lib/rubblesar/detections')."},
	"E-ANL-002":  {"casualty-analyst status API", "Not answering: casualty-analyst is not running ('systemctl status casualty-analyst') or another program uses [analyst] listen (default 127.0.0.1:9102). Running but waiting: zenohd is not running on this ZBook ('systemctl status zenohd')."},
	"E-ANL-003":  {"casualty state file not usable", "casualty-analyst keeps casualty ids in casualties-state.json in its store folder; check that the rubblesar account can read and write it. Deleting it renumbers the casualties from the stored reports."},
	"E-PLN-001":  {"mission-planner status API", "Not answering: mission-planner is not running ('systemctl status mission-planner') or another program uses [planner] listen (default 127.0.0.1:9103). Running but waiting: zenohd is not running on this ZBook ('systemctl status zenohd')."},
	"E-PLN-002":  {"task state file not usable", "mission-planner keeps its tasks in [planner] state_dir/<incident>.json (default /var/lib/rubblesar/planner); check that the rubblesar account can read and write it. Deleting it forgets the tasks (casualties needing a look get new ones)."},
	"E-DSP-001":  {"rescue-dispatch status API", "Not answering: rescue-dispatch is not running ('systemctl status rescue-dispatch') or another program uses [dispatch] listen (default 127.0.0.1:9104). Running but waiting: zenohd is not running on this ZBook ('systemctl status zenohd')."},
	"E-DSP-002":  {"dispatch state file not usable", "rescue-dispatch keeps the casualties already reached in [dispatch] state_dir/<incident>.json (default /var/lib/rubblesar/dispatch); check that the rubblesar account can read and write it. Deleting it forgets which casualties were reached."},
	"E-TAK-001":  {"TAK server not reachable", "taky runs in Docker on the ZBook: 'sudo systemctl status rubblesar-taky', 'docker logs rubblesar-taky'; install it with scripts/zbook/34-taky.sh. Check [dispatch] tak_server (default 127.0.0.1:8087). rescue-dispatch retries on its own."},
	"E-TAK-002":  {"TAK server connection lost", "taky restarted or stopped ('sudo systemctl status rubblesar-taky'); rescue-dispatch reconnects and sends the whole picture again."},
	"E-TAK-004":  {"not a CoT event", "Something sent the TAK server data that is not Cursor-on-Target XML; it is skipped. If it repeats, paste the block with 'docker logs rubblesar-taky'."},
	"E-RTE-001":  {"street file missing or unreadable", "Download the streets once with scripts/zbook/32-roads.sh (needs internet; --synthetic writes a street grid without it), then 'sudo systemctl restart rescue-dispatch'. [dispatch] roads names the file."},
	"E-RTE-002":  {"team too far from any open street", "The team's position is more than 250 m from a street that is not closed by rubble; check its position (TAK teams: the device's GPS) and [dispatch] staging_lat/staging_lon."},
	"E-RTE-003":  {"casualty cut off by rubble", "No open street comes within 200 m of the casualty, or rubble closes every way in; the casualty stays waiting and its marker says so. 'rescue-dispatch roads' lists the closed streets; a smaller [dispatch] block_margin_m opens streets that only touch rubble."},

	// Acceptance checks (sar-check)
	"E-CHK-001": {"bus connection check failed", "See E-BUS-010; zenohd must be running and reachable."},
	"E-CHK-002": {"expected drones not seen", "On the Jetson: 'systemctl status edge-eye' or run it in the foreground; confirm it logs 'connected to zenoh'."},
	"E-CHK-003": {"telemetry rate out of tolerance", "CPU overload or Wi-Fi loss on the Jetson; check 'edge-eye' logs and the link budget."},
	"E-CHK-004": {"message integrity check failed", "Decode errors, wrong incident id or sequence gaps; rebuild both sides from the same release and check Wi-Fi quality."},
	"E-CHK-005": {"telemetry values implausible", "Check [site] centre and [[uav]] settings in edge-eye.toml."},
	"E-CHK-006": {"clock offset too large", "Enable time sync on both machines: 'sudo systemctl enable --now chrony' and see docs/ENVIRONMENT.md (Time)."},
	"E-CHK-007": {"control round trip failed", "ZBook to Jetson direction is broken; check that the Jetson subscribes (edge-eye logs) and zenohd routing."},
	"E-CHK-008": {"link throttle did not take effect", "edge-eye did not apply the budget; check its logs for E-EDGE codes."},
	"E-CHK-009": {"loopback self-test failed", "The local zenoh library cannot exchange messages with itself; re-run scripts/*/10-zenoh-c.sh and sar-doctor."},

	// Phase 1 acceptance checks (sar-check --suite phase1)
	"E-CHK-101": {"scenario check failed", "The scenario folder is missing pieces or does not meet the minimums; regenerate with scripts/zbook/60-scenario.sh."},
	"E-CHK-102": {"layer integrity check failed", "Layers are missing or were changed after rendering; re-run the render step or re-copy the folder (65-push-scenario.sh)."},
	"E-CHK-103": {"camera geometry check failed", "Pixel/ground projection is inconsistent; this is a code fault - paste the block."},
	"E-CHK-104": {"ground-truth visibility check failed", "A victim's exposed parts did not appear where expected in a rendered frame; paste the block."},
	"E-CHK-105": {"thermal plausibility check failed", "Temperatures in rendered thermal frames are outside the expected bands; check [environment] in the scenario config, else paste the block."},
	"E-CHK-106": {"movement check failed", "Moving victims did not change pose, or still victims changed; paste the block."},
	"E-CHK-107": {"frame rendering too slow", "On the Jetson use the highest power mode (MAXN SUPER on an Orin Nano with JetPack 6.2: 'sudo nvpmodel -m 2'; list the modes with 'sudo nvpmodel -p --verbose') and 'sudo jetson_clocks'; close other programs."},
	"E-CHK-108": {"scorer self-test failed", "Scoring logic is inconsistent; this is a code fault - paste the block."},

	// Scenario, rendering, virtual camera, scoring (scenario-tool)
	"E-SCN-001":  {"scenario file cannot be read", "Check the path; create one with 'scenario-tool generate' or scripts/zbook/60-scenario.sh."},
	"E-SCN-002":  {"scenario file is not valid JSON", "The file is damaged or was edited by hand; regenerate it."},
	"E-SCN-003":  {"scenario is inconsistent", "The message lists each problem; fix the scenario config and regenerate."},
	"E-SCN-004":  {"collapse sites could not be placed", "Lower [sites] count or min_gap_m, or raise max_from_centre_m in the scenario config."},
	"E-SCN-005":  {"OpenStreetMap building file unusable", "Re-run 'scenario-tool fetch-osm', or set osm_buildings = \"\" in the scenario config to use procedural sites."},
	"E-SCN-006":  {"OpenStreetMap download failed", "Needs internet on the ZBook (Overpass API). Retry later, or use procedural sites (osm_buildings = \"\")."},
	"E-RND-001":  {"layers could not be written", "Check free disk space ('df -h') and write permission on the output folder."},
	"E-RND-002":  {"layers missing or made from a different scenario", "Render again ('scenario-tool render') or re-copy the whole scenario folder from the ZBook (65-push-scenario.sh)."},
	"E-RND-003":  {"layer file damaged", "A checksum or decode check failed; re-copy or re-render the scenario folder."},
	"E-VCAM-001": {"camera pose invalid", "Altitude must be 2..400 m above ground and gimbal pitch -90..-20 degrees."},
	"E-VCAM-002": {"camera settings invalid", "Check width, height and hfov_deg (1..170) of the camera settings."},
	"E-SCR-001":  {"detections file unusable", "The detections JSON must follow docs/PHASE1.md (Scoring)."},

	// Phase 2 acceptance checks (sar-check --suite phase2)
	"E-CHK-201": {"model folder check failed", "Re-copy the model folder (scripts/zbook/85-push-model.sh) or re-run scripts/zbook/82-evaluate.sh."},
	"E-CHK-202": {"model cannot be run", "See the E-INF code in the evidence; on the Jetson re-run scripts/jetson/80-engine.sh."},
	"E-CHK-203": {"results disagree with the reference", "The evidence lists the detections that differ; paste the block."},
	"E-CHK-204": {"accuracy below the minimum", "Train longer or on more data (81-train.sh --epochs 150, or a larger dataset), then evaluate again; paste the block."},
	"E-CHK-205": {"geolocated detections find too few victims", "Usually follows low accuracy (E-CHK-204); paste the block."},
	"E-CHK-206": {"inference too slow on the Jetson", "Use the highest power mode (MAXN SUPER on an Orin Nano with JetPack 6.2: 'sudo nvpmodel -m 2'; list the modes with 'sudo nvpmodel -p --verbose') and run 'sudo jetson_clocks'; or train the smaller model (81-train.sh --model yolo11n-seg)."},

	// Phase 3: edge self-test (edge-eye selftest, Jetson) and acceptance (sar-check phase3, ZBook)
	"E-CHK-301": {"scenario or model not loadable", "Phase 1 scenario and phase 2 model must be installed: scripts/jetson/60-scenario.sh and 80-engine.sh; paste the block."},
	"E-CHK-302": {"pipeline too slow", "Use the highest power mode (MAXN SUPER: 'sudo nvpmodel -m 2', 'sudo jetson_clocks'); stop other GPU programs; paste the block."},
	"E-CHK-303": {"reports missing or misplaced", "Positions should be within a few metres of the true parts; paste the block (with the stand-in model this fails by design)."},
	"E-CHK-304": {"store-and-forward lost or delayed reports", "Paste the block; this is a software fault, not a setting."},
	"E-CHK-305": {"detection queue on disk not working", "Check free space and permissions of the temporary folder and of [outbox] dir; paste the block."},
	"E-CHK-306": {"movement detection unsure for this model", "The block shows how much the outlines of a moving and a still victim changed while hovering; set [perception] movement_change between the two (the hint suggests a value) and run 'edge-eye selftest' again; paste the block."},
	"E-CHK-311": {"drones or perception not running", "On the Jetson: 'edge-eye status' must show the model and frames; install the phase 3 config (30-install.sh --force-config) and the engine (80-engine.sh); casualty-analyst must run on the ZBook."},
	"E-CHK-312": {"malformed detection packets", "Rebuild and install both machines from the same release; paste the block."},
	"E-CHK-313": {"link budget not respected", "Paste the block with the per-window figures; check that nothing else publishes on the drones' keys."},
	"E-CHK-314": {"detections too far from the true body parts", "Paste the block. With the stand-in model this fails by design. Otherwise the per-class lines show which parts are off, and the count of reports more than 10 m from any true part shows false detections."},
	"E-CHK-315": {"few victims found", "Depends on the model and the survey; paste the block. A better model (phase 2 retraining) raises it."},
	"E-CHK-316": {"detections lost around the outage", "Store-and-forward fault; paste the block and the Jetson's 'journalctl -u edge-eye --since \"15 min ago\"'."},
	"E-CHK-317": {"detections arrive late", "Check the Wi-Fi link and the clocks ('chronyc tracking' on the Jetson); paste the block."},
	"E-CHK-318": {"Jetson pipeline slow or frames dropped", "Use the highest power mode and jetson_clocks; lower [perception] frame_hz; paste the block."},
	"E-CHK-411": {"drones or services not ready for phase 4", "On the Jetson: phase 4 config (30-install.sh --force-config: a scout surveying, investigators on standby) and 'edge-eye status'; on the ZBook: 'casualty-analyst status' and 'mission-planner status' must answer (30-install.sh). 'The run could not be completed': the message names the cause; a reset needs casualty-analyst (account rubblesar) to write <store_dir>/<incident>/archive/ (re-run scripts/zbook/30-install.sh to fix ownership)."},
	"E-CHK-412": {"casualty records do not match the victims", "Paste the block: it lists victims with no record, with extra records, or merged into a neighbour's, and records near no victim (decoys)."},
	"E-CHK-413": {"casualty positions far from the victims", "Paste the block; compare with the phase 3 position check (P3-04) of the same model."},
	"E-CHK-414": {"casualties that needed a close look did not get one", "'mission-planner status' shows why tasks wait (battery, drones offline, refused commands); paste the block and 'journalctl -u mission-planner -n 50'."},
	"E-CHK-415": {"investigations not flown as planned", "'edge-eye status' on the Jetson shows each drone's task; 'mission-planner status' why tasks failed; paste the block."},
	"E-CHK-416": {"triage differs from the scenario's ground truth", "Reported for medical review (thresholds await sign-off, decision D9); the block lists each victim's expected and given colour with the reason."},
	"E-CHK-417": {"movement close up not as expected", "Run 'edge-eye selftest' on the Jetson (ST-06) and set [perception] movement_change as its hint suggests; paste both blocks."},
	"E-CHK-418": {"radio budget exceeded or reports lost", "Paste the block with the per-window figures and 'casualty-analyst status'."},

	// Phase 5: acceptance (sar-check phase5, ZBook)
	"E-CHK-511": {"TAK server, rescue-dispatch or the services not ready", "The line names what is missing: taky ('sudo systemctl status rubblesar-taky', scripts/zbook/34-taky.sh), rescue-dispatch ('rescue-dispatch status', scripts/zbook/30-install.sh), the street file (scripts/zbook/32-roads.sh), the CHECK-1 team in [[dispatch.team]]; for the drones and the other services see E-CHK-411."},
	"E-CHK-512": {"TAK map incomplete or wrong", "Markers missing, in the wrong colour or place, or drones and teams missing: 'rescue-dispatch status' (TAK link, events sent and queued), 'docker logs rubblesar-taky'; paste the block."},
	"E-CHK-513": {"routes cross rubble or are missing", "Paste the block; 'rescue-dispatch roads' shows the street network and the streets closed per rubble area."},
	"E-CHK-514": {"dispatch order wrong or casualties not reached", "Paste the block and 'rescue-dispatch status' (teams still busy, casualties with no route). With the time limit reached, more teams ([[dispatch.team]]) or a higher [dispatch] time_scale finish sooner."},
	"E-CHK-515": {"the TAK team loop did not complete", "sar-check plays CHECK-1 on the TAK server: it needs its route line and chat over TAK and rescue-dispatch must see its position reports and its \"done\". Paste the block with 'journalctl -u rescue-dispatch -n 50' and 'docker logs rubblesar-taky'."},
	"E-CHK-516": {"chat messages missing", "Dispatch and arrival messages go to All Chat Rooms through the TAK server; check 'rescue-dispatch status' for reconnects and dropped events; paste the block."},

	// Phase 7: the live operator view (sar-web /ops/) and its acceptance (sar-check phase7)
	"E-WEB-002": {"sar-web cannot follow the bus", "sar-web subscribes to the drones' and the services' messages on zenohd: 'systemctl status zenohd', then 'sudo systemctl restart sar-web'; the live view's header says what it is waiting for."},
	"E-WEB-003": {"replay not available", "The run named is not in the incident's reports folder ('rubblesar report --list'), or no replay is loaded yet ('rubblesar demo replay' loads one)."},
	"E-CHK-701": {"operator view not served or not connected (P7-01)", "sar-web 0.9 or later must answer on [web] listen with the live view connected to zenohd: 'sar-web status', 'sudo systemctl restart sar-web'; WARN: no offline map yet (scripts/zbook/36-map.sh), or sar-web was started before [[dispatch.team]] changed (sudo systemctl restart sar-web)."},
	"E-CHK-702": {"live picture disagrees with the services (P7-02)", "The block lists the samples where the picture and casualty-analyst, mission-planner or rescue-dispatch differed for more than 4.5 s; paste it with 'journalctl -u sar-web -n 50'."},
	"E-CHK-703": {"event stream not delivered (P7-03)", "A proxy or firewall between the browser and the ZBook may buffer server-sent events; on the ZBook: 'curl -N http://127.0.0.1:8090/v1/stream' must print events every few seconds."},
	"E-CHK-704": {"drone crops not served (P7-04)", "sar-web reads casualty-analyst's store ([analyst] store_dir/<incident>); the rubblesar account must be able to read it (re-run scripts/zbook/30-install.sh)."},
	"E-CHK-705": {"timeline disagrees with the after-action report (P7-05)", "Paste the block: it lists casualties found, sent or freed in the report but missing from the live timeline."},

	// Phase 8: the demo director (rubblesar demo) and its acceptance (sar-check phase8)
	"E-DIR-001": {"storyboard not usable", "The message names the chapter and setting; compare with the built-in one ('rubblesar demo init' writes a copy) or set [demo] storyboard = \"\"."},
	"E-DIR-002": {"demo settings not usable", "Fix the setting named in ~/.config/rubblesar/demo.toml ('rubblesar demo init' writes a commented one next to it if it is missing)."},
	"E-DIR-003": {"a recorder cannot be reached or used", "OBS: running, Tools > WebSocket Server Settings enabled on port 4455 with the password in demo.toml (close code 4009 = wrong password), Windows firewall open for the ZBook, not already recording, the scene present ('rubblesar demo obs-setup'). A command recorder: the program installed (scrcpy: 'adb devices' lists the phone)."},
	"E-DIR-004": {"the drone's frame cannot be rendered again", "The run must have been recorded by this release (frame poses in events.jsonl) against the installed scenario; 'rubblesar demo media --frame ID' tries another frame."},
	"E-DIR-005": {"sar-web or the presenter page not reachable", "'sudo systemctl status sar-web' ([demo] web is its address on this ZBook); a presenter page must be open: OBS's presenter scene ('rubblesar demo obs-setup') or a browser at /ops/?present=1."},
	"E-DIR-006": {"take not found or incomplete", "'rubblesar demo takes' lists them; a rehearsal has no recording; for the rough cut, copy the recordings from other machines to this ZBook and give --clip NAME=FILE (or --main FILE)."},
	"E-DIR-007": {"ffmpeg or the text-to-speech command failed", "'sudo apt install ffmpeg' (scripts/zbook/92-demo-setup.sh); the message ends with ffmpeg's last lines. Text to speech: [demo] tts, run it by hand with a short text."},
	"E-CHK-801": {"demo settings or storyboard not usable (P8-01)", "See E-DIR-002 and E-DIR-001; 'rubblesar demo check' prints the same."},
	"E-CHK-802": {"no recorded run, or it lacks the storyboard's beats (P8-02)", "'rubblesar demo live' records a mission with every beat (staged link events included); the phase 6/7 run works too, without the staged budget cut."},
	"E-CHK-803": {"the drone's frame was not rendered or the model not run (P8-03)", "See E-DIR-004. WARN, no model: install ONNX Runtime and the model on the ZBook (12-onnxruntime.sh, 82-evaluate.sh). WARN, not exact: [demo] edge_seed must equal edge-eye's seed on the Jetson."},
	"E-CHK-804": {"narration not filled in from the run (P8-04)", "'rubblesar demo script' prints every chapter with its warnings and the length."},
	"E-CHK-805": {"sar-web's replay or the presenter page not working (P8-05)", "See E-DIR-005 and E-WEB-003; sar-web must be this release's (scripts/zbook/30-install.sh)."},
	"E-CHK-806": {"a recorder failed its test recording (P8-06)", "See E-DIR-003; 'rubblesar demo test-record' repeats the test with its output."},
	"E-CHK-807": {"rough-cut tools missing (P8-07)", "'sudo apt install ffmpeg' (scripts/zbook/92-demo-setup.sh); the text-to-speech program in [demo] tts, or set it to \"\"."},
	"E-CHK-808": {"the rehearsal did not complete (P8-08)", "'rubblesar demo replay --rehearse' shows each chapter as it goes; a presenter page must be open."},

	// Dataset and inference (scenario-tool dataset, sar-infer)
	"E-DS-001":  {"dataset or test-frame list missing", "Build the dataset with scripts/zbook/80-dataset.sh; on the Jetson the list comes inside the model folder (testset.json)."},
	"E-DS-002":  {"test frames belong to another scenario", "The installed scenario changed after the dataset was built: re-run 80-dataset.sh (and 81-train.sh / 82-evaluate.sh), or reinstall the original scenario."},
	"E-INF-001": {"model file missing or damaged", "Re-copy the model folder (85-push-model.sh) or re-run 81-train.sh and 82-evaluate.sh on the ZBook."},
	"E-INF-002": {"inference back end not in this build", "Install ONNX Runtime (scripts/*/12-onnxruntime.sh) or, on the Jetson, the TensorRT headers ('sudo apt install libnvinfer-dev'), then re-run the 20-build script."},
	"E-INF-003": {"model does not look like the RubbleSAR body-part model", "Use a model trained by scripts/zbook/81-train.sh (5 classes: hand, arm, leg, head, torso; 640 px input)."},
	"E-INF-004": {"inference failed", "Paste the block. On the Jetson check memory with 'tegrastats' and that no other program uses the GPU."},
	"E-INF-005": {"TensorRT engine cannot be loaded", "An engine only works with the TensorRT version and GPU it was built on: re-run scripts/jetson/80-engine.sh."},
	"E-INF-006": {"the GPU cannot be used by this account", "On a Jetson the account needs the groups that own /dev/nvmap and /dev/nvgpu/igpu0 ('ls -l /dev/nvmap /dev/nvgpu/igpu0'). For the edge-eye service, re-run scripts/jetson/30-install.sh: it adds the account 'rubblesar' to them and tests the GPU as that account."},

	// Environment (sar-doctor)
	"E-DOC-001": {"environment check failed", "Follow the hint on the failing line, then re-run sar-doctor."},
	"E-DOC-002": {"a port RubbleSAR needs is used by another program", "Find the program with 'sudo ss -ltnp' and stop it, or move the RubbleSAR port as the hint says."},
}

// Lookup returns the title and hint for a code.
func Lookup(c Code) (title, hint string, ok bool) {
	e, ok := catalogue[c]
	return e.Title, e.Hint, ok
}

// Codes returns every known code, sorted (used by docs and tests).
func Codes() []Code {
	out := make([]Code, 0, len(catalogue))
	for c := range catalogue {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Error is an error with a stable code.
type Error struct {
	Code   Code
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	title, _, _ := Lookup(e.Code)
	var b strings.Builder
	b.WriteString(string(e.Code))
	b.WriteString(": ")
	if e.Detail != "" {
		b.WriteString(e.Detail)
	} else {
		b.WriteString(title)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// Hint returns the remedy text for this error's code.
func (e *Error) Hint() string {
	_, h, _ := Lookup(e.Code)
	return h
}

// New builds a coded error.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Wrap builds a coded error around a cause.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...), Cause: cause}
}

// CodeOf extracts the first code in an error chain ("" if none).
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// HintOf returns the hint for the first coded error in the chain.
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint()
	}
	return ""
}

// Report collects lines for the copy-and-paste block printed on failure.
type Report struct {
	Tool    string
	Version string
	Lines   []string
}

// Add appends a formatted line.
func (r *Report) Add(format string, args ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, args...))
}

// AddError appends an error with its code and hint.
func (r *Report) AddError(prefix string, err error) {
	r.Add("%s%v", prefix, err)
	if h := HintOf(err); h != "" {
		r.Add("%s  hint: %s", strings.Repeat(" ", len(prefix)), h)
	}
}

// Render returns the full block, framed so it is easy to select and paste.
func (r *Report) Render() string {
	host, _ := os.Hostname()
	var b strings.Builder
	b.WriteString("\n=============== COPY FROM HERE: RubbleSAR report ===============\n")
	fmt.Fprintf(&b, "tool:    %s\n", r.Version)
	fmt.Fprintf(&b, "host:    %s (%s/%s)\n", host, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "time:    %s\n", time.Now().UTC().Format(time.RFC3339))
	for _, l := range r.Lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("================ COPY TO HERE ==================================\n")
	return b.String()
}
