# RubbleSAR

Drone-imagery decision support for urban search and rescue: a Jetson Orin Nano plays the payload computer of simulated drones, finds body parts in rubble and sends compact detections over Zenoh to an HP ZBook, which grades casualties, plans rescue routes and feeds ATAK/WinTAK through a TAK server.

This release is **phases 7 and 8 — the live operator view and the demo video**: anyone on the network follows the incident in a browser (`http://<ZBook>:8090/ops/`: map, drones, casualties with the drones' crops, teams, timeline, chat) without TAK; and the ZBook directs the recording of a narrated video of the whole system (`rubblesar demo`): a mission recorded on every screen with staged radio events, then replayed as 14 explained chapters on a presenter page that OBS records, with narration, subtitles and a rough cut. It builds on phase 6 (one command for both machines, the offline incident map, the after-action report), phase 5 (rescue teams sent along real streets around the rubble, a TAK server, ATAK and WinTAK showing it all live), and on phase 4 (casualty records, triage, investigator drones), phase 3 (the edge pipeline), phase 2 (the body-part model), phase 1 (simulated Antakya incident, virtual cameras, scorer) and phase 0 (Zenoh link, simulated drones, link throttling).

**Picking the project up in a new chat or as a new developer: [docs/HANDOVER.md](docs/HANDOVER.md) first.**

**New install: [docs/PHASE0.md](docs/PHASE0.md) to [docs/PHASE6.md](docs/PHASE6.md), then [docs/PHASE7.md](docs/PHASE7.md) and [docs/PHASE8.md](docs/PHASE8.md). Upgrading from phase 6: [docs/PHASE7.md](docs/PHASE7.md) (ZBook), then [docs/PHASE8.md](docs/PHASE8.md).**

## Phases

| Phase | Delivers | Status |
|---|---|---|
| 0 Foundations | Zenoh link, schemas, simulated drone telemetry, control, link throttling, setup and test tooling | accepted |
| 1 Scenario and imagery | Antakya scenario, procedural debris layers, virtual colour + thermal cameras, ground-truth scorer | accepted |
| 2 Model | Body-part segmentation trained on the ZBook, TensorRT engine on the Jetson | accepted |
| 3 Edge pipeline | Survey missions, detection, thermal cue, tracking, geolocation, compression, store-and-forward, casualty-analyst intake | accepted |
| 4 Analysis and drones | Casualty fusion, thermal triage, investigation missions | accepted |
| 5 Routing and TAK | Rescue teams and dispatch, street routes around rubble (OpenStreetMap, built-in router), taky (Docker), CoT to ATAK/WinTAK, team positions and chat back, extrication times | accepted |
| 6 One application | `rubblesar` command for both machines, offline incident map for WinTAK/ATAK (sar-web), run recording and after-action report | accepted |
| 7 Operator view | live browser dashboard on sar-web for people without TAK, fed from the bus; `sar-check phase7` compares it with the services through a whole run | **this release** |
| 8 Demo video | `rubblesar demo`: live take with staged link events, frames rendered again, narrated replay on a presenter page, OBS and scrcpy driven from the ZBook, narration, subtitles, rough cut | **this release** |
| next | TLS (zenoh and TAK SSL), link-loss tests | to agree |

## What runs where (phase 8)

| Machine | Program | Role |
|---|---|---|
| Jetson | `edge-eye` (service) | hosts the simulated drones: `scout-1` surveys, `inv-1` and `inv-2` wait for investigation tasks; telemetry at 1 Hz, control channel, per-drone link budget, local admin API; per drone the perception pipeline (virtual cameras → TensorRT model → thermal cue → geolocation → tracker → movement while hovering → reports) and a store-and-forward queue in `/var/lib/rubblesar/edge/queue` |
| ZBook | `zenohd` (service) | Zenoh router on TCP 7447 |
| ZBook | `casualty-analyst` (service) | stores and acknowledges detection reports in `/var/lib/rubblesar/detections/<incident>/`; merges them into casualty records with a triage colour |
| ZBook | `mission-planner` (service) | sends the investigators to the casualties that need a close look |
| ZBook | `rescue-dispatch` (service) | sends rescue teams (simulated, or TAK users) to the casualties along streets clear of rubble; puts casualties, drones, teams, routes and chat on the TAK map |
| ZBook | `rubblesar-taky` (service, Docker) | taky TAK server on TCP 8087 for ATAK, WinTAK and rescue-dispatch |
| ZBook | `sar-web` (service) | serves the offline incident map, the after-action reports, the live operator view (`/ops/`, fed from the bus) and the demo presenter page on port 8090 |
| ZBook | `rubblesar` | `status`, `up`, `down` of both machines; `mission start` (a recorded run and its after-action report); `report`; `demo` (records the demo video: OBS, scrcpy, ffmpeg) |
| ZBook | `/var/lib/rubblesar/www/` | the offline map (`map/`) and the reports (`reports/`) |
| ZBook | `/var/lib/rubblesar/roads/antakya-demo.json` | the streets around the incident (OpenStreetMap, downloaded once) |
| ZBook | `sar-monitor` | live telemetry, pipeline, queue, detection, casualty, task and team tables |
| both | `sar-check` | acceptance tests: `loopback`, `router`, `phase0`, `phase1`, `phase2`, `phase3`, `phase4`, `phase5`, `phase6`, `phase7`, `phase8`, `tak` (ZBook) |
| both | `sar-doctor` | environment check with fixes |
| both | `scenario-tool` | generate and render the scenario, virtual camera frames, preview pages, scoring |
| both | `/var/lib/rubblesar/scenarios/antakya-demo` | the installed scenario (`scenario.json` + rendered layers) |
| both | `sar-infer` | runs and checks the body-part model: TensorRT (Jetson), ONNX Runtime (both) |
| both | `/var/lib/rubblesar/models/current` | the installed model (ONNX, model card, golden set; plus the TensorRT engine on the Jetson) |
| ZBook | `rubblesar-train` (Docker image) | dataset training and ONNX export on the GPU (Ultralytics YOLO11-seg) |

## Repository layout

```
cmd/                 one folder per program (edge-eye, casualty-analyst, mission-planner, rescue-dispatch, sar-web, rubblesar, sar-check, sar-monitor, sar-doctor, scenario-tool, sar-infer)
internal/bus         messaging interface + in-memory bus; internal/bus/zenoh = cgo shim over zenoh-c 1.9.0
internal/edge        edge-eye runtime, perception loop and admin API
internal/perceive    onboard pipeline: thermal cue, geolocation with uncertainty, tracker, report packing
internal/outbox      store-and-forward queue on disk (priorities, payload reduction, re-sending)
internal/intake      casualty-analyst storage and acknowledgements
internal/casualty    fusion of reports into casualty records, triage
internal/analyst     casualty-analyst service (intake + casualty records + publishing)
internal/planner     mission-planner service (investigation tasks)
internal/dispatch    rescue-dispatch service (teams, assignments, the TAK picture)
internal/roads       street network from OpenStreetMap, closing streets under rubble, routes
internal/basemap     what the offline map draws (streets as routed, rubble, buildings, water)
internal/web         sar-web (map tiles, downloads, reports, the operator view and presenter page in internal/web/ops)
internal/live        the live picture: bus to picture, timeline, crops; replays of recorded runs
internal/director    the demo director (rubblesar demo): storyboard, facts, takes, recorders, rough cut
internal/obs         OBS Studio WebSocket (v5) client and a fake OBS for tests
internal/aar         run recorder and after-action report
internal/ops         the rubblesar command (status, up, down, mission)
internal/cot         Cursor-on-Target events, TAK client (reconnecting) and a small TAK router for tests
internal/mission     survey missions (lawnmower over the sites)
internal/flightsim   deterministic multirotor model, GPS and heading drift, investigation tasks
internal/linkbudget  token-bucket radio budget
internal/scenario    incident model, seeded generator, OpenStreetMap outlines
internal/render      site imagery layers (2.5D procedural renderer)
internal/vcam        virtual gimbal cameras: colour, radiometric thermal, labels
internal/score       detection scoring against ground truth
internal/dataset     training-set generator (tiles, exact outlines, held-out test frames)
internal/masks       connected components, outline tracing and simplification, mask IoU
internal/tiles       1080p frame -> 640 px tiles
internal/infer       model back ends (TensorRT and ONNX Runtime C shims), decoding, NMS, masks, golden set
internal/evalseg     accuracy evaluation (per class, per view, geolocated)
internal/onnxgen     tiny ONNX models for tests
internal/thermal     sun position, air temperature, skin-temperature models
internal/geom, internal/raster   2-D geometry; noise, buffers, PNG I/O
internal/check       acceptance suites; internal/doctor = environment checks
internal/config      TOML configuration with validation
internal/diag        error codes, hints and the copy-and-paste report format
internal/sarpb       generated protobuf code (from proto/sar/v1)
proto/sar/v1         message schemas
configs/             default configs for the Jetson and the ZBook; configs/scenario, configs/dataset = generators
training/            training container (Dockerfile, rubblesar_train.py) and its tests
tools/map/           the offline map renderer (render_map.py, Python + Pillow)
deploy/              systemd units, zenohd configuration, the taky image (deploy/zbook/taky)
scripts/jetson, scripts/zbook   numbered setup and test scripts
docs/                PHASE0 … PHASE8 (runbooks), ENVIRONMENT (reference), TROUBLESHOOTING (error codes)
data/                (created) scenarios, preview pages, sample frames, datasets, training runs, model folders
vendor/              Go dependencies, so builds need no internet
```

## Developing

```bash
make build        # bin/ (needs libzenohc; sar-doctor alone: make doctor)
make test         # unit tests (no network) + zenoh library tests + inference back-end tests
make test-infer   # ONNX Runtime / TensorRT back-end tests (whichever is installed)
make test-python  # training-script tests (plain python3, no GPU)
make test-race    # everything under the race detector
make vet          # go vet and gofmt
make demo-scenario   # generate + render the demo scenario into data/scenarios/antakya-demo
make proto        # regenerate internal/sarpb (needs protoc and protoc-gen-go v1.36.12)
```

## Design notes

- **Zenoh from Go.** The official zenoh-go binding has no tagged release and needs zenoh-c's unstable API, so RubbleSAR uses a small C shim over the *stable* zenoh-c 1.9.0 API (`internal/bus/zenoh/shim.c`), hidden behind `internal/bus`. Swapping in zenoh-go later touches one package.
- **Errors you can paste.** Every failure carries a code (`E-AREA-NNN`) and a hint; tools and scripts print a framed block with the context needed to diagnose it remotely. See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).
- **Reproducible builds.** Versions and checksums are pinned in `VERSIONS.env`; Go modules are vendored.
- **Simulated incident.** Central Antakya (Hatay, Türkiye), heavily damaged in the 6 February 2023 earthquakes; change `[site]` on both machines and the scenario config to move it. All victims and collapses are simulated.
- **One model, three runtimes, one decoder.** Training and export use Ultralytics in Docker on the ZBook (Python is offline tooling only). At run time the network is executed by TensorRT (Jetson) or ONNX Runtime through small C shims, and everything around it (tiling, decoding, NMS, masks) is Go, the same code on both machines. A golden set and Ultralytics' own predictions prove that the Go decoding and the TensorRT engine give the same answers.
- **Reports that survive the radio.** Every report is written to disk on the drone before it is sent and deleted only when the ZBook acknowledges that it is on disk there. Telemetry goes first, then reports by priority; a growing backlog shrinks the payload (smaller crop, thermal only, metadata only) instead of dropping reports. `sar-check phase3` proves it with a scheduled outage and budget cut.
- **Positions with honest uncertainty.** Each position carries a CEP worked out from the drone's GPS, heading, altitude and gimbal errors and the unknown height on the rubble. The tracker treats slowly drifting errors (GPS) differently from frame-to-frame ones, so repeated sightings do not make it overconfident.
- **Imagery you can test.** The renderer is deterministic and pure Go, so both machines build the same scenario and every claim about the imagery (labels align with truth, living people are warm, moving people move) is checked automatically by `sar-check phase1`. Blender-rendered layers can be added later without changing the layer format.
- **One record per person.** Drones' GPS errors are about as large as the spacing of victims, so casualty-analyst matches each drone's records as a group: it finds the shift between that drone's error and what is already known, then pairs records one to one, so neighbours stay apart and the same person seen twice is merged. Investigations are only sent where a close look can change the answer.
- **Routing without a routing server.** Rescue routes need only a street graph, a list of closed streets and a shortest-path search, so rescue-dispatch does it itself in Go over OpenStreetMap streets downloaded once: no Valhalla or OSRM to install, and it works with no internet during the incident. A route goes along open streets to the point closest to the casualty they reach, then on foot, slowly over rubble.
- **A live view without TAK.** sar-web follows the same bus messages the services publish and keeps one picture in memory, pushed to browsers as server-sent events: a plain page, no install, no framework. A drone that goes dark is noticed by its silence, since it sends nothing while its link is down.
- **A video that cannot drift from the system.** The demo is a recorded run replayed on the operator view itself, with the narration's numbers taken from that run; the camera frames are rendered again from the drone's recorded pose and seed, so what is shown is what the drone processed.
- **TAK as the map.** Rather than a map of its own, RubbleSAR speaks Cursor-on-Target to a TAK server, so the casualties, drones and teams appear on the devices rescuers already carry (ATAK, WinTAK), and real teams answer through the same app. Each simulated team has its own TAK connection, like a device, so a TAK server sees it as one.
