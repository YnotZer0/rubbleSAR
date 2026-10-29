# Phase 7 — The live operator view

**Goal.** People at the command post who do not carry ATAK or WinTAK see the incident as it happens, in a browser: the map with the drones, the casualties in their triage colour, the rescue teams and their routes; what the drones report, with the crops they sent; the teams and the chat; and a plain-language timeline of what happened when.

**Accepted when** `scripts/zbook/90-phase7-test.sh` ends with `RESULT: PASS` (warnings allowed), and you have watched a run in the operator view and agree it reads well.

**Time needed.** ZBook: about 20 minutes of commands, plus the 30–50 minute acceptance run (5–15 with `--quick`). Jetson: 10 minutes (needed since 0.9.3, see below).

Phases 7 and 8 come in one release, **0.9.4-phase8**. This document covers the operator view and its acceptance; [PHASE8.md](PHASE8.md) covers recording the demo video, which builds on it. Install once, then accept phase 7 first.

> **Everything here is simulated.** The incident, the victims and the teams are simulated; triage thresholds and extrication times are illustrative and await review (decision D9). sar-web is plain HTTP without encryption or login: anyone on the network can open the operator view (decision D10). Use it on a private network only.

---

## What this phase adds

| Piece | What it does |
|---|---|
| **Operator view** (`http://<ZBook>:8090/ops/`) | One page, any browser on the network, no install. It leads with **what the drones found** (the left half of the screen): one card per casualty record with the colour and thermal pictures the drone sent, the body parts the model outlined and how sure it was, the temperature against the rubble, movement seen close up, and what is being done for that person (team on the way, freed, close look under way), most urgent first; a strip of the latest reports above them. Beside it, a smaller **map** (the offline incident map, the rubble, closed streets, the command post, drones with their recent trails, the scout's planned survey as a numbered dashed line, casualties in their triage colour, close-look lines, teams and their routes; the collapses named after their streets) and tabs: **What happened** (the timeline in plain words: "C-004 found by scout-1, provisional Red: …"; "TEAM-2 sent to C-004 (Red): 340 m, about 6 min …"; "scout-1 not heard for 6 s: link lost; its reports wait in the drone's on-board queue"; "scout-1 queue empty: every report held during the blackout reached the command post and was acknowledged"), **Chat with the teams**, **Drones** (task, battery, link, reports waiting on board), **Rescue teams**. Click a card for the full record. Light and dark look; works on a tablet. |
| **Live picture in sar-web** | sar-web subscribes to the bus (zenohd): telemetry, detection packets with their crops, casualty lists, tasks, dispatch state; seeded from the services' APIs when it starts, so a page opened mid-mission is complete. Pushed to the browsers as server-sent events (a full picture, then only changes; gzip). sar-web is now built with zenoh (`make build`) and starts after zenohd. |
| **A blackout seen as silence** | A drone sends nothing while its link is down, so the ZBook notices the silence (6 s), marks the drone "not heard", and says when it is heard again and when its queue has drained, with the count. |
| **Run recording, richer** | The run's `events.jsonl` (phase 6) now also keeps the casualty details, every detection report with its crops and where it sat in the frame, the simulation's truth of those frames, each drone's link and perception figures and the teams' state. Phase 8 replays and re-renders from it. |
| `sar-check phase7` | The acceptance run: the whole phase 6 run, with the operator view served and connected (P7-01), its picture compared with casualty-analyst, mission-planner and rescue-dispatch every 20 s throughout the run (P7-02), the event stream (P7-03), the crops (P7-04) and the timeline against the after-action report (P7-05). `scripts/zbook/90-phase7-test.sh`. |

API (read-only, on the network): `GET /v1/scene` (incident, map, rubble, teams, model), `/v1/picture`, `/v1/timeline`, `/v1/stream` (server-sent events), `/v1/crop/<id>`, `/v1/thermal/<id>`. Phase 8 adds `?src=replay` and two endpoints accepted from the ZBook itself only.

---

## Before you start

- **0.7.1 or 0.7.2 accepted** (phase 6).
- Unpack into a new folder and link your data folder, as before:
```bash
tar -xzf rubblesar-0.9.4-phase8.tar.gz && cd rubblesar-0.9.4-phase8
ln -s ../rubblesar-0.3.0-phase2/data data
```

## Steps (ZBook)

| Step | Command | What it does | Expect |
|---|---|---|---|
| P7-Z1 | `scripts/zbook/20-build.sh` | rebuilds (sar-web now with zenoh) and runs all tests (about 15 minutes) | `DONE` |
| P7-Z2 | `scripts/zbook/30-install.sh` | installs the programs and the new sar-web service unit (starts after zenohd), restarts sar-web. **No `--force-config`**: no configuration changes in this release | `ok: sar-web is running`, the operator view's address, `DONE` |
| P7-Z3 | open `http://<ZBook-IP>:8090/ops/` on the WinTAK laptop or a tablet | the operator view | "Live" in the header; the map; drones listed if edge-eye runs |
| P7-Z4 | `rubblesar up`, then `scripts/zbook/90-phase7-test.sh` | **the acceptance run, 30–50 minutes**; watch it in the operator view meanwhile | `RESULT: PASS`, `PHASE 7 checks finished` |

### The Jetson

Since 0.9.3 the Jetson needs this release (or 0.9.3) too: the scout's survey order (the shortest round trip, and the order the operator view draws) is decided by edge-eye. Unpack it there, link the data folder, then `scripts/jetson/20-build.sh` and `scripts/jetson/30-install.sh --zbook-ip <ZBOOK-IP>`, as in phase 6. On an older edge-eye the scout flies the sites in the scenario's numbering and the operator view draws no planned survey; `rubblesar status` and P6-01 warn that the drones run older software than the ZBook.

### Upgrading from 0.9.3

ZBook only: P7-Z1 and P7-Z2 above, no `--force-config`. The Jetson can stay on 0.9.3 (nothing changed there); P6-01 then warns that the drones run other software, which rebuilding the Jetson with this release clears. Nothing else: the map, the storyboard and `demo.toml` stay as they are. For a phone or laptop away from the incident, add `at_command_post = true` to its team ([PHASE8.md](PHASE8.md#an-atak-phone-optional)).

### Upgrading from 0.9.0, 0.9.1 or 0.9.2

1. ZBook: P7-Z1 and P7-Z2 above (no `--force-config`).
2. Jetson: as above.
3. ZBook: `scripts/zbook/36-map.sh` draws the offline map again, with the collapses named after their streets; re-import the MBTiles on ATAK and WinTAK if you copied it to them (the streamed map source picks up the new tiles by itself).
4. ZBook, for the video: move your old storyboard aside (`mv ~/.config/rubblesar/storyboard.toml ~/.config/rubblesar/storyboard.toml.old`) and run `rubblesar demo init`: it writes the new storyboard and leaves `demo.toml` as it is. Copy back any wording you had changed.

### What to send back

The `COPY FROM HERE … COPY TO HERE` block of `90-phase7-test.sh` (printed even on a pass), and a screenshot or two of the operator view during the run, with anything that reads badly.

---

## Using the operator view

- **Cards** (left): the most urgent first (need a rescue team, then need a close look, then freed); "No team needed" (kept for recovery, or nothing there) is folded away: **show** opens it. Click a card: the map flies to it and the full record opens beside the map.
- **Map**: drag to move, scroll or `+`/`−` to zoom, **Fit** to show everything. Hover over anything for its details (Esc closes a record). The fading lines are the drones' last minute or so of flight; the numbered dashed line is the scout's planned survey (the shortest round trip from its home); the teams' routes follow the streets.
- **Header**: the incident, one sentence on the situation ("2 people need rescuing: 1 on scene. 2 freed so far."), the connection (**Live**, **Waiting** with the reason, **Reconnecting**), the mission clock, and the light/dark switch.
- **Casualty colours**: Red, Yellow, Grey (temperature unknown or cold from far away: needs a close look), Black; a ring = provisional (not yet seen close up); green = freed.
- `?theme=dark` or `?theme=light` in the address fixes the look (e.g. for a wall screen).

The page shows what the bus carries: if casualty-analyst, mission-planner or rescue-dispatch is down, its part of the picture stays as it last was (`rubblesar status` shows what is down).

---

## What the tests prove

### `sar-check phase7` (the acceptance run)

| Check | Proves | Pass / warn |
|---|---|---|
| P7-01 | sar-web serves `/ops/` (page, script, style, fonts), the scene (incident, collapse sites, rubble, teams, the map) and a live picture connected to zenohd with telemetry from every expected drone | FAIL if anything is missing; WARN without the offline map, or a drone not heard recently |
| P6-01, P6-02, P5-01..06, P6-03 | the whole phase 6 run, as in docs/PHASE6.md | as phase 6 |
| P7-02 | every 20 s during the run: the picture's casualties (id, triage, rescue state), tasks and assignments agree with casualty-analyst, mission-planner and rescue-dispatch (a sample that differs is retried 3 times, 1.5 s apart, before it counts) | PASS at 90 % of samples or more, WARN from 75 %, FAIL below |
| P7-03 | a browser's event stream: a full picture, then changes, within seconds | FAIL if no events |
| P7-04 | the crops of up to 10 detections: the colour crops served byte for byte as casualty-analyst stored them, the thermal crops served as PNG images | FAIL on any difference |
| P7-05 | the timeline against the after-action report: every casualty found, every team sent, every rescue finished, the chat | FAIL on anything missing |

---

## Known limitations

- **No login, no encryption** (D10): the operator view is read-only, but anyone on the network can see it.
- **One incident**: the page shows sar-web's configured incident.
- **Crops are what the drones sent**: small, and fewer or none when the link was short of budget (metadata only).
- The picture is rebuilt from the bus and the services' APIs; sar-web keeps no state of its own across restarts (the timeline starts again when sar-web restarts; the after-action report keeps the whole run).
