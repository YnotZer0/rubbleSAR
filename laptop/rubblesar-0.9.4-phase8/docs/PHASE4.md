# Phase 4 — Casualties, triage and investigator drones

**Goal.** The ZBook turns the drones' reports into **casualty records**: one record per person, however many drones saw them and however many parts they showed, with a position, how sure it is, and a START triage colour from the thermal reading and from what a close look showed. Records that are uncertain trigger an **investigation**: mission-planner sends the nearest free investigator drone, which descends to 15 m over the casualty, holds still for 30 s, watches for movement and reports what it saw. The triage is then settled and compared with the simulation's ground truth.

**Accepted when** `scripts/jetson/90-phase4-test.sh` **and** `scripts/zbook/90-phase4-test.sh` both end with `RESULT: PASS` (warnings allowed), and you have agreed with the numbers they print. The plan's target is: *uncertain casualties trigger investigations; triage accuracy against ground truth is reported.*

**Time needed.**
- ZBook: about 15 minutes of commands, plus the 25–35 minute acceptance run.
- Jetson: about 10 minutes, plus the 4–5 minute self-test.

**Disk.** Unchanged from phase 3. Each acceptance run moves the previous run's reports and crops to an `archive/` folder (1–3 MB per run); delete old ones when you like.

> **Everything here is simulated, and the triage rules are illustrative.** The thresholds (what counts as warm, how long a close look must be) await medical sign-off (decision D9). Nothing here is a validated triage tool.

---

## What this phase adds

| Piece | What it does |
|---|---|
| **casualty-analyst** fusion (ZBook) | Merges the stored reports into casualty records, in two stages. First, per drone: parts seen together and close (2 m; 2.5 m for a hand and arm, or a head and torso) are one person, as long as they look like one (at most two hands, arms and legs, one head and torso; parts of one class within 1.5 m of each other count once, because a real model reports one leg as two or three pieces). In a hover, a part the model reported in only a few frames (at most a quarter of those of the best-seen part) joins a part followed throughout within 3 m; with none near it, it may support a casualty another window found but does not start one (debris taken for a part now and then). Then across drones and passes: each drone's records of a stretch of observation (a hover, or up to 30 s of a survey) share one GPS error, so the analyst finds the shift that best lines them up with the casualties already known and matches them one to one (Hungarian method), so two victims a few metres apart are not merged into one. The record's position averages the drones' errors, and its CEP shrinks accordingly. Ids (`C-007`) stay stable; when two records prove to be one person, one keeps its id and the other is published once more with `merged_into`. |
| **triage** (ZBook) | START colours from the readings, preferring close ones (table below). Warm means bare skin (hand, head) at 20–40 °C and at least 3 °C above its surroundings. When only clothing is read (arm, leg, torso), it must be at least 15 °C and 8 °C above the surroundings: clothing reads cooler than skin on the living, and sun-warmed clothing on a body that has cooled still reads up to about 7 °C above the air. Bare skin, when read, decides. A record is **provisional** until a drone has looked close up. The skin-temperature trend is worked out from close readings of one kind of part (a hand with a hand, never with a sleeve), one value per look (the median of its frames), from at least three looks over 10 minutes, as the median of the slopes between looks (one bad look does not swing it); faster than ±10 °C/h it is not reported (skin does not change that fast: the readings are not of the same thing). It is reported, not used for pass/fail. |
| **mission-planner** (ZBook, new service) | Creates an investigation task for every casualty that needs a close look: tier 1 provisionally Red (may be alive), tier 2 unknown (temperature unreadable, or cold seen from afar), tier 3 position too uncertain (CEP > 5 m). It gives each to the nearest free investigator that keeps 25 % battery after the task and the flight home. When no drone is free, a more urgent task takes a drone still flying to a less urgent one (which waits for the next free drone). It follows each task in the drones' telemetry, gives it back to the queue when a drone refuses, does not answer, aborts or stops making progress, and publishes the task list, which casualty-analyst reads for the outcome. At most 2 completed looks per casualty. Status: `mission-planner status`. |
| **investigator drones** (Jetson) | New behaviour `standby`: inv-1 and inv-2 wait at home. A task makes the drone take off, fly at 10 m/s, hold 30 s over the target at 15 m with a fixed heading (drifting about 0.15 m, as a real one does), report every part it saw within 6 m of the target (reason `INVESTIGATED`, with the movement verdict and the task id), then wait 20 s for another task before flying home. The scout still surveys every site from 40 m. |
| **edge tracking** (Jetson) | Every part seen during a hover carries the hover's task (not only those in the middle of the frame), so casualty-analyst registers the hover's records together. Each hover has its own tracks: a part seen again from another place gets a new track instead of joining the old one, because positions from two viewpoints disagree by the unknown height of the rubble (a part on a 4 m pile, 6 m off nadir from 15 m, lands about 1 m out, the other way from the other side) and casualty-analyst matches the hovers' records. Outlines of one class that mostly overlap in a frame are one part (a real model sometimes outputs a whole leg and its lower half). The object-height error of an off-centre sighting no longer shrinks as frames from the same place are averaged, so such a sighting carries a larger CEP. |
| **movement detector** (Jetson) | While hovering, each frame is registered to the hover's first frame (the drone drifts), then each part again against its own surroundings (a part high on the rubble shifts differently: parallax). A frame counts as *moved* when more than 12 % of the part's outline shifts by more than 2 px **and the picture changed where it did**: a real model's outlines change on their own (a forearm's outline takes in the hand in one frame and not the next, a part's class flips, a track picks up the neighbouring part), so the pixels where the outlines disagree are compared with the hover's first frame (within 2 px, after matching the exposure on the ring of rubble around the part), and more than 12 % of them, beyond the ring's own share, must have changed. An outline that is nowhere near where it was (another object, or a frame that did not register) does not count. Only outlines in the middle 70 % of the frame are compared: near the edge they are cut by the frame or seen past nearby debris at an angle, which changes as the drone drifts. Each hover gives its own verdict per part (2 such frames), and casualty-analyst keeps every hover's. casualty-analyst calls a casualty moved when, in one hover aimed at it, its parts moved in 2 frames in all (`[analyst.triage] moved_frames`): a limb moving for a second or two shows in one frame of the hand and the same frame of the arm. Hovers aimed at another casualty do not count for this one (a neighbour in the picture was not watched). |
| `edge-eye selftest` ST-06 | Hovers 20–30 s at 15 m, as an investigation does (target off by up to a metre, any heading, drifting), over up to two moving and five still victims of the installed scenario with the installed model, and shows for each part how much changed, to set `movement_change` for the model. |
| `sar-check phase4` | The acceptance run on the ZBook (P4-01..08): archives the previous run, restarts the missions, waits until the survey is done and every task has finished, and scores the casualty records against the scenario. |
| `sar-monitor` | Two more tables: the casualties and the investigation tasks. |
| `edge-eye status` | A task table: each drone's task, state, seconds held, parts seen, movement. |
| `casualty-analyst status` / `reset` | Status now lists every casualty with its colour and why. `reset --yes` archives the run and starts afresh. |
| `sar-doctor` | DOC-J7 (a drone on standby on the Jetson), DOC-Z8 (mission-planner running). |

The phase 0–3 programs are otherwise unchanged. The Jetson still runs everything natively, with no Docker.

### Triage rules

| What the readings show | Status | Colour | Needs a close look |
|---|---|---|---|
| moved while watched close up | warm, responsive | **Yellow** | no |
| warm, watched close up for 20 s, no movement | warm, unresponsive | **Red** | no |
| warm, not yet seen close up | warm, unresponsive | **Red** (provisional) | yes, tier 1 |
| warm but cooling faster than 0.5 °C/h over 10 min or more | deteriorating | **Red** | if not seen close up |
| at ambient after a close look | no heat | **Black** | no |
| cold, seen only from afar | unknown | **Grey** (provisional) | yes, tier 2 |
| temperature not readable | unknown | **Grey** (provisional) | yes, tier 2 |
| two close looks that saw nothing there | not found | **Grey** | no |

Green (walking wounded) does not occur: people trapped in rubble cannot walk. A record placed less certainly than 5 m (CEP) also asks for a look (tier 3).

---

## Before you start

### Your data folder (ZBook)

As in phase 3, link the phase 2 data folder into the new release folder:
```bash
cd rubblesar-0.5.1-phase4
ln -s ../rubblesar-0.3.0-phase2/data data
```

### The phase 3 acceptance is not repeated

The phase 4 Jetson config puts the investigators on standby, so `sar-check phase3` (which expects every drone to survey) would now fail P3-01. Phase 3 is already accepted; its checks of the link, budget and storage are part of P4-08.

### Updating from 0.5.0-phase4

Unpack 0.5.1 next to 0.5.0 on both machines and run the same steps as below, **without** `--force-config`: 0.5.1 changes no Jetson setting, and its two new ZBook settings (`[analyst.triage] moved_frames`, `[analyst.fusion] duplicate_m`) have built-in defaults, so the configs you installed for 0.5.0 stay as they are. On the ZBook, link the data folder as before (`cp -P ../rubblesar-0.5.0-phase4/data .` copies the link). Then P4-Z2, P4-Z3 (without `--force-config`), P4-J1, P4-J2 (without `--force-config`), P4-J3 and P4-Z5. What changed is in `CHANGELOG.md`.

---

## Steps

Unpack the release **next to** the earlier folders on both machines. Your `/etc/rubblesar` configs, the installed scenario and the models are kept.
```bash
tar -xzf rubblesar-0.5.1-phase4.tar.gz && cd rubblesar-0.5.1-phase4
```

| Step | Machine | Command | What it does | Expect |
|---|---|---|---|---|
| P4-Z1 | ZBook | `ln -s ../rubblesar-0.3.0-phase2/data data` | shares your datasets and runs | — |
| P4-Z2 | ZBook | `scripts/zbook/20-build.sh` | rebuilds and runs all tests (about 10 minutes now: the phase 4 tests fly a whole investigation in simulation) | `DONE` |
| P4-Z3 | ZBook | `scripts/zbook/30-install.sh --force-config` | installs the programs, **replaces** `/etc/rubblesar/sar.toml` with the phase 4 one (new sections `[analyst.fusion]`, `[analyst.triage]`, `[planner]`; your old file is kept as `sar.toml.bak.<time>`), moves the phase 3 reports aside (to `/var/lib/rubblesar/detections/<incident>/archive/<time>-before-phase4/`, kept, so the casualty records start from phase 4 runs), restarts casualty-analyst and installs the **mission-planner** service (account `rubblesar`, tasks in `/var/lib/rubblesar/planner`) | `ok: casualty-analyst is running`, `ok: mission-planner is running`, `DONE` |
| P4-J1 | Jetson | `scripts/jetson/20-build.sh` | native rebuild with TensorRT, all tests (longer than before: the phase 4 tests fly whole investigations in simulation) | `DONE` |
| P4-J2 | Jetson | `scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config` | installs and **replaces** `/etc/rubblesar/edge-eye.toml` with the phase 4 one: inv-1 and inv-2 on `standby` at 10 m/s, the movement settings (your old file is kept as `edge-eye.toml.bak.<time>`) | `ok: edge-eye is running and connected` |
| P4-J3 | Jetson | `scripts/jetson/90-phase4-test.sh` | the config, `edge-eye selftest` ST-01..06 (4–5 min), then `edge-eye status` | `RESULT: PASS`; the status shows inv-1 and inv-2 `standby` |
| P4-Z4 | ZBook | second terminal: `sar-monitor` | live view while the run goes (optional) | five tables |
| P4-Z5 | ZBook | `scripts/zbook/90-phase4-test.sh` | **the acceptance run, 25–35 minutes.** Keep both machines awake | `RESULT: PASS`, `PHASE 4 checks finished` |

Without `--force-config` on the ZBook, your phase 3 `sar.toml` stays and the defaults of the new sections apply (the same values as the release file), so that is also fine. On the Jetson `--force-config` is needed: without it the investigators keep surveying and nothing waits for tasks (the installer and DOC-J7 warn). If you had edited a file, copy your changes from the `.bak` file into the new one.

`--max-minutes N` on P4-Z5 changes the time limit (default 45). The run ends by itself as soon as the survey is done, no task is open and the queues are empty for 45 s.

### What to send back

Both `90-phase4-test.sh` runs print a `COPY FROM HERE … COPY TO HERE` block **even when everything passes**. Send both: the numbers (records per victim, positions, investigations, triage, movement) are what we agree on. If any other step fails, send its block.

---

## What the tests prove

### Jetson: `edge-eye selftest` (no ZBook needed)

ST-01..05 as in phase 3. New:

| Check | Proves | Pass |
|---|---|---|
| ST-06 | movement while hovering: 30 frames at 15 m over each of up to two victims of the installed scenario whose limb swings now and then, 20 over each of up to five still ones (living and dead), as an investigation hovers (target off by up to a metre, any heading, the drone drifting), with the installed model | at least one moving victim reported moving and no still one. WARN otherwise, with each part's figures (in how many frames it changed, the largest change) and a suggested `movement_change` between the two. A movement lasting a second or two can fall between frames, so not every moving victim is caught |

### ZBook: `sar-check phase4` (the acceptance run)

The run first archives casualty-analyst's previous run (to `<store_dir>/<incident>/archive/<time>-before-phase4/`) and forgets mission-planner's tasks, then restarts all three drones (tracks and queues cleared). The scout surveys every site from 40 m; from its first reports on, casualty-analyst builds records and mission-planner sends inv-1 and inv-2. Progress is printed every 30 s. At the end, every casualty record is matched one to one to the scenario's victims by position (within 3.5 m of the victim's body or exposed parts).

| Check | Proves | Pass / warn |
|---|---|---|
| P4-01 | the scout surveys, the investigators are on standby (phase 4 edge-eye), the **model** is loaded, casualty-analyst and mission-planner are running their phase 4 versions | FAIL if a piece is missing |
| P4-02 | one casualty record per victim the cameras saw: victims without a record, extra records for one victim, victims merged into a neighbour's record, records near no victim (often a decoy: a sun-heated metal sheet, a smouldering spot, or debris the model took for a body part) | PASS: ≥ 80 % found, ≤ 10 % extra, ≤ 5 % merged; FAIL below 60 %, above 25 % or 15 %. WARN when over 25 % of the records match no victim; records the system already closed ("nothing found on 2 close looks") are listed but not counted for that |
| P4-03 | each victim's record position against the victim | median < 4 m (WARN < 6 m); the share within their CEP is shown (about 50 % is right) |
| P4-04 | every record that needed a close look got one (or used up its looks); how soon a drone was assigned, by tier | WARN if one was missed (and why) |
| P4-05 | the investigations were flown: done, held at the task height for the hover time, saw something near the target; each task's drone, frames and result | WARN on failed or unfinished tasks or a wrong height (median more than 3 m off; the reported height carries the GPS height error) |
| P4-06 | **triage accuracy against the ground truth**: each victim's colour against the colour the scenario expects (alive and responsive: Yellow; alive and unresponsive or deteriorating: Red; deceased: Black), as a table, with the reason for each difference, and the skin-temperature trends (reported only) | PASS ≥ 70 %, else WARN (a target to agree, not yet signed off) |
| P4-07 | movement close up: moving victims seen moving, still victims not | WARN below 70 % or above 20 % |
| P4-08 | the radio budget was kept while hovering and sending crops; no report missing; casualty-analyst stored every report that arrived | FAIL on a busy window, a gap or a report not stored |

---

## How a casualty record looks

Each record is a `sar.v1.Casualty` (`proto/sar/v1/casualty.proto`), published on `sar/<incident>/casualty/<id>` when it changes; the whole list goes out on `sar/<incident>/casualties`.

| Field | Content |
|---|---|
| `casualty_id`, `revision` | `C-007`, revision 3 |
| `position`, `cep_m` | fused position; radius with 50 % chance of containing the person |
| `parts`, `uav_ids` | every part (drone, track, class, position, closest range) and the drones that saw them |
| `thermal` | skin and surrounding temperature (median of the close readings, else all), warm or not, number of readings, trend in °C/h over how long |
| `movement` | moved or not, seconds watched close up |
| `status`, `triage`, `provisional`, `reason` | e.g. `WARM_UNRESPONSIVE`, Red, provisional, "warm; not yet watched close up" |
| `confidence`, `p_alive` | detection confidence; a rough chance of a living person |
| `needs_investigation`, `investigation_reason`, `investigations`, `not_found_looks` | whether it needs a close look and why; looks done; looks that saw nothing |
| `summary` | one line, e.g. `C-007: hand, arm seen by inv-1, scout-1; 22.4 C against 7.8 C (+13.7); still for 30 s -> Red: warm, no movement in 30 s watched close up, +-1.4 m` |
| `merged_into` | set on the last message of a record merged into another |

Tasks are `sar.v1.Task` in a `TaskList` on `sar/<incident>/tasks`: id `T-<run>-<n>` (the run part changes with each mission-planner reset, so a new run never reuses an id a drone still shows), casualty, tier, state (queued → assigned → en route → on station → done; or failed, cancelled), drone, target, distance and battery estimate, result (frames, parts seen, moved) and a note in words.

**On the ZBook:**
```
/var/lib/rubblesar/detections/<incident>/detections.jsonl        reports (phase 4 adds task_id, movement, min_range_m)
/var/lib/rubblesar/detections/<incident>/casualties-state.json   casualty ids, investigation outcomes (kept across restarts)
/var/lib/rubblesar/detections/<incident>/archive/<time>/        earlier runs
/var/lib/rubblesar/planner/<incident>.json                       mission-planner's tasks
```
```bash
casualty-analyst status                                    # reports per drone, then every casualty with colour and why
curl -s 127.0.0.1:9102/v1/casualties | python3 -m json.tool | less
mission-planner status                                     # drones (battery, task) and every task
curl -s 127.0.0.1:9103/v1/tasks | python3 -m json.tool | less
```

---

## Watching it work

```bash
sar-monitor                          # ZBook: drones, link, pipeline/queue, detections, casualties, investigations
casualty-analyst status              # ZBook: the casualty table
mission-planner status               # ZBook: drones and tasks
edge-eye status                      # Jetson: mission, pipeline, and each drone's task (held for, parts, movement)
journalctl -u mission-planner -f     # ZBook: "investigation queued/assigned/done"
journalctl -u casualty-analyst -f    # ZBook: new casualties, merges
journalctl -u edge-eye -f            # Jetson: "investigation task" as each drone takes one
```
Outside an acceptance run the system works the same way: restart the scout (`sudo systemctl restart edge-eye`) and the investigators are sent as records appear. `casualty-analyst reset --yes` and `mission-planner reset --yes` start afresh by hand.

---

## How the records are merged

**Why it is hard.** Each drone's GPS is off by about 1.5 m per axis, and the error drifts over about 30 s. Two drones looking at the same person therefore place it typically 2–3 m apart, which is about the spacing of victims in a collapse (3–12 m here, median 5 m). Position alone cannot tell "the same person seen by two drones" from "two neighbours".

**What helps.** Within one stretch of observation (a hover, or a short part of a survey), all of a drone's records share nearly the same GPS error. So the analyst does not compare records one by one. It tries each shift that would line up the drone's records with the casualties already known and keeps the one that explains them best (the residuals, the size of the shift against what two GPS errors make likely, the casualties in view that it did not see, and whether the body parts and temperatures agree). The records are then registered to a common frame for later comparisons. The published position still averages the drones' own positions, so independent GPS errors cancel. A hover's target also counts: what a drone sees where it was sent is most likely the casualty it was sent to.

**How well.** `go test ./internal/casualty` measures it on 200 random sites of 3–6 victims at least 3.2 m apart (2.5 m in one case), with every drone's GPS error, parts hidden 40 % of the time and 8 % of the classes wrong. With all victims looking alike (the worst case: only positions tell them apart), about 2–4 % of the people end up merged with a neighbour or split over two records; with varied body parts and temperatures, as in the scenario, 1.5–3 %. Re-merging everything each second keeps the ids stable: a record keeps the id it shares most parts with.

---

## New configuration

All keys, with defaults, are in [ENVIRONMENT.md](ENVIRONMENT.md#configuration-reference).
- Jetson `[[uav]]`: `behaviour = "standby"`, `task_loiter_s`, `hover_wander_m`; the investigators' `speed_mps` is now 10.
- Jetson `[perception]`: `detection_sigma_m` (1.0 m: the position error not explained by the pose, calibrated from your phase 3 runs, added to every CEP), `debris_height_m` (2.5 m: parts are placed where the camera ray meets the top of a typical pile, not the ground; otherwise a part high on a pile lands a metre or more off, on opposite sides for two drones looking from opposite sides), `movement_change`, `movement_tolerance_px`, `movement_min_frames`.
- ZBook `[analyst.fusion]`: how parts and records are joined.
- ZBook `[analyst.triage]`: the warm thresholds, what a close look is, cooling, CEP, empty looks.
- ZBook `[planner]`: investigation height, hover time and radius, battery reserve, looks per casualty, timeouts, which drones take tasks.

`detector = "truth-oracle"` (the simulation's own outlines instead of the model) tests fusion, tasking and triage without the model: `sar-check phase4 --allow-oracle`. It is not accepted as the phase 4 result.

---

## Known limitations

- **Movement must show in the outline.** A swinging hand, arm, leg or foot is seen. A head turning barely changes its outline and is usually missed, and so is a hand alone twitching through a small angle. Those victims stay **Red** (warm, still) instead of Yellow: a higher priority, which is the safe side. P4-07 reports how many.
- **Short movements can be missed.** The scenario's moving victims move one limb for 1.5–3 s every 6–20 s; at one frame a second a 30 s hover sees each movement in one or two frames. A victim whose movement falls between frames, or happens once, stays **Red** (the safe side).
- **Movement thresholds depend on the model.** A changed outline only counts when the picture changed too, so a model's outline jitter is not movement; ST-06 shows the figures for the installed model.
- **Decoys.** A sun-heated metal sheet or a smouldering spot can look like a warm body with no part visible. It becomes a provisional Red record and gets a close look; when no body part shows up it stays Red ("warm, no body part seen"), so a rescuer still checks it. P4-02 counts these.
- **GPS-bound positions.** Fusion averages the drones' GPS errors but cannot remove what they share. Expect about 1–2 m.
- **Battery model.** Linear drain over the endurance, a fixed swap time at home, return home at 25 %. mission-planner keeps a 25 % reserve on top of the trip.
- **No obstacle avoidance.** The investigators fly straight at 15 m. Routing around obstacles is phase 5's road network, not flight planning.
- **Illustrative triage.** The colours follow START as agreed (D9), with thresholds awaiting medical sign-off.

---

## Command reference

```
casualty-analyst run|status [--json]|reset --yes|check-config|version
mission-planner  run|status [--json]|reset --yes|check-config|version
edge-eye selftest [--frames 20] [--oracle] [--report-dir DIR]   ST-01..06
edge-eye status                                                 includes the task table
sar-check phase4 [--max-minutes 35] [--allow-oracle] [--report-dir DIR]
sar-monitor [--once 10s]
```
APIs (127.0.0.1 only): casualty-analyst `GET /v1/status`, `GET /v1/casualties`, `POST /v1/reset`; mission-planner `GET /v1/status`, `GET /v1/tasks`, `POST /v1/reset`.
