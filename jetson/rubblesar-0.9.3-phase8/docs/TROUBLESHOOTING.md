# Troubleshooting: error codes

Every RubbleSAR error carries a code. Find it here for the first thing to try; if that does not fix it, paste the whole `COPY FROM HERE … COPY TO HERE` block into the chat. `scripts/collect-diagnostics.sh` produces such a block on demand.

## Program errors (edge-eye, sar-check, sar-monitor, sar-doctor, scenario-tool)

| Code | Meaning | First thing to try |
|---|---|---|
| E-BUS-001 | zenoh session could not be opened | Run sar-doctor; check that libzenohc 1.9.0 is installed (pkg-config --modversion zenohc). |
| E-BUS-002 | binary built without zenoh support | Rebuild with 'make build' (cgo + libzenohc); binaries built with -tags nozenoh cannot talk to the network. |
| E-BUS-003 | invalid key expression | Incident and drone ids may only use a-z, 0-9 and '-', max 32 characters. |
| E-BUS-004 | zenoh publisher could not be declared | Usually follows a closed or broken session; restart the program and check zenohd. |
| E-BUS-005 | zenoh subscriber could not be declared | Usually follows a closed or broken session; restart the program and check zenohd. |
| E-BUS-006 | zenoh publish failed | The session may be closing or the router unreachable; check zenohd and the network. |
| E-BUS-010 | cannot reach the zenoh router | On the ZBook: 'systemctl status zenohd' and 'sudo ss -ltnp' (look for 7447). From the Jetson: 'nc -zv <zbook-ip> 7447'. Check the IP in the config and the ZBook firewall (ufw allow 7447/tcp). |
| E-BUS-011 | zenoh rejected the session configuration | The generated zenoh config is printed in the message; check [zenoh] in the TOML config. |
| E-BUS-012 | bus is closed | The program is shutting down or the session failed earlier; see the first error in the log. |
| E-CFG-001 | config file not found | Check the --config path. Installed configs live in /etc/rubblesar/. |
| E-CFG-002 | config file is not valid TOML | Fix the syntax at the line shown; compare with configs/ in the repository. |
| E-CFG-003 | config value invalid | Correct the field named in the message; docs/ENVIRONMENT.md lists allowed values. |
| E-CHK-001 | bus connection check failed | See E-BUS-010; zenohd must be running and reachable. |
| E-CHK-002 | expected drones not seen | On the Jetson: 'systemctl status edge-eye' or run it in the foreground; confirm it logs 'connected to zenoh'. |
| E-CHK-003 | telemetry rate out of tolerance | CPU overload or Wi-Fi loss on the Jetson; check 'edge-eye' logs and the link budget. |
| E-CHK-004 | message integrity check failed | Decode errors, wrong incident id or sequence gaps; rebuild both sides from the same release and check Wi-Fi quality. |
| E-CHK-005 | telemetry values implausible | Check [site] centre and [[uav]] settings in edge-eye.toml. |
| E-CHK-006 | clock offset too large | Enable time sync on both machines: 'sudo systemctl enable --now chrony' and see docs/ENVIRONMENT.md (Time). |
| E-CHK-007 | control round trip failed | ZBook to Jetson direction is broken; check that the Jetson subscribes (edge-eye logs) and zenohd routing. |
| E-CHK-008 | link throttle did not take effect | edge-eye did not apply the budget; check its logs for E-EDGE codes. |
| E-CHK-009 | loopback self-test failed | The local zenoh library cannot exchange messages with itself; re-run scripts/*/10-zenoh-c.sh and sar-doctor. |
| E-DOC-001 | environment check failed | Follow the hint on the failing line, then re-run sar-doctor. |
| E-DOC-002 | a port RubbleSAR needs is used by another program | Find the program with 'sudo ss -ltnp' and stop it, or move the RubbleSAR port as the hint says. |
| E-EDGE-001 | admin API could not start | Another program is using the admin port; change [admin] listen or stop the other edge-eye. |
| E-EDGE-002 | unknown drone id | Use one of the ids listed in [[uav]] in edge-eye.toml. |
| E-EDGE-003 | invalid control request | The control message was malformed; rebuild both sides from the same release. |
| E-EDGE-004 | edge-eye admin API not answering | Is edge-eye running? 'systemctl status edge-eye' or start it in the foreground; check [admin] listen in edge-eye.toml. |
| E-EDGE-005 | scenario or survey setting not usable | Check [site] scenario_dir and the [[uav]] survey_sites in edge-eye.toml against the installed scenario ('scenario-tool info --scenario /var/lib/rubblesar/scenarios/antakya-demo'); the scenario must match [site]. |
| E-EDGE-006 | detection queue folder not usable | edge-eye stores unacknowledged detections in [outbox] dir; check free space ('df -h /var/lib/rubblesar') and that the rubblesar account can write there. |
| E-EDGE-007 | investigation task not accepted by the drone | The message says why (target outside the incident area, height outside 5–120 m, or no such task to cancel). Both machines must run the same release |
| E-ANL-001 | detection store not writable | casualty-analyst writes to [analyst] store_dir/<incident>; check free space and ownership ('ls -ld /var/lib/rubblesar/detections'). |
| E-ANL-002 | casualty-analyst status API | Not answering: casualty-analyst is not running ('systemctl status casualty-analyst') or another program uses [analyst] listen (default 127.0.0.1:9102). Running but waiting: zenohd is not running on this ZBook ('systemctl status zenohd'). |
| E-ANL-003 | casualty state file not usable | casualty-analyst keeps casualty ids in `casualties-state.json` in its store folder; check that the `rubblesar` account can read and write it. Deleting it renumbers the casualties from the stored reports |
| E-PLN-001 | mission-planner status API | Not answering: mission-planner is not running (`systemctl status mission-planner`) or another program uses [planner] listen (default 127.0.0.1:9103). Running but waiting: zenohd is not running on this ZBook (`systemctl status zenohd`). |
| E-PLN-002 | task state file not usable | mission-planner keeps its tasks in [planner] `state_dir/<incident>.json` (default `/var/lib/rubblesar/planner`); check that the `rubblesar` account can read and write it. Deleting it forgets the tasks (casualties needing a look get new ones). |
| E-DSP-001 | rescue-dispatch status API | Not answering: rescue-dispatch is not running (`systemctl status rescue-dispatch`) or another program uses [dispatch] listen (default 127.0.0.1:9104). Running but waiting: zenohd is not running on this ZBook (`systemctl status zenohd`). |
| E-DSP-002 | dispatch state file not usable | rescue-dispatch keeps the casualties already reached in [dispatch] `state_dir/<incident>.json` (default `/var/lib/rubblesar/dispatch`); check that the `rubblesar` account can read and write it. Deleting it forgets which casualties were reached. |
| E-TAK-001 | TAK server not reachable | taky runs in Docker on the ZBook: `sudo systemctl status rubblesar-taky`, `docker logs rubblesar-taky`; install it with `scripts/zbook/34-taky.sh`. Check [dispatch] tak_server (default 127.0.0.1:8087). rescue-dispatch retries on its own. From a phone or PC: same network, TCP 8087 open (`sudo ufw status`), protocol TCP not SSL. |
| E-TAK-002 | TAK server connection lost | taky restarted or stopped (`sudo systemctl status rubblesar-taky`; it restarts daily on purpose); rescue-dispatch reconnects and sends the whole picture again. |
| E-TAK-004 | not a CoT event | Something sent the TAK server data that is not Cursor-on-Target XML; it is skipped. If it repeats, paste the block with `docker logs rubblesar-taky`. |
| E-RTE-001 | street file missing or unreadable | Download the streets once with `scripts/zbook/32-roads.sh` (needs internet; `--synthetic` writes a street grid without it), then `sudo systemctl restart rescue-dispatch`. [dispatch] roads names the file. |
| E-RTE-002 | team too far from any open street | The team's position is more than 250 m from a street that is not closed by rubble; check its position (TAK teams: the device's GPS) and [dispatch] staging_lat/staging_lon. |
| E-RTE-003 | casualty cut off by rubble | No open street comes within 200 m of the casualty, or rubble closes every way in; the casualty stays waiting and its marker says so. `rescue-dispatch roads` lists the closed streets; a smaller [dispatch] block_margin_m opens streets that only touch rubble. |
| E-MSG-001 | message could not be decoded | Sender and receiver are probably built from different versions; rebuild both from the same release. |
| E-MSG-002 | unsupported schema version | Rebuild sender and receiver from the same release. |
| E-MSG-003 | incident id mismatch | Both machines must use the same [site] incident_id. |
| E-MSG-004 | key and message disagree | A drone id in the key differs from the payload; usually a configuration copy error. |

## Scenario, imagery and scoring errors (scenario-tool, phase 1)

| Code | Meaning | First thing to try |
|---|---|---|
| E-SCN-001 | scenario file cannot be read | Check the path; create one with 'scenario-tool generate' or scripts/zbook/60-scenario.sh. |
| E-SCN-002 | scenario file is not valid JSON | The file is damaged or was edited by hand; regenerate it. |
| E-SCN-003 | scenario is inconsistent | The message lists each problem; fix the scenario config and regenerate. |
| E-SCN-004 | collapse sites could not be placed | Lower [sites] count or min_gap_m, or raise max_from_centre_m in the scenario config. |
| E-SCN-005 | OpenStreetMap building file unusable | Re-run 'scenario-tool fetch-osm', or set osm_buildings = "" in the scenario config to use procedural sites. |
| E-SCN-006 | OpenStreetMap download failed | Needs internet on the ZBook (Overpass API). Retry later, or use procedural sites (osm_buildings = ""). |
| E-RND-001 | layers could not be written | Check free disk space ('df -h') and write permission on the output folder. |
| E-RND-002 | layers missing or made from a different scenario | Render again ('scenario-tool render') or re-copy the whole scenario folder from the ZBook (65-push-scenario.sh). |
| E-RND-003 | layer file damaged | A checksum or decode check failed; re-copy or re-render the scenario folder. |
| E-VCAM-001 | camera pose invalid | Altitude must be 2..400 m above ground and gimbal pitch -90..-20 degrees. |
| E-VCAM-002 | camera settings invalid | Check width, height and hfov_deg (1..170) of the camera settings. |
| E-SCR-001 | detections file unusable | The detections JSON must follow docs/PHASE1.md (Scoring). |
| E-CHK-101 | scenario check failed | The scenario folder is missing pieces or does not meet the minimums; regenerate with scripts/zbook/60-scenario.sh. |
| E-CHK-102 | layer integrity check failed | Layers are missing or were changed after rendering; re-run the render step or re-copy the folder (65-push-scenario.sh). |
| E-CHK-103 | camera geometry check failed | Pixel/ground projection is inconsistent; this is a code fault - paste the block. |
| E-CHK-104 | ground-truth visibility check failed | A victim's exposed parts did not appear where expected in a rendered frame; paste the block. |
| E-CHK-105 | thermal plausibility check failed | Temperatures in rendered thermal frames are outside the expected bands; check [environment] in the scenario config, else paste the block. |
| E-CHK-106 | movement check failed | Moving victims did not change pose, or still victims changed; paste the block. |
| E-CHK-107 | frame rendering too slow | On the Jetson use the highest power mode (MAXN SUPER on an Orin Nano with JetPack 6.2: 'sudo nvpmodel -m 2'; list the modes with 'sudo nvpmodel -p --verbose') and 'sudo jetson_clocks'; close other programs. |
| E-CHK-108 | scorer self-test failed | Scoring logic is inconsistent; this is a code fault - paste the block. |

## Model, dataset and inference errors (phase 2)

| Code | Meaning | First thing to try |
|---|---|---|
| E-DS-001 | dataset or test-frame list missing | Build the dataset with scripts/zbook/80-dataset.sh; on the Jetson the list comes inside the model folder (testset.json). |
| E-DS-002 | test frames belong to another scenario | The installed scenario changed after the dataset was built: re-run 80-dataset.sh (and 81-train.sh / 82-evaluate.sh), or reinstall the original scenario. |
| E-INF-001 | model file missing or damaged | Re-copy the model folder (85-push-model.sh) or re-run 81-train.sh and 82-evaluate.sh on the ZBook. |
| E-INF-002 | inference back end not in this build | Install ONNX Runtime (scripts/*/12-onnxruntime.sh) or, on the Jetson, the TensorRT headers ('sudo apt install libnvinfer-dev'), then re-run the 20-build script. |
| E-INF-003 | model does not look like the RubbleSAR body-part model | Use a model trained by scripts/zbook/81-train.sh (5 classes: hand, arm, leg, head, torso; 640 px input). |
| E-INF-004 | inference failed | Paste the block. On the Jetson check memory with 'tegrastats' and that no other program uses the GPU. |
| E-INF-005 | TensorRT engine cannot be loaded | An engine only works with the TensorRT version and GPU it was built on: re-run scripts/jetson/80-engine.sh. |
| E-INF-006 | the GPU cannot be used by this account (CUDA does not start) | On a Jetson the account needs the groups that own /dev/nvmap and /dev/nvgpu/igpu0 (`ls -l /dev/nvmap /dev/nvgpu/igpu0`). For the edge-eye service, re-run `scripts/jetson/30-install.sh`: it adds the account `rubblesar` to them and tests the GPU as that account |
| E-CHK-201 | model folder check failed | Re-copy the model folder (scripts/zbook/85-push-model.sh) or re-run scripts/zbook/82-evaluate.sh. |
| E-CHK-202 | model cannot be run | See the E-INF code in the evidence; on the Jetson re-run scripts/jetson/80-engine.sh. |
| E-CHK-203 | results disagree with the reference | The evidence lists the detections that differ; paste the block. |
| E-CHK-204 | accuracy below the minimum | Train longer or on more data (81-train.sh --epochs 150, or a larger dataset), then evaluate again; paste the block. |
| E-CHK-205 | geolocated detections find too few victims | Usually follows low accuracy (E-CHK-204); paste the block. |
| E-CHK-206 | inference too slow on the Jetson | Use the highest power mode (MAXN SUPER on an Orin Nano with JetPack 6.2: 'sudo nvpmodel -m 2'; list the modes with 'sudo nvpmodel -p --verbose') and run 'sudo jetson_clocks'; or train the smaller model (81-train.sh --model yolo11n-seg). |

### Training errors (printed by the training container)

| Code | Meaning | First thing to try |
|---|---|---|
| E-TRN-001 | Docker cannot see the GPU | `docker run --rm --gpus all ubuntu nvidia-smi` must list the Quadro; if not, set up the NVIDIA Container Toolkit (docs/PHASE2.md, GPU in Docker) |
| E-TRN-002 | dataset missing or incomplete | Run scripts/zbook/80-dataset.sh |
| E-TRN-003 | training stopped with an error | Paste the block (it holds the traceback) |
| E-TRN-004 | GPU out of memory even at batch 4 | Close other GPU programs (`nvidia-smi`), retry |
| E-TRN-005 | ONNX export failed | Paste the block |
| E-TRN-006 | starting weights missing | `scripts/zbook/81-train.sh --rebuild` |
| E-TRN-007 | parity predictions failed | Paste the block |

## Edge pipeline errors (phase 3)

`edge-eye selftest` (Jetson, ST-01 to ST-05) and `sar-check phase3` (ZBook, P3-01 to P3-08).

| Code | Meaning | What to do |
|---|---|---|
| E-CHK-301 | scenario or model not loadable | Phase 1 scenario and phase 2 model must be installed: scripts/jetson/60-scenario.sh and 80-engine.sh; paste the block. |
| E-CHK-302 | pipeline too slow | Use the highest power mode (MAXN SUPER: 'sudo nvpmodel -m 2', 'sudo jetson_clocks'); stop other GPU programs; paste the block. |
| E-CHK-303 | reports missing or misplaced | Positions should be within a few metres of the true parts; paste the block (with the stand-in model this fails by design). |
| E-CHK-304 | store-and-forward lost or delayed reports | Paste the block; this is a software fault, not a setting. |
| E-CHK-305 | detection queue on disk not working | Check free space and permissions of the temporary folder and of [outbox] dir; paste the block. |
| E-CHK-306 | movement detection unsure for this model | The block shows how much the outlines of a moving and a still victim changed while hovering; set [perception] `movement_change` between the two (the hint suggests a value) and run `edge-eye selftest` again; paste the block. |
| E-CHK-311 | drones or perception not running | On the Jetson: 'edge-eye status' must show the model and frames; install the phase 3 config (30-install.sh --force-config) and the engine (80-engine.sh); casualty-analyst must run on the ZBook. |
| E-CHK-312 | malformed detection packets | Rebuild and install both machines from the same release; paste the block. |
| E-CHK-313 | link budget not respected | Paste the block with the per-window figures; check that nothing else publishes on the drones' keys. |
| E-CHK-314 | detections too far from the true body parts | Paste the block. With the stand-in model this fails by design. Otherwise the per-class lines show which parts are off, and the count of reports more than 10 m from any true part shows false detections. |
| E-CHK-315 | few victims found | Depends on the model and the survey; paste the block. A better model (phase 2 retraining) raises it. |
| E-CHK-316 | detections lost around the outage | Store-and-forward fault; paste the block and the Jetson's 'journalctl -u edge-eye --since "15 min ago"'. |
| E-CHK-317 | detections arrive late | Check the Wi-Fi link and the clocks ('chronyc tracking' on the Jetson); paste the block. |
| E-CHK-318 | Jetson pipeline slow or frames dropped | Use the highest power mode and jetson_clocks; lower [perception] frame_hz; paste the block. |
| E-CHK-411 | drones or services not ready for phase 4 | On the Jetson: phase 4 config (30-install.sh --force-config: a scout surveying, investigators on standby) and 'edge-eye status'; on the ZBook: 'casualty-analyst status' and 'mission-planner status' must answer (30-install.sh). *The run could not be completed*: the message names the cause; a reset needs casualty-analyst (account `rubblesar`) to write `<store_dir>/<incident>/archive/` (re-run `scripts/zbook/30-install.sh` to fix ownership). |
| E-CHK-412 | casualty records do not match the victims | Paste the block: it lists victims with no record, with extra records, or merged into a neighbour's, and records near no victim (decoys). |
| E-CHK-413 | casualty positions far from the victims | Paste the block; compare with the phase 3 position check (P3-04) of the same model. |
| E-CHK-414 | casualties that needed a close look did not get one | 'mission-planner status' shows why tasks wait (battery, drones offline, refused commands); paste the block and 'journalctl -u mission-planner -n 50'. |
| E-CHK-415 | investigations not flown as planned | 'edge-eye status' on the Jetson shows each drone's task; 'mission-planner status' why tasks failed; paste the block. |
| E-CHK-416 | triage differs from the scenario's ground truth | Reported for medical review (thresholds await sign-off, decision D9); the block lists each victim's expected and given colour with the reason. |
| E-CHK-417 | movement close up not as expected | Run 'edge-eye selftest' on the Jetson (ST-06) and set [perception] movement_change as its hint suggests; paste both blocks. |
| E-CHK-418 | radio budget exceeded or reports lost | Paste the block with the per-window figures and 'casualty-analyst status'. |
| E-CHK-511 | TAK server, rescue-dispatch or the services not ready | The line names what is missing: taky (`sudo systemctl status rubblesar-taky`, `scripts/zbook/34-taky.sh`), rescue-dispatch (`rescue-dispatch status`, `scripts/zbook/30-install.sh`), the street file (`scripts/zbook/32-roads.sh`), the CHECK-1 team in [[dispatch.team]]; for the drones and the other services see E-CHK-411. |
| E-CHK-512 | TAK map incomplete or wrong | Markers missing, in the wrong colour or place, or drones and teams missing: `rescue-dispatch status` (TAK link, events sent and queued), `docker logs rubblesar-taky`; paste the block. |
| E-CHK-513 | routes cross rubble or a river, or are missing | Paste the block; `rescue-dispatch roads` shows the street network, the rivers and the streets closed per rubble area. A walk across a river with no rivers listed: the street file is from 0.6.0, run `scripts/zbook/32-roads.sh`. |
| E-CHK-514 | dispatch order wrong or casualties not reached | Paste the block and `rescue-dispatch status` (teams still busy, casualties with no route). With the time limit reached, more teams ([[dispatch.team]]) or a higher [dispatch] time_scale finish sooner. |
| E-CHK-515 | the TAK team loop did not complete | sar-check plays CHECK-1 on the TAK server: it needs its route line and chat over TAK, and rescue-dispatch must see its position reports and its "done". Paste the block with `journalctl -u rescue-dispatch -n 50` and `docker logs rubblesar-taky`. |
| E-CHK-516 | chat messages missing | Dispatch and arrival messages go to All Chat Rooms through the TAK server; check `rescue-dispatch status` for reconnects and dropped events; paste the block. |
| E-CHK-601 | a service or a drone is down (P6-01) | The block lists `rubblesar status`: `rubblesar up` starts everything. WARN: the Jetson not linked (`scripts/zbook/38-jetson-link.sh --jetson USER@IP`) or a drone on older software (update the Jetson to this release). |
| E-CHK-602 | the offline map is missing, incomplete or wrong (P6-02) | `scripts/zbook/36-map.sh` draws it again; `sar-web status` shows what is served. Rubble or closed streets not where expected: paste the block (the street file and the scenario must be the ones the map was drawn from: run 36-map.sh after 32-roads.sh). |
| E-CHK-603 | the after-action report is missing or does not match the run (P6-03) | Paste the block; the recording stays in the folder it names (`rubblesar report --run <folder>` writes the report again). Not writable: `scripts/zbook/30-install.sh` makes `/var/lib/rubblesar/www/reports` yours. |
| E-CHK-701 | operator view not served or not connected (P7-01) | sar-web 0.9 or later must answer on [web] listen with the live view connected to zenohd: `sar-web status`, `sudo systemctl restart sar-web`; WARN: no offline map yet (scripts/zbook/36-map.sh). |
| E-CHK-702 | live picture disagrees with the services (P7-02) | The block lists the samples where the picture and casualty-analyst, mission-planner or rescue-dispatch differed for more than 4.5 s; paste it with `journalctl -u sar-web -n 50`. |
| E-CHK-703 | event stream not delivered (P7-03) | A proxy or firewall between the browser and the ZBook may buffer server-sent events; on the ZBook: `curl -N http://127.0.0.1:8090/v1/stream` must print events every few seconds. |
| E-CHK-704 | drone crops not served (P7-04) | sar-web reads casualty-analyst's store ([analyst] store_dir/<incident>); the rubblesar account must be able to read it (re-run scripts/zbook/30-install.sh). |
| E-CHK-705 | timeline disagrees with the after-action report (P7-05) | Paste the block: it lists casualties found, sent or freed in the report but missing from the live timeline. |
| E-CHK-801 | demo settings or storyboard not usable (P8-01) | See E-DIR-002 and E-DIR-001; `rubblesar demo check` prints the same. |
| E-CHK-802 | no recorded run, or it lacks the storyboard`s beats (P8-02) | `rubblesar demo live' records a mission with every beat (staged link events included); the phase 6/7 run works too, without the staged budget cut. |
| E-CHK-803 | the drone`s frame was not rendered or the model not run (P8-03) | See E-DIR-004. WARN, no model: install ONNX Runtime and the model on the ZBook (12-onnxruntime.sh, 82-evaluate.sh). WARN, not exact: [demo] edge_seed must equal edge-eye`s seed on the Jetson. |
| E-CHK-804 | narration not filled in from the run (P8-04) | `rubblesar demo script` prints every chapter with its warnings and the length. |
| E-CHK-805 | sar-web`s replay or the presenter page not working (P8-05) | See E-DIR-005 and E-WEB-003; sar-web must be this release`s (scripts/zbook/30-install.sh). |
| E-CHK-806 | a recorder failed its test recording (P8-06) | See E-DIR-003; `rubblesar demo test-record` repeats the test with its output. |
| E-CHK-807 | rough-cut tools missing (P8-07) | `sudo apt install ffmpeg` (scripts/zbook/92-demo-setup.sh); the text-to-speech program in [demo] tts, or set it to "". |
| E-CHK-808 | the rehearsal did not complete (P8-08) | `rubblesar demo replay --rehearse` shows each chapter as it goes; a presenter page must be open. |
| E-MAP-001 | the buildings could not be downloaded or read (`scenario-tool fetch-map`) | Check internet access and try again later (the Overpass API is sometimes busy), or `36-map.sh --no-download` for a map from the streets alone |
| E-MAP-002 | the street file could not be read for the map (`rescue-dispatch map-layers`) | `scripts/zbook/32-roads.sh --force`, then `36-map.sh` |
| E-MAP-003 | drawing the tiles failed (`tools/map/render_map.py`) | `python3 -c "import PIL"` must work (`sudo apt install python3-pil fonts-dejavu-core`); paste the block |
| E-MAP-004 | the map renderer found no DejaVu font | `sudo apt install fonts-dejavu-core`, or pass `--font` and `--bold-font` with any TrueType font |
| E-WEB-001 | sar-web cannot listen or does not answer | `sudo systemctl status sar-web`; another program on 8090 (`sudo ss -ltnp \| grep 8090`) or change `[web] listen` |
| E-DIR-001 | storyboard not usable | The message names the chapter and setting; compare with the built-in one (`rubblesar demo init` writes a copy) or set [demo] storyboard = "". |
| E-DIR-002 | demo settings not usable | Fix the setting named in ~/.config/rubblesar/demo.toml (`rubblesar demo init` writes a commented one next to it if it is missing). |
| E-DIR-003 | a recorder cannot be reached or used | OBS: running, Tools > WebSocket Server Settings enabled on port 4455 with the password in demo.toml (close code 4009 = wrong password), Windows firewall open for the ZBook, not already recording, the scene present (`rubblesar demo obs-setup`). A command recorder: the program installed (scrcpy: `adb devices` lists the phone). |
| E-DIR-004 | the drone`s frame cannot be rendered again | The run must have been recorded by this release (frame poses in events.jsonl) against the installed scenario; `rubblesar demo media --frame ID' tries another frame. |
| E-DIR-005 | sar-web or the presenter page not reachable | `sudo systemctl status sar-web` ([demo] web is its address on this ZBook); a presenter page must be open: OBS`s presenter scene (`rubblesar demo obs-setup') or a browser at /ops/?present=1. |
| E-DIR-006 | take not found or incomplete | `rubblesar demo takes` lists them; a rehearsal has no recording; for the rough cut, copy the recordings from other machines to this ZBook and give --clip NAME=FILE (or --main FILE). |
| E-DIR-007 | ffmpeg or the text-to-speech command failed | `sudo apt install ffmpeg` (scripts/zbook/92-demo-setup.sh); the message ends with ffmpeg's last lines. Text to speech: [demo] tts, run it by hand with a short text. |
| E-WEB-002 | sar-web cannot follow the bus | sar-web subscribes to the drones` and the services` messages on zenohd: `systemctl status zenohd`, then `sudo systemctl restart sar-web`; the live view's header says what it is waiting for. |
| E-WEB-003 | replay not available | The run named is not in the incident`s reports folder (`rubblesar report --list`), or no replay is loaded yet (`rubblesar demo replay' loads one). |
| E-OPS-001 | `rubblesar up/down`: a service did not start or stop | The message names it and the fix; `journalctl -u <service> -n 50`. The Jetson: `38-jetson-link.sh` sets up the SSH key and the sudo rule |
| E-OPS-002 | `rubblesar mission start` cannot start | The services must answer and at least one drone report: `rubblesar status`, `rubblesar up` |
| E-AAR-001 | the run cannot be recorded | The reports folder must be writable by you (`30-install.sh` creates `/var/lib/rubblesar/www/reports`) |
| E-AAR-002 | no recorded run, or a recording that cannot be read | `rubblesar report --list` shows the runs; a run stopped by a crash has a partial `events.jsonl`, which still gives a report |
| E-AAR-003 | the report could not be written | Disk full or the folder not writable; paste the block |


## Setup script errors (scripts/*)

| Code | Meaning | First thing to try |
|---|---|---|
| E-SETUP-&lt;n&gt; | a command failed with exit code n | The block shows the command and the last log lines; most often a network or package problem |
| E-SETUP-ROOT | script run with sudo | Run it as your normal user; it calls sudo itself |
| E-SETUP-SUDO | no sudo rights | Ask an administrator to add you to the sudo group |
| E-SETUP-CMD | a required command is missing | Install the package named in the hint, or run the 00-prereqs script |
| E-SETUP-ARCH | unsupported CPU | Only x86_64 (ZBook) and aarch64 (Jetson) are supported |
| E-SETUP-HW | wrong machine for this script | Use scripts/zbook on the ZBook and scripts/jetson on the Jetson |
| E-SETUP-APT | apt-get update failed | Check internet access and third-party entries in /etc/apt/sources.list.d/ |
| E-SETUP-NET | download failed | Check internet access, or use scripts/fetch-downloads.sh on a connected machine (docs/PHASE0.md, Offline Jetson) |
| E-SETUP-SHA | checksum mismatch | Run the script again; the corrupt file was deleted. Repeated mismatches mean something interferes with downloads |
| E-SETUP-GO | Go could not be installed or verified | Check go.dev is reachable, or set SAR_GO_SHA256; check /usr/local/go |
| E-SETUP-ZC | zenoh-c archive or install problem | Re-run 10-zenoh-c.sh; check `pkg-config --variable pc_path pkg-config` includes /usr/local/lib/pkgconfig |
| E-SETUP-ZD | zenohd failed to install or listen | `journalctl -u zenohd -n 50`; `sudo ss -ltnp \| grep 7447` for a port clash |
| E-SETUP-PORT | a port zenohd (7447) or taky (8087) needs is taken | `sudo ss -ltnp \| grep -E '7447\|8087'` shows the program; stop it (an old zenohd, another TAK server, a container?) |
| E-SETUP-ARG | wrong script arguments | See the usage line in the message |
| E-SETUP-BIN | binaries missing | Run the 20-build script first |
| E-SETUP-SVC | a service did not become healthy (edge-eye on the Jetson, casualty-analyst, mission-planner or rescue-dispatch on the ZBook) | The message says which: *crashed while starting* (paste the block; it shows the start of the crash), *cannot reach the zenoh router* (zenohd running on the ZBook? right `--zbook-ip`? firewall allows TCP 7447?), or casualty-analyst (`journalctl -u casualty-analyst -n 30`; often E-ANL-001, the storage folder) |
| E-SETUP-GPUACC | the edge-eye service account `rubblesar` cannot use the GPU (Jetson) | The block shows the model loading three ways (as the service, as `rubblesar`, as you) and the GPU device files with their groups. The installer has already added `rubblesar` to those groups; paste the block if it still fails |
| E-BUILD-001 | compile error | Paste the block; this should not happen with an unmodified release |
| E-TEST-UNIT | a unit test failed | Paste the block (it lists the failing test and line) |
| E-BUILD-CACHE | the Go toolchain itself crashed (`fatal error: fault`, `go/internal/gcimporter`, `internal/pkgbits` in the trace), twice: `20-build.sh` already cleared the build cache and tried again | Not a RubbleSAR fault. Check free disk space (`df -h ~`), then the memory (`sudo apt install memtester`, `sudo memtester 2G 1`); run `20-build.sh` again. Once only: `20-build.sh` clears the cache itself and carries on |
| E-TEST-ZENOH | zenoh library tests failed | Re-run 10-zenoh-c.sh, then 20-build.sh; paste the block if it persists |
| E-TEST-INFER | inference back-end tests failed (ONNX Runtime / TensorRT) | Re-run 12-onnxruntime.sh (and 75-tensorrt.sh on the Jetson), then 20-build.sh; paste the block |
| E-TEST-PYTHON | training-script tests failed (ZBook, plain python3) | Paste the block; `python3 --version` too |
| E-TEST-ZBOOK | one of the ZBook tests in 40-test.sh failed | Each failed section above has its own code |
| E-TEST-JETSON | one of the Jetson tests in 40-test.sh failed | Each failed section above has its own code |
| E-TEST-ACCEPT | phase 0 acceptance failed | Paste the sar-check block |
| E-TEST-PHASE1 | phase 1 acceptance failed (70-phase1-test.sh) | Paste the sar-check block printed above it |
| E-TEST-PHASE2 | phase 2 acceptance failed (90-phase2-test.sh) | Paste the sar-check block printed above it |
| E-TEST-PHASE3 | phase 3 test failed (90-phase3-test.sh: edge-eye selftest on the Jetson, sar-check phase3 on the ZBook) | Paste the block printed above it |
| E-TEST-PHASE4 | phase 4 test failed (90-phase4-test.sh: edge-eye selftest on the Jetson, sar-check phase4 on the ZBook) | Paste the block printed above it |
| E-TEST-PHASE5 | phase 5 test failed (90-phase5-test.sh: sar-check tak and sar-check phase5 on the ZBook) | Paste the block printed above it |
| E-TEST-PHASE6 | phase 6 test failed (90-phase6-test.sh: rubblesar status, sar-check tak and sar-check phase6 on the ZBook) | Paste the block printed above it |
| E-TEST-PHASE7 | phase 7 test failed (90-phase7-test.sh: sar-web status, rubblesar status, sar-check phase7 on the ZBook) | Paste the block printed above it |
| E-TEST-PHASE8 | phase 8 test failed (90-phase8-test.sh: rubblesar demo check, sar-check phase8 on the ZBook) | Paste the block printed above it |
| E-SETUP-PKG | an apt package could not be installed | Check internet access and `sudo apt-get update`; the message names the packages |
| E-SETUP-TAKY | the taky image or service failed (34-taky.sh) | Building needs internet (Docker Hub, PyPI); the block shows `docker logs rubblesar-taky` when taky does not open TCP 8087; `sudo ss -ltnp \| grep 8087` for a port clash |
| E-SETUP-ROADS | the streets could not be downloaded or used (32-roads.sh) | Check internet access and try again later (the Overpass API is sometimes busy), or `32-roads.sh --synthetic` for a street grid; `--from FILE` takes a file downloaded elsewhere with `scenario-tool fetch-roads` |
| E-SETUP-CFG | a config file the script needs is missing | The message names it; it ships with the release (configs/) |
| E-SETUP-ORT | ONNX Runtime archive or pkg-config problem | Re-run 12-onnxruntime.sh; `pkg-config --modversion rubblesar-onnxruntime` should print the pinned version |
| E-SETUP-DOCKER | Docker not usable | `docker info` must work; add yourself to the docker group (`sudo usermod -aG docker $USER`, then log out and in) |
| E-SETUP-GPU | NVIDIA driver missing or too old for the training image | `nvidia-smi`; the script falls back to an older CUDA image for drivers older than 570 |
| E-SETUP-TRT | TensorRT pieces missing on the Jetson | `sudo apt install libnvinfer-dev libnvinfer-bin` (part of JetPack) |
| E-SETUP-MODEL | model folder incomplete | Run 82-evaluate.sh on the ZBook before copying the model |
| E-SETUP-DATA | a new release folder has no `data/`, but an earlier release next to it has one (datasets, training runs) | Run the `ln -s …` command in the hint (safe while a training run is writing there), then the command again |
| E-CAM-001 | Argus camera daemon not running | `sudo systemctl restart nvargus-daemon` |
| E-CAM-002 | a CSI camera did not deliver a frame | Check the ribbon cable and the camera overlay in `sudo /opt/nvidia/jetson-io/jetson-io.py`; cameras are optional in phase 0 |

## Running the system (phase 6)

| Symptom | Check |
|---|---|
| `rubblesar status` shows a service FAIL | `rubblesar up`; if it stays down: `journalctl -u <service> -n 50` (the status line prints the command) |
| The Jetson line says "cannot reach it over SSH" | `scripts/zbook/38-jetson-link.sh --jetson USER@JETSON-IP` (again after `30-install.sh --force-config`, which clears `[ops] jetson`); `ssh USER@JETSON-IP` must work without a password |
| Drones show "edge-eye 0.6 or older" | The Jetson runs an older release: unpack this one there, `scripts/jetson/20-build.sh`, `scripts/jetson/30-install.sh --zbook-ip <ZBook IP>` |
| `http://<ZBook>:8090/` does not open on the laptop | Same network? `sudo ufw status` allows 8090/tcp (`30-install.sh` adds it); `sar-web status` on the ZBook |
| The RubbleSAR map does not appear in WinTAK | Streaming: the XML came from `http://<ZBook>:8090/` (it carries that address), is in `C:\ProgramData\WinTAK\Imagery`, and WinTAK was restarted; then MOBILE, **Online**. MBTiles: imported, then MOBILE, **Local** |
| The map is blank or grey when zoomed out far | It covers zoom 13 to 19 over the incident area only; outside it WinTAK shows its own world map |
| A mission never ends | It ends when the survey is done, no close look is open and no team is busy, for 30 s; or after `[ops] max_mission` (90 min); Ctrl-C ends it and still writes the report |

## The live operator view (phase 7)

| Symptom | Check |
|---|---|
| The header says "Waiting" and "Live view: waiting for zenohd at ..." | `systemctl status zenohd`, then `sudo systemctl restart sar-web` |
| The page opens but nothing moves | "Reconnecting" in the header: a proxy or a firewall buffers the event stream (`curl -N http://127.0.0.1:8090/v1/stream` on the ZBook must print events). "Live" but no drones listed: no mission running (`rubblesar mission start`) |
| A drone shows "not heard" | Its link is down or the Jetson is off; its reports wait on board and arrive when it is heard again (the timeline says how many) |
| No crops in a casualty's card | The drone sent metadata only (the link was short of budget), or casualty-analyst's store is not readable by sar-web (`30-install.sh`) |
| The map is blank grey | No offline map for the incident: `scripts/zbook/36-map.sh` (the markers, rubble and routes still show) |

## Recording the demo (phase 8)

| Symptom | Check |
|---|---|
| `rubblesar demo check`: OBS "connection refused" | OBS not running, or Tools > WebSocket Server Settings not enabled (port 4455). On the WinTAK laptop: Windows Defender Firewall must allow OBS (or port 4455) on the incident network |
| OBS: close code 4009 | The password in `~/.config/rubblesar/demo.toml` differs from OBS's (Show Connect Info) |
| The presenter scene in OBS is black or shows "no presenter" | The browser source's URL must be sar-web on this ZBook (`http://127.0.0.1:8090/ops/?present=1`); right-click the source > Refresh. `rubblesar demo check` counts the presenter pages open |
| The presenter shows the wrong run | `rubblesar demo replay --run RUN` loads the run you want; `rubblesar report --list` lists them |
| The model panel shows "the scenario's labels" | No model on the ZBook: `12-onnxruntime.sh`, `82-evaluate.sh`, then `rubblesar demo media` again |
| The radio chapter shows no blackout | The run had none: record a live take (`rubblesar demo live`), which stages the budget cut and the blackout of `demo.toml` |
| scrcpy records nothing | `adb devices` must list the phone (USB debugging on, the prompt accepted); scrcpy 1.x does not know `--no-audio` |
| The rough cut has no WinTAK insert | Copy the WinTAK recording to the ZBook and give it: `rubblesar demo cut --clip wintak=FILE` |

## TAK devices (phase 5)

| Symptom | Check |
|---|---|
| ATAK / WinTAK does not connect (server icon red) | Same network as the ZBook? Address = the ZBook's IP (`hostname -I`), port **8087**, protocol **TCP** (not SSL, which is ATAK's default with 8089). On the ZBook: `sar-check tak` passes, `sudo ufw status` allows 8087/tcp. From the device's network: `nc -zv <ZBook IP> 8087` on a laptop |
| Connected, but no markers | Zoom to Antakya (36.2025 N, 36.1606 E). `rescue-dispatch status`: TAK connected, events sent; casualties known (casualty-analyst running, a run done since the last reset). Markers come within 30 s of connecting |
| Markers vanish after a few minutes | rescue-dispatch stopped refreshing them (`marker_stale`, 5 min): `systemctl status rescue-dispatch` |
| My device is not used as a team | Its callsign must equal a `[[dispatch.team]] callsign` with `source = "tak"` (case does not matter) and rescue-dispatch must have been restarted after editing; `rescue-dispatch status` shows the team `idle` (not `offline`) once the device reports its position. ATAK reports its position only with a GPS fix or a manually set position |
| Replies "done" are ignored | Send them to SAR-OPS (Contacts → SAR-OPS) or in All Chat Rooms, from the team's device; the first word counts (`done`, `unable`, `status`, `ok`). `rescue-dispatch status` lists the chat it received |
| Routes are straight lines | No street file: `scripts/zbook/32-roads.sh` |
| A route goes a long way round | Often right: OpenStreetMap marks some streets and bridges closed (`access=no`, `foot=no`; in Antakya the central bridge, Ata Köprüsü). `rescue-dispatch route --from LAT,LON --to LAT,LON` prints the streets it takes; `rescue-dispatch roads` how many pieces the access tags close. Check the command post ([dispatch] staging_lat/staging_lon) is on the same bank as the collapses |
| My WinTAK never gets a casualty | `rescue-dispatch status`: the team must be `idle`, not `offline`. Offline: WinTAK's callsign must equal `[[dispatch.team]] callsign` (VADER in the release config) and WinTAK must report a position at the incident (Settings → My Preferences → GPS Preferences → GPS Type: Stationary GPS, location 36.20160, 36.15850). E-RTE-002 in the log: the position is too far from any street |
| My casualty went to another team | Within `[dispatch] tak_ack_timeout` (3 min) reply `ok` to SAR-OPS or move 20 m towards the casualty; `unable` gives it back at once |
| The street map in WinTAK stops before street level | OpenStreetMap's tile servers refuse many apps since early 2026; the ZBook serves its own map from the next phase |
| Warning: the street file has no rivers | Downloaded by 0.6.0: `scripts/zbook/32-roads.sh` downloads it again with the rivers |
| A casualty says NO STREET ROUTE | Rubble closes every street within 200 m of it: `rescue-dispatch roads`; a smaller `[dispatch] block_margin_m` helps when streets only touch rubble |

## Exit codes

| Program | 0 | 1 | 2 |
|---|---|---|---|
| sar-check | all checks passed (warnings allowed) | a check failed | could not run (configuration, library) |
| sar-doctor | no failures | at least one failure | bad arguments |
| sar-monitor --once | telemetry seen | nothing heard | could not run |
| edge-eye | stopped normally | — | could not start |
| scenario-tool | success | `verify` found a bad site | could not run |
