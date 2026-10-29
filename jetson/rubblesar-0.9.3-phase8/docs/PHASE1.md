# Phase 1 — Scenario and imagery

**Goal.** Both machines hold the same simulated incident — collapsed buildings in central Antakya with trapped people — and can photograph it from any drone position and time with a simulated colour camera and a simulated radiometric thermal camera, together with exact ground truth for every pixel. A scorer compares geolocated detections with that truth. Phase 2 trains and runs the body-part model against this imagery.

**Accepted when** `scripts/zbook/70-phase1-test.sh` **and** `scripts/jetson/70-phase1-test.sh` both end with
`RESULT: PASS (8 passed ...)` and `PHASE 1 ACCEPTED`.

**Time needed.** About 15 minutes: rebuild and tests (ZBook 3 min, Jetson 6–8 min), scenario (ZBook 1–2 min, Jetson 1–3 min), acceptance (1–2 min ZBook, 2–5 min Jetson).

**Disk.** About 60 MB per installed scenario, plus 35 MB of preview pictures on the ZBook.

> **Everything here is simulated.** No real person, building or casualty is represented. The thermal and casualty models are illustrative and must be reviewed by a medical lead before any operational interpretation.

---

## What this phase adds

| Piece | What it does |
|---|---|
| `scenario-tool generate` | Builds the incident from `configs/scenario/antakya-demo.toml` and a seed: 5 collapsed buildings (pancake or mound) within 350 m of the centre, 23 victims with their status (responsive, unresponsive, deteriorating, deceased), what sticks out of the rubble (hand, forearm, full arm, foot, lower leg, head, head and shoulders, back), skin-temperature model, movement, looks, and the rubble spill areas routes must avoid. Same file and seed = identical scenario on every machine. |
| `scenario-tool render` | Draws every site as imagery **layers** at 1.5 cm per pixel: colour, height, body-part class, victim id, and the thermal ingredients (skin coupling, solar absorptivity, fixed heat sources). Moving victims get a second pose. Each site also gets decoys: a sun-heated metal sheet, a smouldering hot spot and a cold skin-coloured object. |
| virtual cameras | From any drone pose (position, altitude, heading, gimbal pitch) and scenario time: a **1920×1080 colour frame** (70° field) and a **640×512 radiometric thermal frame** (50° field, values in centikelvin like real radiometric cameras), with sun position and shadows, the day's air temperature cycle, thermal inertia, optics blur and sensor noise, plus label frames giving the body-part class and victim of every pixel, and `frame.json` with the pose, intrinsics and ground truth. |
| scorer | Matches geolocated detections to the truth per class within 2 m: precision, recall, victims found, geolocation error. |
| `sar-check phase1` | The acceptance suite (below). |
| `sar-doctor` | New check DOC-09: is the scenario installed. |

The programs from phase 0 (`edge-eye`, `zenohd`, `sar-monitor`) are unchanged in behaviour; `edge-eye` starts photographing the scenario with the edge pipeline (phase 3), after the model is trained on this imagery (phase 2).

**Change from the plan.** The plan named Blender for the debris imagery. Phase 1 uses a procedural 2.5D renderer written in Go instead: it is deterministic, needs no GPU or extra installs, runs on both machines, and — most important for you — can be tested automatically. The layer format is independent of the renderer, so Blender-rendered layers can be added later as an optional realism upgrade without changing anything downstream.

---

## Upgrading from phase 0

1. Copy `rubblesar-0.2.0-phase1.tar.gz` to both machines and unpack it **next to** the phase 0 folder (your configs in `/etc/rubblesar` are kept):
   ```bash
   tar -xzf rubblesar-0.2.0-phase1.tar.gz && cd rubblesar-0.2.0-phase1
   ```
2. Run every script from that folder as your normal user, as before.

### ZBook

| Step | Command | What it does | Expect |
|---|---|---|---|
| P1-Z1 | `scripts/zbook/20-build.sh` | rebuilds everything (now including `scenario-tool`) and runs all unit tests | `DONE` |
| P1-Z2 | `scripts/zbook/30-install.sh` | installs the new programs; keeps your `/etc/rubblesar/sar.toml` and saves the new default as `sar.toml.new` | `DONE` |
| P1-Z3 | `scripts/zbook/15-zenohd.sh` | **only if you installed zenohd from 0.1.0**: moves zenohd's admin API off port 8000 (used by another program on your ZBook) to 127.0.0.1:8447 | `ok: listening on 0.0.0.0:7447` |
| P1-Z4 | `scripts/zbook/60-scenario.sh` | generates, renders, verifies, makes the preview, installs to `/var/lib/rubblesar/scenarios/antakya-demo`, takes two sample frames | `Scenario hash: 22e37148032a` and `DONE` |
| P1-Z5 | `scripts/zbook/70-phase1-test.sh` | acceptance suite, then a scorer demonstration | `RESULT: PASS (8 passed ...)`, `PHASE 1 ACCEPTED on the zbook` |

### Jetson

| Step | Command | What it does | Expect |
|---|---|---|---|
| P1-J1 | `scripts/jetson/20-build.sh` | native rebuild and all unit tests (6–8 minutes) | `DONE` |
| P1-J2 | `scripts/jetson/30-install.sh --zbook-ip <ZBook IP>` | installs the new programs and restarts `edge-eye` | `ok: edge-eye is running and connected` |
| P1-J3 | `scripts/jetson/60-scenario.sh` | generates and renders the same scenario on the Jetson and installs it | the **same** `Scenario hash` as the ZBook printed |
| P1-J4 | `scripts/jetson/70-phase1-test.sh` | acceptance suite on the Jetson | `RESULT: PASS (8 passed ...)`, `PHASE 1 ACCEPTED on the jetson` |

**Alternative to P1-J3:** copy the ZBook's files instead of rendering on the Jetson (byte-identical layers, needs SSH):
```bash
# on the ZBook
scripts/zbook/65-push-scenario.sh --jetson <jetson-user>@<jetson-ip>
# on the Jetson
scripts/jetson/60-scenario.sh --from ~/rubblesar-data/scenarios/antakya-demo
```
Use this if the two machines print different scenario hashes, or copy `/var/lib/rubblesar/scenarios/antakya-demo` on a USB stick and pass that folder to `--from`.

Optional regression check: `scripts/zbook/50-acceptance.sh` (phase 0) should still pass.

For a faster check, `70-phase1-test.sh --quick` tests every third victim plus all moving ones.

---

## Look at it

`60-scenario.sh` writes a preview on the ZBook. Open it in a browser:
```bash
xdg-open data/scenarios/antakya-demo/preview/index.html
```
- **Overview**: the 0.5-mile operating area, the five sites, rubble spill (red), victims in their expected START colour.
- **Victims**: for each victim, the 12 m investigator view in colour, the same with the ground-truth labels painted on (red hand, orange arm, blue leg, green head, purple torso), and thermal. Living victims glow; long-dead victims are close to the rubble temperature.
- **Sites**: a close-up map, the top view of the layers with labels, and what the scout sees from 40 m in colour and thermal.

Sample frames are in `data/frames/antakya-demo/`. Take your own:
```bash
scenario-tool frame --scenario /var/lib/rubblesar/scenarios/antakya-demo --out /tmp/f1 --victim v03 --alt 15
scenario-tool frame --scenario /var/lib/rubblesar/scenarios/antakya-demo --out /tmp/f2 --victim v10 --alt 40 --pitch -55 --yaw 200 --t 7200
xdg-open /tmp/f1/overlay.png
```
`--t` is seconds after the scenario start (09:00 local, 10 February); later times bring warmer air, a higher sun, sun-heated debris and cooling deteriorating victims.

---

## What the acceptance suite proves

| Check | What it verifies | Pass condition |
|---|---|---|
| P1-01 scenario | `scenario.json` is valid, matches `[site]` in this machine's config, and meets the minimums | ≥ 3 sites, ≥ 20 victims, same incident id and centre; warns if a status or body-part class is missing |
| P1-02 layer integrity | every layer file of every site against the SHA-256 recorded when it was rendered, and that each victim shows at least one body part | no missing, changed or damaged file |
| P1-03 camera geometry | pixel → ground → pixel round trips for both cameras over several poses, including oblique and rolled; 40 m footprint size | error below 10⁻⁶ pixel |
| P1-04 ground truth visible & aligned | every victim photographed from 12 m shows every body part the truth lists, and each label's centre geolocates back onto the truth | offset ≤ 0.15 m |
| P1-05 thermal plausibility | living victims stand out, long-dead victims are near ambient, deteriorating victims lose warmth over 3 hours, open ground is near air temperature | living > air + 6 °C; dead > 10 h within air + 6 °C |
| P1-06 victim movement | responsive victims change pose over time, all others never do | per-victim label masks differ / match |
| P1-07 frame render time | colour + thermal frame from 40 m | median ≤ 2.5 s (warning up to 6 s) |
| P1-08 scorer self-test | perfect, shifted, wrong-class, duplicate and empty detection sets give the expected counts | all cases as expected |

Every failure prints a code (`E-CHK-101` … `E-CHK-108`), the evidence and a hint, then the `COPY FROM HERE … COPY TO HERE` block.

---

## scenario-tool reference

```
scenario-tool generate  --config FILE --out DIR [--force]
scenario-tool fetch-osm --config FILE --out FILE            # real building outlines (internet)
scenario-tool render    --scenario DIR [--site ID]
scenario-tool verify    --scenario DIR
scenario-tool info      --scenario DIR                      # sites and victims table
scenario-tool frame     --scenario DIR --out DIR [--victim vNN | --e M --n M | --lat D --lon D]
                        [--alt 40] [--yaw 0] [--pitch -90] [--t SECONDS] [--uav NAME] [--seed N] [--gps-noise M]
scenario-tool preview   --scenario DIR [--out DIR]
scenario-tool truth-detections --scenario DIR --out FILE [--noise-m M] [--seed N]
scenario-tool score     --scenario DIR --detections FILE [--radius 2] [--json FILE]
```
Exit codes: 0 success, 1 a check failed (`verify`), 2 could not run (with a copy-and-paste block).

## Files and formats

```
/var/lib/rubblesar/scenarios/antakya-demo/
  scenario.json            the incident (sites, victims, blockages, environment)
  sites/s01/site.json      layer frame, SHA-256 of each file, ground truth per victim and body part, decoys
  sites/s01/*.png          layers (below)
  sites/s01/pose-b/v03/    second pose of a moving victim (same files, a small rectangle)
```

| Layer | Format | Meaning |
|---|---|---|
| `rgb.png` | 8-bit colour | surface colour with baked ambient occlusion (sun shading is added by the camera) |
| `height.png` | 16-bit grey | millimetres above ground |
| `parts.png` | 8-bit grey | 0 none, 1 hand, 2 arm, 3 leg, 4 head, 5 torso |
| `victim.png` | 8-bit grey | victim index (1…N) owning the pixel's warmth |
| `coupling.png` | 8-bit grey | 0–255: share of the victim's skin temperature the surface shows (skin 255, clothing 140, hair 115) |
| `absorb.png` | 8-bit grey | solar absorptivity × 255 |
| `tbase.png` | 8-bit grey | fixed heat above air in quarter degrees (hot spots) |

Pixel (x, y) of a site covers east `e0_m + x·gsd_m` … and north `n0_m − y·gsd_m` … (metres from the incident centre; `frame` in `site.json`).

A **frame folder** (`scenario-tool frame`) holds `rgb.png`, `thermal.png` (16-bit; °C = value / 100 − 273.15), `thermal-view.png` (false colour), `labels-parts.png`, `labels-victim.png`, the same two at thermal resolution, `overlay.png` and `frame.json`. In `frame.json`, `pose_reported` is what the drone's GPS and IMU claim (with `--gps-noise`); keys starting with `truth_` are ground truth for evaluation only and are never sent to the onboard pipeline.

**Detections for scoring** (`scenario-tool score`):
```json
{"schema": 1, "scenario_id": "antakya-demo", "source": "any text",
 "detections": [{"class": "hand", "lat": 36.2040290, "lon": 36.1612359, "confidence": 0.91, "uav": "scout-1", "frame_id": "..."}]}
```
Classes: `hand`, `arm`, `leg`, `head`, `torso`. A detection matches a truth part of the same class within 2 m (`--radius`); each truth part matches at most once; further detections of an already-found part count as duplicates, not false alarms. Body parts smaller than 0.004 m² (a few centimetres across) are not expected to be found.

## Changing the scenario

Edit a copy of `configs/scenario/antakya-demo.toml` (every setting is commented) and run `scripts/zbook/60-scenario.sh --config <file> --force`, then the Jetson step again. Notes:
- `[scenario] id` must stay equal to `[site] incident_id` in `edge-eye.toml` and `sar.toml`, and `[site]` centre must match both configs (P1-01 checks this).
- `seed` changes the layout, statuses and looks; same seed = same scenario.
- **Real buildings:** `scripts/zbook/60-scenario.sh --osm` downloads building outlines around the centre from OpenStreetMap (Overpass API, internet needed, once) and places the collapses on real buildings. Building data © OpenStreetMap contributors, ODbL.

## Models and known limits

- **Thermal.** Living skin = air + α·(37 °C − air), α 0.55–0.80; deteriorating victims lose α at 0.03–0.08 per hour; deceased victims cool exponentially from their temperature at death (k 0.15–0.35 per hour). Clothing and hair carry part of the skin temperature; debris heats with the sun according to its absorptivity, slope and shade, building up over the first hours after sunrise. Air follows a daily cycle (3 °C at 06:00, 14 °C at 15:00).
- **2.5D world.** Height field plus top-down colour: good for nadir and moderately oblique views (pitch −90° to about −45°); no overhangs. Shadows are computed from the height field.
- **Flat-ground geolocation.** Pixels are geolocated onto the ground plane; on top of a 6 m pile, an object at the image edge can be placed up to about 4 m off. Phase 3 compensates with the pile height.
- **Architecture note.** The ZBook (x86-64) and the Jetson (ARM) can differ in the last bit of floating-point results, so layers rendered on each machine may differ by a pixel here and there; their checksums are only compared on the machine that rendered them. Use `65-push-scenario.sh` for byte-identical copies.

## Phase 0 follow-ups from your acceptance run

- **CHK-06 showed a 161 ms median receive delay** with a 2.8 ms round trip: the Jetson's clock was still converging to the ZBook. On the Jetson:
  ```bash
  chronyc sources -v      # the ZBook's address should be marked '*'
  chronyc tracking        # 'System time' should be well under 0.01 s after a few minutes
  ```
  If the ZBook is not marked `*`, check that UDP port 123 is allowed on the ZBook (`sudo ufw allow 123/udp` if ufw is active) and re-run `scripts/jetson/30-install.sh --zbook-ip <ip>`. The `LAT ms` column in `sar-monitor` then drops to a few milliseconds.
- **zenohd admin port**: step P1-Z3 above.

## What to send back

The two `RESULT:` lines (ZBook and Jetson) and the scenario hashes both machines printed. If something failed, the `COPY FROM HERE … COPY TO HERE` block. Impressions from the preview page are welcome — they steer the realism work in phase 2.
