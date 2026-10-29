# Environment configuration

Everything RubbleSAR installs or expects, per machine, as of phase 5. Later phases add rows; nothing here is removed without notice.

## Topology

```
 Jetson Orin Nano (JetPack 6.2.2, Ubuntu 22.04, arm64)         HP ZBook (Ubuntu 26.04, amd64)
 ┌──────────────────────────────────────────┐   Wi-Fi    ┌──────────────────────────────────────────┐
 │ edge-eye.service  (user rubblesar)       │            │ zenohd.service    (user zenohd)          │
 │   3 simulated drones: scout surveys,     │──TCP 7447─▶│   router, listens 0.0.0.0:7447           │
 │   investigators fly tasks and hover      │            │   REST admin 127.0.0.1:8447 (read-only)  │
 │   virtual cameras, TensorRT model,       │            │ casualty-analyst.service (user rubblesar)│
 │   thermal cue, tracker, movement,        │            │   stores and acknowledges detections,    │
 │   detection queue                        │            │   casualty records and triage            │
 │   admin API 127.0.0.1:9101               │            │   status API 127.0.0.1:9102              │
 │ chrony client  ──────────────────────────┼──UDP 123──▶│ mission-planner.service (user rubblesar) │
 └──────────────────────────────────────────┘            │   investigation tasks for the drones     │
                                                         │   status API 127.0.0.1:9103              │
                                                         │ chrony server (local stratum 10)         │
                                                         │ sar-monitor, sar-check (connect locally) │
                                                         └──────────────────────────────────────────┘
```

All RubbleSAR traffic is initiated by the Jetson towards the ZBook; the Jetson needs no open inbound ports.

## Network and ports

| Machine | Port | Protocol | Bound to | Used by | Firewall |
|---|---|---|---|---|---|
| ZBook | 7447 | TCP | 0.0.0.0 | zenohd: the Jetson and local tools connect here | open (`15-zenohd.sh` adds a ufw rule when ufw is active) |
| ZBook | 8447 | TCP | 127.0.0.1 | zenohd REST admin, read-only diagnostics | not exposed |
| ZBook | 123 | UDP | all | chrony serving time to private networks | open when ufw is active |
| Jetson | 9101 | TCP | 127.0.0.1 | edge-eye admin API (`edge-eye status`, `edge-eye budget`) | not exposed |
| ZBook | 9102 | TCP | 127.0.0.1 | casualty-analyst API (`casualty-analyst status`, `/v1/casualties`, `/v1/reset`) | not exposed |
| ZBook | 9103 | TCP | 127.0.0.1 | mission-planner API (`mission-planner status`, `/v1/tasks`, `/v1/reset`), phase 4 | not exposed |
| ZBook | 9104 | TCP | 127.0.0.1 | rescue-dispatch API (`rescue-dispatch status`, `/v1/state`, `/v1/reset`), phase 5 | not exposed |
| ZBook | 8087 | TCP | 0.0.0.0 (Docker) | taky TAK server, plain CoT over TCP: ATAK, WinTAK, rescue-dispatch, sar-check (phase 5) | open (`34-taky.sh` adds a ufw rule when ufw is active); no encryption: private networks only |
| ZBook | 8090 | TCP | 0.0.0.0 | sar-web: the offline map's tiles, the MBTiles file and map source XML, the after-action reports (phase 6); the live operator view `/ops/` and its API, read-only (phase 7); the demo director's `POST /v1/replay` and `/v1/present`, accepted from the ZBook itself only (phase 8) | open (`30-install.sh` adds a ufw rule when ufw is active); plain HTTP, no login |
| ZBook, WinTAK laptop | 4455 | TCP | all | OBS Studio's WebSocket server, password-protected: the demo director starts and stops the recordings (phase 8, optional) | the ZBook's own OBS: local only is enough; the laptop's: allow OBS on private networks in Windows Firewall |
| Jetson | 22 | TCP | all | SSH: `rubblesar up/down` on the ZBook start and stop edge-eye (phase 6, `38-jetson-link.sh`) | as configured on the Jetson |

Recommendations: give the ZBook a fixed address (DHCP reservation) on the incident Wi-Fi; keep both machines on one subnet; zenoh multicast discovery is disabled, so addresses are always explicit.

## Pinned versions

| Component | Version | Source | Verified by |
|---|---|---|---|
| Go | 1.26.0 | go.dev | SHA-256 published on go.dev, checked at install |
| zenoh-c | 1.9.0 | github.com/eclipse-zenoh/zenoh-c releases (standalone zip) | SHA-256 in `VERSIONS.env` |
| zenohd | 1.9.0 | github.com/eclipse-zenoh/zenoh releases (standalone zip) | SHA-256 in `VERSIONS.env` |
| Go modules | protobuf 1.36.12, BurntSushi/toml 1.6.0 | vendored in `vendor/` | `go.sum`; builds need no internet |
| JetPack | 6.2.2 (Jetson Linux R36.5) | NVIDIA | `sar-doctor` (DOC-J1) |
| TensorRT | 10.3 (with JetPack; `libnvinfer-dev`, `libnvinfer-bin`) + CUDA 12.6 runtime headers | NVIDIA apt repository on the Jetson | `75-tensorrt.sh`, DOC-J3 |
| ONNX Runtime | 1.22.0 CPU (x64 and aarch64 release archives) | github.com/microsoft/onnxruntime releases | SHA-256 in `VERSIONS.env` |
| Training image base | `pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime` (driver ≥ 570); fallback `2.5.1-cuda12.4-cudnn9-runtime` | Docker Hub | `81-train.sh` picks by driver |
| Ultralytics | 8.3.253 (in the training image only) | PyPI | `pip freeze` saved in the image (`/opt/rubblesar/pip-freeze.txt`) |
| Starting weights | `yolo11s-seg.pt`, `yolo11n-seg.pt` (+ `yolo11n.pt` for Ultralytics' AMP check), `Arial.ttf` | github.com/ultralytics/assets releases | SHA-256 in `VERSIONS.env` |
| taky | 0.10 on `python:3.11-slim` (in the `rubblesar-taky` image only) | PyPI, Docker Hub | `TAKY_VERSION`, `TAKY_BASE_IMAGE` in `VERSIONS.env`; `34-taky.sh` prints the installed version |
| Street data | OpenStreetMap via the Overpass API (© OpenStreetMap contributors, ODbL 1.0), downloaded once | overpass-api.de | `32-roads.sh` parses and checks it |
| Operator view fonts | Barlow and Barlow Condensed (SIL Open Font License 1.1), embedded in sar-web | github.com/jpt/barlow | `internal/web/ops/fonts/OFL.txt` |
| Demo recording (phase 8, optional) | OBS Studio 28 or later (WebSocket server v5 built in; 30.2+ for chapter marks), ffmpeg (Ubuntu's), scrcpy 1.x or 2.x with adb | OBS PPA `ppa:obsproject/obs-studio` or Flathub `com.obsproject.Studio` (ZBook), obsproject.com (Windows); Ubuntu apt | `92-demo-setup.sh`, `rubblesar demo check`, `sar-check phase8` |

zenoh-c and zenohd must stay on the same version. To upgrade, change `ZENOH_VERSION` and the checksums in `VERSIONS.env` and re-run the `10-` and `15-` scripts.

## Files and locations

| Path | Machine | Contents | Owner / mode |
|---|---|---|---|
| `/usr/local/go` | both | Go toolchain; `/etc/profile.d/rubblesar-go.sh` adds it to PATH | root |
| `/usr/local/lib/libzenohc.so`, `/usr/local/include/zenoh*.h`, `/usr/local/lib/pkgconfig/zenohc.pc` | both | zenoh-c; list in `/usr/local/share/rubblesar/zenoh-c.manifest` | root |
| `/opt/rubblesar/bin/` | both | `edge-eye` (Jetson), `sar-monitor`, `casualty-analyst`, `mission-planner`, `rescue-dispatch`, `sar-web`, `rubblesar` (ZBook), `sar-check`, `sar-doctor`, `scenario-tool`, `sar-infer`; symlinked into `/usr/local/bin` | root, 0755 |
| `/opt/onnxruntime-1.22.0/`, `/usr/local/lib/pkgconfig/rubblesar-onnxruntime.pc` | both | ONNX Runtime headers and library; the programs find it through an rpath, nothing is added to the system library path | root |
| `/var/lib/rubblesar/models/<name>/`, `/var/lib/rubblesar/models/current` → `<name>` | both | installed models (ONNX, model card, golden set, test-frame list, ZBook evaluation; on the Jetson also `model.engine`, `engine.json`), about 60–120 MB each; written by `82-evaluate.sh` / `80-engine.sh` | root, read-only for services |
| `/var/lib/rubblesar/edge/queue/<uav>/*.pb` | Jetson | detections waiting for the ZBook's acknowledgement (store-and-forward, at most `[outbox] max_queue_mb` per drone) | rubblesar |
| `/var/lib/rubblesar/detections/<incident>/` | ZBook | `detections.jsonl` (one line per report), `crops/<uav>/<id>.jpg|.png` and (phase 4) `casualties-state.json` (casualty ids, investigation outcomes), written by casualty-analyst; `archive/<time>/` holds runs moved aside by `casualty-analyst reset` or `sar-check phase4` | rubblesar, readable by all |
| `/var/lib/rubblesar/planner/<incident>.json` | ZBook | mission-planner's tasks (phase 4) | rubblesar |
| `/var/lib/rubblesar/dispatch/<incident>.json` | ZBook | rescue-dispatch: casualties already reached, teams' counts (phase 5) | rubblesar |
| `/var/lib/rubblesar/roads/<incident>.json` | ZBook | the streets around the incident (Overpass JSON with node ids, or a synthetic grid), written by `32-roads.sh` (phase 5) | root, read-only for services |
| `/var/lib/rubblesar/map/<incident>-osm.json`, `<incident>-layers.json` | ZBook | buildings, water and green areas downloaded by `36-map.sh`; what the map is drawn from (phase 6) | root, readable by all |
| `/var/lib/rubblesar/www/map/<incident>/` | ZBook | the offline map: `{z}/{x}/{y}.png` tiles (zoom 13–19), `<incident>.mbtiles`, `legend.png`, `overview.png` (20–60 MB), served by sar-web (phase 6) | root, readable by all |
| `/var/lib/rubblesar/www/reports/<incident>/<run>/` | ZBook | one run: `events.jsonl` (the recording), `meta.json`, `index.html` (the after-action report), `report.json`, `summary.json` (1–3 MB), written by `rubblesar mission` and `sar-check phase6` (phase 6) | you (the installing user), readable by all |
| `/var/lib/rubblesar/www/reports/<incident>/<run>/media/` | ZBook | a scout frame rendered again with the model's outlines (`frame-rgb.jpg`, `frame-thermal.png`, `frame-masks.jpg`, `media.json`, about 1 MB), written by `rubblesar demo media` (phase 8) | you, readable by all |
| `~/.config/rubblesar/demo.toml`, `storyboard.toml` | ZBook | the demo director's settings (recorders, OBS passwords, staged link events) and the video's chapters, written by `rubblesar demo init` (phase 8) | you, 0600 / 0644 |
| `~/rubblesar-takes/<time>-<live|replay|rehearsal|test>/` | ZBook | one take: `cues.json`, `plan.json`, `narration.txt`, `narration.srt`, `shotlist.md`, `voice/`, `rough-cut.mp4`; the screen recordings stay where each OBS writes them (phase 8) | you |
| `/etc/sudoers.d/rubblesar-ops` | Jetson | lets your user run `systemctl start|stop|restart edge-eye` without a password (phase 6, `38-jetson-link.sh`) | root, 0440 |
| Docker image `rubblesar-taky:0.10` | ZBook | the TAK server (about 200 MB), built by `34-taky.sh` | Docker |
| `<repo>/data/datasets/`, `<repo>/data/runs/`, `<repo>/data/models/` | ZBook | training sets (1–2 GB each), training runs, model folders before installation | you |
| `~/rubblesar-data/models/<name>/` | Jetson | model folder copied by `85-push-model.sh` | you |
| Docker image `rubblesar-train:<hash>` | ZBook | training environment (8–9 GB); the tag changes when the Dockerfile, script or pinned versions change. Remove old ones with `docker image ls rubblesar-train` and `docker image rm` | Docker |
| `/etc/rubblesar/edge-eye.toml` | Jetson | edge-eye configuration | root, 0644 |
| `/etc/rubblesar/edge-eye.env` | Jetson | `SAR_ZENOH_CONNECT` (ZBook address), `RUST_LOG`, `NO_COLOR` | root, 0644 |
| `/var/lib/rubblesar/` | Jetson | edge-eye working directory | rubblesar |
| `/var/lib/rubblesar/scenarios/<id>/` | both | installed scenario: `scenario.json`, `sites/*/` layers and `site.json` (about 60 MB); written by `60-scenario.sh` | root, read-only for services |
| `<repo>/data/` | both | working copies: `scenarios/<id>/` (with `preview/index.html` on the ZBook), `frames/`, OpenStreetMap downloads | you |
| `/etc/rubblesar/sar.toml` | ZBook | sar-monitor, sar-check, casualty-analyst, mission-planner and rescue-dispatch configuration | root, 0644 |
| `/etc/rubblesar/zenohd.json5` | ZBook | zenoh router configuration | root, 0644 |
| `/usr/local/bin/zenohd`, `/usr/local/lib/zenoh/libzenoh_plugin_*.so` | ZBook | router and its plugins | root |
| `/etc/chrony/sources.d/rubblesar.sources` | Jetson | `server <zbook-ip> iburst prefer` | root |
| `/etc/chrony/conf.d/rubblesar.conf` | ZBook | `allow` private ranges, `local stratum 10` | root |
| `<repo>/reports/` | both | JSON test reports, setup logs (`reports/setup/`) | you |
| `<repo>/downloads/` | both | verified downloads, reused by re-runs and offline installs | you |

## Service accounts and services

| Machine | Account | Service | Start / stop / log |
|---|---|---|---|
| Jetson | `rubblesar` (system, no login, groups `video` and `render`: the groups that own the GPU device files; `30-install.sh` adds them and tests the model as this account) | `edge-eye.service` | `sudo systemctl restart edge-eye` · `journalctl -u edge-eye -f` |
| ZBook | `zenohd` (system, no login) | `zenohd.service` | `sudo systemctl restart zenohd` · `journalctl -u zenohd -f` |
| ZBook | `rubblesar` (system, no login) | `casualty-analyst.service` (phase 3) | `sudo systemctl restart casualty-analyst` · `journalctl -u casualty-analyst -f` |
| ZBook | `rubblesar` | `mission-planner.service` (phase 4) | `sudo systemctl restart mission-planner` · `journalctl -u mission-planner -f` |
| ZBook | `rubblesar` | `rescue-dispatch.service` (phase 5) | `sudo systemctl restart rescue-dispatch` · `journalctl -u rescue-dispatch -f` |
| ZBook | `rubblesar` | `sar-web.service` (phase 6; phase 7: built with zenoh, starts after zenohd and follows the bus for the operator view) | `sudo systemctl restart sar-web` · `journalctl -u sar-web -f` |
| ZBook | root (runs `docker run`; taky runs as user `taky` inside the container) | `rubblesar-taky.service` (phase 5): restarts daily (`RuntimeMaxSec=1d`), memory capped at 512 MB | `sudo systemctl restart rubblesar-taky` · `docker logs -f rubblesar-taky` |

The units restart on failure and are hardened (`ProtectSystem=strict`, `NoNewPrivileges`). edge-eye retries the ZBook forever, so boot order between the two machines does not matter; it also reconnects by itself if zenohd restarts.

## Configuration reference

### Shared sections (edge-eye.toml and sar.toml)

| Key | Default | Meaning |
|---|---|---|
| `[site] incident_id` | `antakya-demo` | Must match on both machines. `a-z`, `0-9`, `-`, max 32 characters |
| `[site] center_lat`, `center_lon` | 36.20250, 36.16056 | Incident centre (central Antakya, Hatay, Türkiye) |
| `[site] ground_alt_msl_m` | 80 | Ground height above sea level (the simulation uses flat ground at this height) |
| `[site] radius_m` | 805 | Operating radius (0.5 mile) |
| `[site] scenario_dir` | `/var/lib/rubblesar/scenarios/<incident_id>` | Scenario folder used by `sar-check phase1` (and the edge pipeline later) |
| `[zenoh] mode` | `client` | `client` (normal) or `peer` (tests) |
| `[zenoh] connect` | Jetson: `tcp/192.168.1.50:7447`, ZBook: `tcp/127.0.0.1:7447` | Router endpoints; overridden by `SAR_ZENOH_CONNECT` |
| `[zenoh] open_timeout` | `10s` | Per connection attempt |
| `[zenoh] retry_forever` | edge-eye `true` | Keep retrying until the router is reachable |
| `[zenoh] config_file` | (unset) | Full zenoh JSON5 file instead of the fields above (used for TLS) |
| `[log] level`, `format` | `info`, `text` | `debug`/`info`/`warn`/`error`; `text`/`json` |

Unknown keys are rejected with `E-CFG-003`, so a typo is reported rather than silently ignored. Check a file with `edge-eye check-config --config FILE`.

### edge-eye.toml only

| Key | Default | Meaning |
|---|---|---|
| `seed` | 1 | Same seed, same simulated flight |
| `[admin] listen` | `127.0.0.1:9101` | Admin API address |
| `[link] default_budget_kbps` | 256 | Radio budget per drone; 0 = unlimited |
| `[[uav]] id`, `role` | — | `scout` or `investigator` |
| `[[uav]] behaviour` | `hover` | `survey` (lawnmower over sites, return, land; phase 3), `standby` (wait at home for investigation tasks; phase 4), `orbit` (circle the centre), `hover` (hold over home), `landed` |
| `[[uav]] task_loiter_s`, `hover_wander_m` | 20, 0.15 | After a task, wait this long over the target for the next one before flying home; how far the drone drifts while holding (phase 4) |
| `[[uav]] survey_sites` | all sites | Site ids to survey, in order |
| `[[uav]] survey_speed_mps`, `side_overlap` | `speed_mps`, 0.7 | Speed along survey lines; share of the camera swath neighbouring lines share |
| `[[uav]] yaw_noise_deg` | 1 | Heading error of the IMU (1 sigma, drifting) |
| `[[uav]] frame_hz` | `[perception] frame_hz` | Camera frames per second of this drone |
| `[[uav]] home_offset_east_m`, `home_offset_north_m` | 0 | Take-off point relative to the centre |
| `[[uav]] alt_agl_m`, `speed_mps`, `orbit_radius_m` | —, 8, 150 | Flight profile (altitude 5–120 m when flying) |
| `[[uav]] telemetry_hz` | 1 | Telemetry rate (0–10 Hz) |
| `[[uav]] endurance_min`, `gps_noise_m` | 30, 1.5 | Battery model and GPS noise (1 sigma) |
| `[[uav]] budget_kbps` | `[link]` default | Per-drone override |
| `[perception] enabled` | false (release config: true) | Run the onboard pipeline |
| `[perception] model_dir`, `detector` | `/var/lib/rubblesar/models/current`, `model` | The model; `truth-oracle` tests the pipeline without a model (not accepted by `sar-check phase3`) |
| `[perception] conf`, `frame_hz` | 0.25, 1.0 | Detection confidence threshold; camera frames per second per drone |
| `[perception] skin_min_c`, `skin_max_c`, `warm_delta_c` | 20, 40, 3 | Thermal cue: skin band and the contrast that counts as warm |
| `[perception] min_thermal_px`, `warm_spots`, `warm_spot_min_px` | 9, true, 3 | Pixels needed for a temperature reading; thermal-only candidates |
| `[perception] confirm_hits`, `update_min_interval` | 2, 5s | Report a part after this many sightings; updates at most this often |
| `[perception] publish_truth`, `sim_start_s`, `camera_pitch_deg` | true, 0, -90 | Simulation truth side channel; scenario clock at start; camera angle |
| `[perception] detection_sigma_m` | 1.0 | Position error of a detection beyond the pose errors, added to the CEP (phase 4; calibrated from the phase 3 runs) |
| `[perception] debris_height_m` | 0 (release config: 2.5) | Height of the debris surface parts are assumed to lie on when geolocated (phase 4); the unknown remainder (1 m) is part of the CEP |
| `[perception] movement_change`, `movement_tolerance_px`, `movement_min_frames` | 0.12, 2, 2 | Movement while hovering: share of an outline that must shift by more than the tolerance, with the picture changed where it did (the same share), in at least this many frames for the part's own verdict (phase 4; `edge-eye selftest` ST-06 shows the model's figures) |
| `[outbox] dir`, `max_queue_mb` | `/var/lib/rubblesar/edge/queue`, 200 | Store-and-forward queue per drone |
| `[outbox] ack_timeout`, `max_backoff`, `probe_after` | 8s, 60s, 20s | Re-send timing; after `probe_after` without acknowledgement only a probe every 5 s |

### sar.toml only

| Key | Default | Meaning |
|---|---|---|
| `[check] expect_uavs` | `scout-1, inv-1, inv-2` | Drones the acceptance test must hear |
| `[check] expect_rate_hz`, `rate_tolerance_pct` | 1.0, 10 | Telemetry rate test |
| `[check] discover_timeout`, `rate_window` | 20s, 30s | Test timing |
| `[check] max_clock_offset`, `ping_timeout` | 250ms, 2s | Clock and control tests |
| `[check] report_dir` | `reports` | JSON reports (relative to where sar-check runs) |
| `[monitor] refresh`, `stale_after` | 1s, 5s | sar-monitor display |
| `[analyst] store_dir`, `save_crops` | `/var/lib/rubblesar/detections`, true | casualty-analyst storage (a folder per incident) |
| `[analyst] ack_interval`, `listen` | 200ms, `127.0.0.1:9102` | Acknowledgement batching; status API |
| `[analyst.fusion] link_m`, `span_m`, `window` | 2.0, 2.5, 10s | One drone's parts seen together within `link_m` (related parts within `span_m`) are one person (phase 4) |
| `[analyst.fusion] duplicate_m` | 1.5 | Parts of one class within this distance of each other count once against a person's limits (two hands, arms and legs, one head and torso); 0 counts every part |
| `[analyst.fusion] match_min_m`, `match_ceps`, `publish_interval` | 5, 3, 10s | Records of different drones are compared within max(`match_min_m`, `match_ceps` × combined CEP); the whole list is published this often |
| `[analyst.triage] skin_min_c`, `skin_max_c`, `warm_delta_c` | 20, 40, 3 | Warm: bare skin (hand, head) in the skin band and this much above ambient. Awaiting medical sign-off (D9) |
| `[analyst.triage] clothed_min_c`, `clothed_delta_c` | 15, 8 | Warm when only clothing is read (arm, leg, torso): from this temperature and this much above ambient (sun-warmed clothing on a cooled body reads several degrees above the air) |
| `[analyst.triage] close_range_m`, `min_watch_s` | 20, 20 | A close look: readings from within this range, watched this long |
| `[analyst.triage] moved_frames` | 2 | Moved: in one hover aimed at the casualty, its parts moved in this many frames in all |
| `[analyst.triage] cooling_c_per_h`, `trend_min_span` | 0.5, 10m | Deteriorating: warm but cooling faster than this over at least this span |
| `[analyst.triage] max_cep_m`, `not_found_looks` | 5, 2 | Ask for a look when the position is less certain; "not found" after this many empty looks |
| `[planner] listen` | `127.0.0.1:9103` | mission-planner API (phase 4) |
| `[planner] alt_agl_m`, `hover_s`, `radius_m` | 15, 30, 6 | Investigation: height and time over the casualty; parts within this radius count |
| `[planner] reserve_pct`, `max_attempts` | 25, 2 | Battery a drone must keep after a task and the flight home; completed looks per casualty |
| `[planner] task_timeout`, `command_timeout`, `publish_interval` | 4m, 3s, 5s | A task with no progress goes back to the queue; wait for a drone's answer; task list rate |
| `[planner] investigators`, `state_dir` | every `investigator`, `/var/lib/rubblesar/planner` | Drones that take tasks; where the tasks are kept |
| `[dispatch] listen` | `127.0.0.1:9104` | rescue-dispatch API (phase 5) |
| `[dispatch] roads`, `block_margin_m` | `""` (= `/var/lib/rubblesar/roads/<incident>.json`), 1.0 | The street file; streets within this distance of a rubble spill area are closed |
| `[dispatch] tak_server`, `callsign`, `group` | `127.0.0.1:8087`, `SAR-OPS`, `Cyan` | The TAK server (plain TCP; `""` = no TAK map); rescue-dispatch's callsign and team colour |
| `[dispatch] marker_refresh`, `marker_stale`, `drone_every`, `show_investigations`, `show_collapses` | 30s, 5m, 2s, true, true | Markers are sent on change and refreshed this often, and disappear from the devices this long after the last; drones at most this often; investigation lines on the map; the rubble of each collapse as a shape on the map (phase 6) |
| `[dispatch] triage` | `["red", "yellow"]` | Colours that get a team, most urgent first (also `"green"`; black and grey never) |
| `[dispatch] arrive_m`, `extrication`, `on_scene` | 15, true, 10m | A team this close has reached its casualty; it then frees them from the rubble (`extrication`: 15 min to 6 h from the scenario, docs/PHASE5.md "Freeing a casualty"; false = none) and hands them over (`on_scene`); a TAK team replies `done` when finished |
| `[dispatch] time_scale`, `rescue_time_scale` | 5, 30 | Simulated teams move, and free casualties, this many times faster than real people; messages and markers give real times |
| `[dispatch] team_offline`, `tak_ack_timeout` | 3m, 3m | A TAK team silent this long is offline; a TAK team that neither answers SAR-OPS nor moves 20 m towards its casualty this long after being sent loses it to another team (0 = never) |
| `[dispatch] staging_lat`, `staging_lon` | 0, 0 (= incident centre; the release file: 36.20160, 36.15850, on the west bank of the Asi by four of the five collapses) | The command post, where the teams start |
| `[dispatch] min_age`, `publish_interval`, `state_dir` | 10s, 5s, `/var/lib/rubblesar/dispatch` | A casualty gets a team once listed this long; dispatch state rate; state file |
| `[[dispatch.team]] id`, `callsign`, `mode`, `source` | TEAM-1, TEAM-2 (foot, sim), TEAM-3 (vehicle, sim), VADER (foot, tak: the WinTAK laptop), CHECK-1 (foot, tak) | One rescue team: `mode` foot or vehicle; `source` sim (simulated) or tak (a TAK user with exactly this callsign) |
| `[[dispatch.team]] at_command_post` | false | TAK teams only: the team stands at the command post (staging) whatever position its device reports, for a phone or laptop away from the incident (a demo). Its routes start there; it never arrives by itself, so it answers `done` when it decides |
| `[web] listen`, `root` | `0.0.0.0:8090`, `/var/lib/rubblesar/www` | sar-web (phase 6): the offline map (`root/map/<incident>`) and the after-action reports (`root/reports/<incident>`) to the network; the operator view (phase 7, no settings of its own: it follows `[zenoh]` and reads the services at `[analyst] listen`, `[planner] listen`, `[dispatch] listen`) |
| `[ops] jetson`, `jetson_service`, `max_mission` | `""`, edge-eye, 90m | The `rubblesar` command (phase 6): the Jetson as user@address for SSH (`38-jetson-link.sh` sets it; `""` = not handled from the ZBook), its service, the longest mission run |

### Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `SAR_ZENOH_CONNECT` | all | Comma-separated router endpoints, overrides `[zenoh] connect` |
| `SAR_INCIDENT_ID` | all | Overrides `[site] incident_id` |
| `SAR_LOG_LEVEL`, `SAR_LOG_FORMAT` | all | Override `[log]` |
| `RUST_LOG` | all (zenoh library), zenohd | zenoh's own log level, default `error` (programs) / `warn` (zenohd) |
| `NO_COLOR=1` | zenoh | No colour codes in logs |
| `SAR_DOWNLOAD_DIR`, `SAR_KEEP_GO=1`, `SAR_GO_SHA256` | setup scripts | Download cache, keep an existing Go, supply the Go checksum by hand |
| `RUBBLESAR_OPS_DIR` | sar-web | Serve the operator view's page files from this folder instead of the built-in copy (development only) |
| `INFER_TAGS` | `make` | Override the detected inference back ends (`ort`, `trt`); e.g. `make build INFER_TAGS=` builds without them |

## Zenoh key expressions

| Key | Direction | Message (proto/sar/v1) | QoS |
|---|---|---|---|
| `sar/{incident}/uav/{uav}/telemetry` | Jetson → ZBook | `Telemetry`, 1 Hz | data priority, drop under congestion, subject to the link budget |
| `sar/{incident}/uav/{uav}/control` | ZBook → Jetson | `Control` (ping, set budget, status, restart; phase 4: `Investigate`, `CancelTask`) | real-time priority, reliable, express |
| `sar/{incident}/uav/{uav}/control/reply` | Jetson → ZBook | `ControlReply` | real-time priority, reliable, outside the budget |
| `sar/{incident}/uav/{uav}/detection` | Jetson → ZBook | `Detection` (metadata, summary, crops, outline), per new or changed part | data-high priority, reliable, within the budget, held until acknowledged |
| `sar/{incident}/uav/{uav}/ack` | ZBook → Jetson | `DetectionAck` (ids stored) | interactive-high priority, reliable, express |
| `sar/{incident}/sim/uav/{uav}/truth` | Jetson → ZBook | `SimFrameTruth`: ground truth of each frame, simulation only, for scoring | background, may drop, not part of the radio budget |
| `sar/{incident}/casualty/{id}` | ZBook (casualty-analyst) → ZBook | `Casualty`, whenever it changes; once more with `merged_into` when merged away (phase 4) | data-high priority, reliable, not on the radio |
| `sar/{incident}/casualties` | ZBook (casualty-analyst) → ZBook | `CasualtyList`, on every change and every `publish_interval` (phase 4) | as above |
| `sar/{incident}/tasks` | ZBook (mission-planner) → ZBook | `TaskList`, on every change and every `publish_interval` (phase 4) | as above |
| `sar/{incident}/dispatch` | ZBook (rescue-dispatch) → ZBook | `DispatchState`: teams, assignments with routes, waiting casualties, streets, TAK link; on change and every `publish_interval` (phase 5) | as above |
| `sar/selftest/**`, `sar/{incident}-selftest/**` | local | test traffic from `sar-check` | — |

Every message starts with a `Header` (schema version 1, incident id, source, sequence number, UTC send time). Receivers reject other schema versions (`E-MSG-002`) and other incidents (`E-MSG-003`).

## Time

Latency and ordering checks need both clocks within 250 ms. The ZBook runs chrony as a local time server (`local stratum 10`, serving private networks) so this works without internet; the Jetson follows the ZBook (`server <zbook-ip> iburst prefer`). Check on the Jetson with `chronyc sources` (the ZBook should be marked `*`) and `chronyc tracking`.

## Link budget (Wi-Fi throttling)

Each simulated drone has its own token bucket on the Jetson (the drone's radio). Telemetry is sent first; detection reports use what is left, in priority order (new, warm and confident parts first; thermal-only warm spots last). When the queue holds more than 10 s of budget the reports go out with smaller crops, beyond 30 s with the thermal crop only, beyond 60 s as metadata only. Telemetry over budget is dropped (newest wins); control replies are never dropped but are counted. Change budgets at run time on the Jetson with `edge-eye budget`, remotely from the ZBook through the control channel (as `sar-check` CHK-08 does), or permanently in `edge-eye.toml`. `edge-eye status` and every telemetry message report the current budget, the bits used in the last second and the dropped count.

## TLS for the field

Phase 0 runs zenoh over plain TCP on a trusted network. For field deployment, switch to TLS (tested with zenoh 1.9.0):

1. On the ZBook, create a small certificate authority and a router certificate for its address (example address 192.168.1.50):
   ```bash
   sudo mkdir -p /etc/rubblesar/tls && cd /etc/rubblesar/tls
   sudo openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj "/CN=RubbleSAR CA" -keyout ca.key -out ca.crt
   sudo openssl req -newkey rsa:3072 -nodes -subj "/CN=zbook" -keyout zbook.key -out zbook.csr
   printf "subjectAltName=IP:192.168.1.50,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n" | sudo tee ext.cnf
   sudo openssl x509 -req -in zbook.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 -extfile ext.cnf -out zbook.crt
   sudo chown zenohd zbook.key && sudo chmod 600 ca.key zbook.key
   ```
2. In `/etc/rubblesar/zenohd.json5`, listen on TLS and name the certificate:
   ```json5
   listen: { endpoints: ["tls/0.0.0.0:7447"] },
   transport: {
     link: { tls: { listen_certificate: "/etc/rubblesar/tls/zbook.crt",
                    listen_private_key: "/etc/rubblesar/tls/zbook.key" } },
     shared_memory: { enabled: false },
   },
   ```
   then `sudo systemctl restart zenohd`.
3. Copy `ca.crt` (never `ca.key`) to `/etc/rubblesar/tls/ca.crt` on the Jetson and create `/etc/rubblesar/zenoh-client.json5` on both machines:
   ```json5
   { mode: "client",
     connect: { endpoints: ["tls/192.168.1.50:7447"] },   // ZBook tools may use tls/127.0.0.1:7447
     scouting: { multicast: { enabled: false } },
     transport: { link: { tls: { root_ca_certificate: "/etc/rubblesar/tls/ca.crt" } },
                  shared_memory: { enabled: false } } }
   ```
4. Point the programs at it: in `edge-eye.toml` and `sar.toml` set `[zenoh] config_file = "/etc/rubblesar/zenoh-client.json5"`, and on the Jetson remove `SAR_ZENOH_CONNECT` from `edge-eye.env`. Restart edge-eye and run `sar-check phase0` again.

For mutual authentication (each drone proves its identity) add `enable_mtls: true` on the router and `connect_certificate` / `connect_private_key` on each client, signed by the same CA. SSL for the TAK server (taky's port 8089 with client certificates) is not set up yet: taky uses plain TCP 8087 and sar-web plain HTTP (decision D10), for private networks only.

## GPU and inference

| Machine | What uses the GPU | How |
|---|---|---|
| ZBook | training (`81-train.sh`) and the Ultralytics comparison (`82-evaluate.sh`) | Docker `--gpus all` through the NVIDIA Container Toolkit; check with `docker run --rm --gpus all ubuntu nvidia-smi` (docs/PHASE2.md, *GPU in Docker*) |
| ZBook | evaluation, golden set (`sar-infer`) | ONNX Runtime on the CPU (no CUDA needed outside Docker) |
| Jetson | `sar-infer` and, from phase 3, `edge-eye` | TensorRT FP16 engine built on the Jetson by `80-engine.sh` (`trtexec`, batch 1–6, 1 GB builder workspace) |

Jetson speed depends on the power mode: use the highest (`sudo nvpmodel -p --verbose`; MAXN SUPER on an Orin Nano with JetPack 6.2) and `sudo jetson_clocks`.

## Offline installation

`scripts/fetch-downloads.sh` (run where there is internet) stores every archive both machines need in `downloads/`, verified. The setup scripts always look there first. The training image needs Docker Hub and PyPI the first time it is built; after that, training works offline.
