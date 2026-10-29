# Phase 3 — The onboard pipeline (edge-eye)

**Goal.** Each simulated drone flies a survey mission over the collapse sites and looks for people with the phase 2 model on the Jetson GPU. A thermal check tells living from dead, and the drone works out where each body part is on the map and how sure it is. It follows parts across frames and sends compact reports (crops, outline, temperature, position with uncertainty) to the ZBook within its radio budget. When the link drops, it keeps the reports and sends them later. The ZBook stores and acknowledges every report and measures the whole chain against the simulation's ground truth.

**Accepted when** `scripts/jetson/90-phase3-test.sh` **and** `scripts/zbook/90-phase3-test.sh` both end with `RESULT: PASS` (warnings allowed), and you have agreed with the numbers they print. The plan's target is: *detections reach the ZBook within budget, with a median position error under 5 m in simulation.*

**Time needed.**
- ZBook: about 15 minutes of commands, plus the 10–15 minute acceptance run.
- Jetson: about 10 minutes, plus the 1–2 minute self-test.

**Disk.**
- Jetson: up to 200 MB per drone for the report queue (normally almost empty).
- ZBook: about 5 KB per stored report with its crops (a 15-minute run stores about 1–2 MB).

> **Everything here is simulated.** The camera images, thermal images, GPS errors and victims are generated. The model has only seen procedurally drawn people. Nothing here is a validated casualty detector.

---

## What this phase adds

| Piece | What it does |
|---|---|
| **edge-eye** perception (Jetson) | Per drone, at 1 frame/s:<br>1. renders the virtual colour and thermal frame (from the drone's true pose; the reported pose has drifting GPS and heading errors);<br>2. runs the TensorRT model (shared by the three drones);<br>3. reads the skin temperature under every mask against its surroundings, and finds warm spots that no mask covers;<br>4. geolocates each part with an uncertainty (CEP) from the pose errors;<br>5. follows parts across frames, so a part is reported once and updated when it changes;<br>6. packs a report;<br>7. queues it on disk until the ZBook acknowledges it. |
| survey missions | New behaviour `survey`. Each drone flies a lawnmower pattern over its sites, with lines sized to the camera footprint and 70 % side overlap. It then returns home and lands. scout-1 surveys every site from 40 m at 5 m/s. inv-1 (s04, s01) and inv-2 (s05, s02) fly at 15 m and 2 m/s. The missions take 5–6 minutes. |
| store-and-forward | A queue on disk per drone, in `/var/lib/rubblesar/edge/queue/<uav>/`, that survives restarts. Reports are re-sent until acknowledged, with backoff. During a long silence only a probe goes out every 5 s. Telemetry always goes first. When the backlog grows, the reports get smaller: smaller crops, then the thermal crop only, then metadata only. |
| **casualty-analyst** (ZBook, new service) | Receives every report and drops duplicates. It writes each report to `/var/lib/rubblesar/detections/<incident>/detections.jsonl`, with the crops as files, and acknowledges it only after it is on disk. Status: `casualty-analyst status`. |
| `edge-eye selftest` | Proves the pipeline on the Jetson alone: speed, positions, and the queue through a slow link with an outage (ST-01..05). |
| `sar-check phase3` | The acceptance run on the ZBook (P3-01..08). It restarts the missions, cuts the link and the budget on schedule, and scores what arrives against the simulation truth. |
| `sar-monitor` | A third table: pipeline speed, queue, payload level and the latest detections. |
| `sar-doctor` | DOC-J6 (phase 3 config on the Jetson), DOC-Z7 (casualty-analyst running). |
| phase 2 evaluation | Counts the pieces of one victim's part (a hand cut in two by a slab) as one object, which is how you judge it by eye. It adds person-level recall ("is someone there?"), results by size (0–25, 25–100, 100–400, 400+ px), a confusion table and `sar-infer eval --conf`. `90-phase2-test.sh` prints both counts. |
| dataset v2 config | `configs/dataset/v2.toml` labels the pieces of a part as one instance (optional retraining, below). |

The phase 0–2 programs are otherwise unchanged. The Jetson still runs everything natively, with no Docker.

---

## Before you start

### Your data folder (ZBook)

Datasets, training runs and model folders are in `data/` of the **phase 2** release folder. The phase 3 folder starts without one. Share it with a link. This is safe even while your 150-epoch run is writing there:
```bash
cd rubblesar-0.4.1-phase3
ln -s ../rubblesar-0.3.0-phase2/data data
ls data/runs/          # your runs, including rubblesar-v1-yolo11s-seg-e150 when it is done
```
If you forget, `80-dataset.sh`, `81-train.sh` and `82-evaluate.sh` stop with `E-SETUP-DATA` and print this command with your paths.

### Which model (optional)

The acceptance run measures whatever model is installed as `current`, so phase 3 does not have to wait for the 150-epoch run. If that run turns out better (compare the `test` lines at the end of `81-train.sh`, or run `90-phase2-test.sh` after installing it), install it the phase 2 way, before or after the acceptance:
```bash
# ZBook (phase 3 folder, after the data link)
scripts/zbook/82-evaluate.sh --run data/runs/rubblesar-v1-yolo11s-seg-e150
scripts/zbook/85-push-model.sh --jetson <user>@<jetson-ip>
# Jetson
scripts/jetson/80-engine.sh --from ~/rubblesar-data/models/<name printed by 85-push-model>
sudo systemctl restart edge-eye
```

### Jetson power mode

Keep MAXN SUPER and `jetson_clocks` as in phase 2. Three drones share the GPU, and the simulation renders on the CPU.

---

## Steps

Unpack the release **next to** the phase 2 folder on both machines. Your `/etc/rubblesar` configs, the installed scenario and the models are kept.
```bash
tar -xzf rubblesar-0.4.1-phase3.tar.gz && cd rubblesar-0.4.1-phase3
```

| Step | Machine | Command | What it does | Expect |
|---|---|---|---|---|
| P3-Z1 | ZBook | `ln -s ../rubblesar-0.3.0-phase2/data data` | shares your datasets and runs (above) | — |
| P3-Z2 | ZBook | `scripts/zbook/20-build.sh` | rebuilds and runs all tests | `DONE` |
| P3-Z3 | ZBook | `scripts/zbook/30-install.sh` | installs the programs and the **casualty-analyst** service (account `rubblesar`, store `/var/lib/rubblesar/detections`). Your `sar.toml` is kept; the `[analyst]` defaults apply without a section | `ok: casualty-analyst is running`, `DONE` |
| P3-Z4 | ZBook | *optional:* `scripts/zbook/90-phase2-test.sh` | the phase 2 numbers again, now also counted per victim's part, per person and by size | `RESULT: PASS` |
| P3-J1 | Jetson | `scripts/jetson/20-build.sh` | native rebuild with TensorRT, all tests | `DONE` |
| P3-J2 | Jetson | `scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config` | installs, **replaces** `/etc/rubblesar/edge-eye.toml` with the phase 3 one (survey missions, `[perception]`, `[outbox]`; your old file is kept as `edge-eye.toml.bak.<time>`), creates the queue folder, adds the service account to the GPU groups (`video`, `render`) and loads the model as the service would before restarting edge-eye | `ok: the model loads on the GPU as the service runs it`, `ok: edge-eye is running and connected` |
| P3-J3 | Jetson | `scripts/jetson/90-phase3-test.sh` | `edge-eye selftest` (1–2 min), then `edge-eye status` | `RESULT: PASS`; the status shows `survey` and the pipeline |
| P3-Z5 | ZBook | second terminal: `sar-monitor` | live view while the run goes (optional) | three tables |
| P3-Z6 | ZBook | `scripts/zbook/90-phase3-test.sh` | **the acceptance run, 10–15 minutes.** Keep both machines awake | `RESULT: PASS`, `PHASE 3 checks finished` |

Without `--force-config` your old Jetson config stays. The drones then hover without looking, and the installer warns that `[perception]` is missing. If you had edited the old file, copy your changes from the `.bak` file into the new one. The ZBook address lives in `/etc/rubblesar/edge-eye.env`, not in the config.

`--quick` on P3-Z6 is a shorter outage (30 s) and a shorter budget cut (60 s), about 6 minutes in all. Use it to try the chain; the full run is the acceptance.

### What to send back

Both `90-phase3-test.sh` runs print a `COPY FROM HERE … COPY TO HERE` block **even when everything passes**. Send both: the numbers (position error, victims found, delay, speed) are what we agree on. If any other step fails, send its block.

---

## What the tests prove

### Jetson: `edge-eye selftest` (no ZBook needed)

It runs 20 frames over victims at 40 m (scout) and 15 m (investigator) through the installed model, on the drones' real code path, without the radio.

| Check | Proves | Pass |
|---|---|---|
| ST-01 | scenario and model load | the installed scenario opens and the model loads (TensorRT engine); `--oracle` warns |
| ST-02 | pipeline speed per frame (model, thermal, geolocation, tracking, packing; *not* the simulation's rendering) | median ≤ 250 ms (WARN ≤ 600 ms) |
| ST-03 | reports are well formed and land near the true body parts | median position error < 5 m |
| ST-04 | store-and-forward: at least 300 reports through 64 kbit/s with a 60 s outage on a virtual clock | every report delivered once, within the budget, smaller payloads during the backlog |
| ST-05 | the queue survives a restart | every queued report is found again after reopening the queue from disk |

### ZBook: `sar-check phase3` (the acceptance run)

The run restarts all three missions from the start, with tracks and queues cleared. At +90 s every drone's link goes silent for 60 s. At +180 s every budget drops to 32 kbit/s for 90 s. The run ends when the missions are done and every queue is empty, up to 15 minutes. It also listens to the simulation's truth channel to know what each camera actually saw.

| Check | Proves | Pass / warn |
|---|---|---|
| P3-01 | three drones on `survey`, the **model** (not the test oracle) loaded, casualty-analyst running | FAIL if a drone has no pipeline or uses the oracle |
| P3-02 | every report decodes. Position, CEP, summary and outline are present; crops decode as JPEG and 16-bit PNG and fit the payload level. Repeats after a lost acknowledgement are counted | FAIL on any malformed report |
| P3-03 | no drone's radio exceeded its budget: every 10 s window ≤ budget + 10 % burst + one message | FAIL above that |
| P3-04 | position error of every reported body part to the nearest true part of its class | **median < 5 m**. Reports more than 10 m from any true part are counted as false. Coverage of the reported CEP is shown (about 50 % is right) |
| P3-05 | victims the cameras saw with a report within 3 m, colour or thermal | WARN below 70 % |
| P3-06 | nothing lost: no gaps in each drone's report numbers, nothing discarded, queues empty at the end, how fast it caught up after the outage, and every report that arrived also stored by casualty-analyst | FAIL on a gap or a report not stored |
| P3-07 | delay from the drone confirming a part to its arrival on the ZBook, on a healthy link | median ≤ 2 s (WARN ≤ 5 s) |
| P3-08 | Jetson pipeline speed under the full three-drone load, from the drones' own telemetry | median ≤ 250 ms (WARN ≤ 600 ms); WARN if fewer than 80 % of the frames could be taken |

P3-08 also prints how long the **simulation** spends rendering each frame. On the Jetson CPU that may be 1–2 s with three drones, so fewer than 1 frame/s may be taken. That is a cost of the simulation (a real camera delivers frames for free), and it only warns. The survey overlap tolerates it.

---

## How a report looks

Each report is a `sar.v1.Detection` (`proto/sar/v1/detection.proto`) published on `sar/<incident>/uav/<uav>/detection`:

| Field | Content |
|---|---|
| `detection_id`, `track_id`, `revision`, `reason` | e.g. `inv-1-mgy5x1k2-0007.2`: drone, run token (when edge-eye started, so ids never repeat after a restart), track number, and the 2nd report about it; reason NEW or UPDATE (position improved, class changed, warm/cold changed, much more seen) |
| `part`, `confidence`, `fused_confidence`, `class_votes` | hand, arm, leg, head, torso, or WARM_SPOT (warm but no mask); model confidence, and the confidence after repeated sightings and the thermal cue |
| `position`, `cep_m`, `area_m2`, `observations`, `range_m` | latitude/longitude; radius with 50 % chance of containing the part; visible area; sightings; camera distance |
| `thermal` | skin and surrounding temperature, difference, pixels read, warm or not, or why it could not be read |
| `visible_crop`, `thermal_crop`, `outline` | JPEG crop of at most 128 px (from at least a 96 px region); 16-bit PNG of the thermal crop in deci-kelvin; outline of at most 32 points relative to the crop |
| `summary` | one line for people, e.g. `Hand exposed, about 0.05 m2 visible, 27.4 C against 18.9 C around (+8.5 C), seen in 4 frames by inv-1 from 15 m, +-1.9 m` |
| `capture_pose`, `frame_id`, `sim_time_s` | the reported drone pose when the crop was taken |
| `payload` | FULL (about 3–5 KB), SMALL (64 px crop), THERMAL_ONLY, METADATA (under 1 KB) |

**Order on a busy link:** new, warm, fairly confident parts first (most likely a living person), then other new parts, then updates, then thermal-only warm spots. **Payload by backlog:** when the queue holds more than 10 s of the drone's budget, reports go out SMALL; beyond 30 s, THERMAL_ONLY; beyond 60 s, METADATA.

The ZBook acknowledges stored reports on `sar/<incident>/uav/<uav>/ack`.

**On the ZBook:**
```
/var/lib/rubblesar/detections/<incident>/detections.jsonl     one JSON line per report (all fields but the images)
/var/lib/rubblesar/detections/<incident>/crops/<uav>/<id>.jpg  visible crop
/var/lib/rubblesar/detections/<incident>/crops/<uav>/<id>.png  thermal crop (16-bit, deci-kelvin)
```
For example:
```bash
casualty-analyst status
tail -n 3 /var/lib/rubblesar/detections/antakya-demo/detections.jsonl | python3 -m json.tool
xdg-open /var/lib/rubblesar/detections/antakya-demo/crops/inv-1/
```

---

## How the position and its uncertainty are worked out

A part's position is where the camera ray through the centre of its mask meets the ground. The uncertainty comes from:
- the drone's GPS error (1.5 m per axis, drifting with a time constant of about 30 s);
- the heading error (1°, drifting over about 60 s; it matters more the further the part is from the drone);
- the altitude error;
- gimbal jitter;
- the unknown height of the part on the rubble (1 m), which shifts it when seen at an angle.

The tracker averages the frame-to-frame errors over every sighting. The drifting errors are shared by sightings close in time, so they shrink only as the sightings spread over the drift time. The reported **CEP** is the radius that holds the true position half the time. Expect a median error of about 1.5–2.5 m, set mainly by the GPS. Each drone tracks on its own. Combining the drones' reports of the same person, which cancels much of the GPS error, is phase 4 on the ZBook.

---

## Watching it work

```bash
sar-monitor                                  # ZBook: drones, link, pipeline/queue table, latest detections
edge-eye status                              # Jetson: mission progress, frames/s, pipeline ms, queue, payload level
edge-eye budget --kbps 16 --uav inv-1        # Jetson: squeeze one drone and watch its payload level rise
edge-eye budget --kbps 256 --uav inv-1       # back to the release budget (a service restart also restores the config)
journalctl -u edge-eye -f                    # Jetson log
journalctl -u casualty-analyst -f            # ZBook log
```
When the missions are done, the drones land and wait. `sar-check phase3` restarts them. A service restart (`sudo systemctl restart edge-eye`) does too, and keeps any queued reports.

---

## New configuration

All keys, with defaults, are in [ENVIRONMENT.md](ENVIRONMENT.md#configuration-reference). The Jetson release config sets `[perception] enabled = true` and the survey missions.
- `[perception]`: model folder, confidence threshold, frame rate, thermal thresholds, how many sightings confirm a part, update interval.
- `[outbox]`: queue folder and size, acknowledgement timeout, backoff, probe.
- `[[uav]]`: `behaviour = "survey"`, `survey_sites`, `survey_speed_mps`, `side_overlap`, `yaw_noise_deg`, `frame_hz`.
- ZBook `[analyst]`: storage folder, crops on/off, acknowledgement batching, status port 127.0.0.1:9102.

`detector = "truth-oracle"` replaces the model with the simulation's own masks. It tests the pipeline and the link without a model: `edge-eye selftest --oracle`, or `sar-check phase3 --allow-oracle`. It is never accepted as the phase 3 result.

---

## Known limitations

- **Simulated sensing.** The colour and thermal frames are rendered from the scenario. The CSI cameras are not used (D13).
- **Flat-ground geolocation.** Height on the rubble is treated as uncertainty, not measured. Parts on top of a pile are placed a little off when seen at an angle; the CEP includes it.
- **One drone at a time.** Each drone tracks on its own. Two drones reporting the same hand give two reports. Merging them into one casualty with a better position is phase 4 (casualty fusion on the ZBook).
- **Rendering cost.** The simulation renders frames on the Jetson CPU, which limits the frame rate (P3-08 shows it). A real camera would not.
- **Plain TCP.** The link is unencrypted, as agreed for now (D10).

---

## Optional: retrain on dataset v2

The phase 2 dataset labels each piece of a part separately: a hand cut by a slab is two labels. The merged evaluation shows the model is often right about the person but splits the part. `configs/dataset/v2.toml` writes one label per victim's part, joining the pieces, and leaves out pieces under 3 px. Everything else is identical, so the two models can be compared directly:
```bash
scripts/zbook/80-dataset.sh --config configs/dataset/v2.toml             # data/datasets/rubblesar-v2, 15-25 min
tmux new -s train
scripts/zbook/81-train.sh --dataset data/datasets/rubblesar-v2 --epochs 150
scripts/zbook/82-evaluate.sh --run data/runs/rubblesar-v2-yolo11s-seg --dataset data/datasets/rubblesar-v2
scripts/zbook/90-phase2-test.sh                                          # compare with v1
```
Do this only if the phase 3 numbers call for it; it is not needed for acceptance.

---

## Command reference

```
edge-eye selftest [--frames 20] [--oracle] [--report-dir DIR]   Jetson: pipeline and queue test (exit 0 pass, 1 fail)
edge-eye status                                                 Jetson: includes the phase 3 table
sar-check phase3 [--quick] [--allow-oracle] [--max-minutes N] [--report-dir DIR]
casualty-analyst run|status|check-config|version
sar-infer eval --conf 0.25 ...                                  phase 2 evaluation at another confidence threshold
```
