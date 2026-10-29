# Phase 5 — Rescue routing and the TAK map

**Goal.** The ZBook sends **rescue teams** to the casualties and shows the whole operation on **ATAK and WinTAK**. A new service, **rescue-dispatch**, gives each casualty that needs a team (Red first, then Yellow) to the free team with the quickest route, along real streets downloaded once from OpenStreetMap, around the rubble that blocks them. A TAK server (**taky**, in Docker on the ZBook) carries everything to the devices: casualty markers in their triage colours with the reasons, the drones moving, the investigating drones' lines to their casualties, the teams and their routes, and chat messages such as `TEAM-2 → C-007 RED: 420 m via Kurtuluş Caddesi, about 6 min.` Teams are simulated, or real people with ATAK or WinTAK whose callsign is listed: they get their route and a direct message, and reply `done` when finished.

**Accepted when** `scripts/zbook/90-phase5-test.sh` ends with `RESULT: PASS` (warnings allowed) and you have agreed with the numbers it prints. The plan's target is an **end-to-end application**: drones find and triage the casualties, teams are sent along routes clear of rubble, and the TAK devices show it as it happens.

**Time needed.** ZBook only: about 15 minutes of commands, plus the 30–45 minute acceptance run (or 5–15 minutes with `--quick`). Optional: 10 minutes to connect a phone or a Windows PC.

**Disk.** The taky image is about 200 MB; the street file a few MB.

> **Everything here is simulated.** The incident, the victims and the teams are simulated; the triage rules are illustrative and await medical sign-off (decision D9). The TAK link is plain TCP without encryption (decision D10): use it on a private network only. TLS comes in phase 6.

---

## What this phase adds

| Piece | What it does |
|---|---|
| **rescue-dispatch** (ZBook, new service) | Reads casualty-analyst's casualties, the drones' telemetry and mission-planner's tasks. Every casualty that needs a team gets one, most urgent first: settled Red, then provisional Red, then Yellow (`[dispatch] triage`); Black and Grey never do. Within a priority, the free team with the quickest route goes. A team keeps its casualty until it is done there: no team is taken off a casualty for a more urgent one. It reaches the casualty (within 15 m), frees them from the rubble (15 min to 6 h, from how the building collapsed and how the person is trapped), hands them over (`on_scene`, 10 min), and is then free where it is. An assignment is withdrawn when its casualty stops needing a team (triage changed after a close look, record gone) or its TAK team goes offline; when two records prove to be one person, the assignment carries over. Publishes the teams and assignments on `sar/<incident>/dispatch`. Status: `rescue-dispatch status`. |
| **routes** (in rescue-dispatch) | The street network around the incident (OpenStreetMap, downloaded once by `32-roads.sh`) cut into pieces of at most 10 m. Every piece within 1 m of a rubble spill area (the scenario's collapsed buildings plus 5 m) is closed. A route goes from the team to the nearest open street, along open streets to the point closest to the casualty they reach (the **access point**), then on foot to the casualty: 1.0 m/s on open ground, 0.5 m/s over rubble. On foot 1.4 m/s on any street; vehicles 10–30 km/h by street class (50 on a motorway), keep to one-way streets and stay off footways and steps, and their crews walk the last leg. Without a street file, routes are straight lines (a warning everywhere). |
| **teams** | `source = "sim"`: rescue-dispatch moves the team along its route, `time_scale` (5) times faster than real people so a demo run takes under an hour. `source = "tak"`: a person with ATAK or WinTAK whose callsign is listed; their position comes from their device, and they are offline when it has not reported for 3 minutes. The release lists TEAM-1, TEAM-2 (on foot), TEAM-3 (vehicle) and CHECK-1 (the TAK team the acceptance test plays). |
| **TAK server** (ZBook, Docker) | taky 0.10 in a container (`rubblesar-taky`), plain TCP 8087, as a systemd service; restarted daily (taky's XML parser slowly leaks memory). `34-taky.sh` builds and installs it and checks its routing (`sar-check tak`). |
| **TAK map** | casualty markers (spot markers in the triage colour, label `C-007 RED`, `RED?` while provisional, the reason, the summary, the CEP and the rescue state in the details), the three drones (battery, task), the investigation lines (drone → casualty, orange), the simulated teams, each team's route (in its casualty's triage colour), and SAR-OPS (rescue-dispatch itself, at the command post). Markers are sent on every change and refreshed every 30 s; anything not refreshed disappears after 5 minutes. |
| **chat** | In All Chat Rooms: every dispatch (`TEAM-2 → C-007 RED: 420 m via …, about 6 min.`), arrival, finish, withdrawal, and casualties no street reaches. A TAK team also gets a direct message with the position and what to reply. Teams reply to SAR-OPS (or in All Chat Rooms): `done` (finished here), `unable` (cannot go: the casualty goes to another team), `status` (what am I doing), `ok`/`roger` (noted). |
| `sar-check phase5` | The acceptance run (P5-01..06): the phase 4 run with rescue-dispatch reset at the start; sar-check joins taky as **CHECK-1**, follows the routes it is given and replies `done`, and checks the TAK map, the routes, the dispatch order and the chat. |
| `sar-check tak` | Three test clients through the TAK server: positions, a marker, chat to all and to one callsign, a delete (T-01..05). |
| `rescue-dispatch route` / `roads` | A route between two points on the installed streets, and the street network with the rubble areas, without the service. |
| `sar-monitor` | One more table: the teams and their casualties. |
| `sar-doctor` | DOC-Z9: taky running and answering, the street file, rescue-dispatch running. |

**The Jetson is unchanged.** Keep 0.5.1 running there with the phase 4 config (`edge-eye status` shows inv-1 and inv-2 on standby). Nothing in this release needs Docker on the Jetson.

---

## Before you start

- **Phase 4 accepted**, the scenario installed (`/var/lib/rubblesar/scenarios/antakya-demo`), Docker working on the ZBook (as for training: `docker run --rm hello-world`).
- **Internet on the ZBook** for two downloads: the streets (Overpass API, a few MB) and the taky image (Docker Hub, PyPI). Without it, `32-roads.sh --synthetic` makes a street grid instead (P5-01 then warns), and the taky image must be built on a connected machine (`docker save rubblesar-taky:0.10 | gzip > taky.tgz`, then `docker load < taky.tgz` here).
- **For a phone or a PC (optional)**: ATAK-CIV (Android, Google Play) or WinTAK-CIV (Windows, tak.gov) on the **same network** as the ZBook.

### Your data folder

As before, link the data folder into the new release folder:
```bash
cd rubblesar-0.6.2-phase5
ln -s ../rubblesar-0.3.0-phase2/data data      # or: cp -P ../rubblesar-0.5.1-phase4/data .
```

---

## Upgrading from 0.6.1

0.6.2 changes only the ZBook: teams take a realistic time to free each casualty ([Freeing a casualty](#freeing-a-casualty)), a rescued casualty's marker turns green, a TAK team that does not answer loses its casualty, and your WinTAK (callsign VADER) is listed as a team. With 0.6.1 accepted:
```bash
tar -xzf rubblesar-0.6.2-phase5.tar.gz && cd rubblesar-0.6.2-phase5
ln -s ../rubblesar-0.3.0-phase2/data data
scripts/zbook/20-build.sh
scripts/zbook/30-install.sh --force-config     # the new [dispatch] keys and team VADER (your file kept as sar.toml.bak.<time>)
scripts/zbook/90-phase5-test.sh
```
The street file and taky are unchanged (`32-roads.sh`, `34-taky.sh` not needed). Then set up WinTAK as below ([Being a team yourself](#being-a-team-yourself), [Chat that stays on screen](#chat-that-stays-on-screen)).

## Upgrading from 0.6.0

0.6.1 changes only the ZBook's routes (below, [How a route is made](#how-a-route-is-made)) and moves the command post to the west bank of the Asi. With 0.6.0 accepted:
```bash
tar -xzf rubblesar-0.6.1-phase5.tar.gz && cd rubblesar-0.6.1-phase5
ln -s ../rubblesar-0.3.0-phase2/data data
scripts/zbook/20-build.sh
scripts/zbook/30-install.sh --force-config     # the new command post (your file kept as sar.toml.bak.<time>)
scripts/zbook/32-roads.sh                      # downloads the streets again, now with the rivers
scripts/zbook/90-phase5-test.sh
```
taky is unchanged: `34-taky.sh` is not needed again. If you edited `sar.toml` after 0.6.0, use `30-install.sh` without `--force-config` and set `staging_lat = 36.20160`, `staging_lon = 36.15850` yourself.

## Steps (ZBook)

Unpack the release next to the earlier folders. Your `/etc/rubblesar` config, the installed scenario and the models are kept.
```bash
tar -xzf rubblesar-0.6.2-phase5.tar.gz && cd rubblesar-0.6.2-phase5
```

| Step | Command | What it does | Expect |
|---|---|---|---|
| P5-Z1 | `ln -s ../rubblesar-0.3.0-phase2/data data` | shares your datasets and runs | — |
| P5-Z2 | `scripts/zbook/20-build.sh` | rebuilds and runs all tests (about 15 minutes: the phase 5 test runs a whole rescue in simulation) | `DONE` |
| P5-Z3 | `scripts/zbook/30-install.sh --force-config` | installs the programs, **replaces** `/etc/rubblesar/sar.toml` with the phase 5 one (new section `[dispatch]` and the teams; your old file is kept as `sar.toml.bak.<time>`), installs the **rescue-dispatch** service (account `rubblesar`, state in `/var/lib/rubblesar/dispatch`) | `ok: rescue-dispatch is running`, warnings that the street file and the TAK server are not there yet, `DONE` |
| P5-Z4 | `scripts/zbook/32-roads.sh` | downloads the streets and rivers within 1.1 km of the incident centre into `/var/lib/rubblesar/roads/antakya-demo.json`, shows how many street pieces the rubble closes and a route from the command post to each collapse, restarts rescue-dispatch | `rubble areas: 5; street pieces closed: …`, `rivers: …`, `DONE` |
| P5-Z5 | `scripts/zbook/34-taky.sh` | builds the taky image, installs and starts `rubblesar-taky`, opens 8087/tcp in ufw, runs `sar-check tak`, prints the address for the devices | `RESULT: PASS`, `DONE` |
| P5-Z6 | (optional) connect ATAK or WinTAK, below | see the picture live | the markers, drones and teams appear |
| P5-Z7 | `scripts/zbook/90-phase5-test.sh` | **the acceptance run, 30–45 minutes**. Keep both machines awake. The Jetson's edge-eye must be running | `RESULT: PASS`, `PHASE 5 checks finished` |

Without `--force-config`, your phase 4 `sar.toml` stays and the `[dispatch]` defaults apply: the same values, except that the teams start at the incident centre instead of the command post by the drones (set `staging_lat`, `staging_lon` as in the release file).

`--quick` on P5-Z7 skips the survey: the casualties of the last run (still in casualty-analyst) get teams again, in 5–15 minutes. Use it to try settings; the acceptance is the full run.

### What to send back

`90-phase5-test.sh` prints a `COPY FROM HERE … COPY TO HERE` block **even when everything passes**. Send it: the numbers (markers, routes, dispatch order, times, the CHECK-1 loop, chat) are what we agree on. If any other step fails, send its block.

---

## Connecting ATAK and WinTAK

`34-taky.sh` prints the ZBook's addresses at the end (`hostname -I` shows them too). The device must be on the same network and reach TCP 8087.

**ATAK-CIV (Android)**
1. ☰ menu → **Settings** → **Network Preferences** → **TAK Servers** (in some versions *Manage Server Connections*) → **+ / Add**.
2. Description `RubbleSAR`, Address: the ZBook's IP. Tick **Advanced Options**: **Streaming protocol TCP** (not SSL), **port 8087**. Leave authentication and certificate enrollment off.
3. OK. The server's icon turns green within a few seconds; the casualty markers, drones and teams appear around Antakya (☰ → the map, zoom to 36.2025 N, 36.1606 E).
4. Your callsign: ☰ → **Settings** → **My Preferences** (or *Callsign Preferences*) → **My Callsign**.

**WinTAK-CIV (Windows)**
1. **Settings** (gear) → **Network Preferences** → **Manage Server Connections** → **Add**.
2. Description `RubbleSAR`, Address: the ZBook's IP, **Port 8087**, **Protocol TCP** (no SSL), OK. The connection shows as connected.
3. Your callsign: **Settings** → **My Preferences** → **Device Preferences** → **Callsign** (the team and role are there too).

Menu names vary a little between versions; the settings are always: address, port 8087, TCP, no SSL.

**A street map underneath.** Out of the box WinTAK and ATAK have only a coarse world picture, blank at street level. `configs/tak/openstreetmap.xml` adds OpenStreetMap's street map (the same streets the routes follow), streamed over the device's internet connection while you look:
- **WinTAK:** quit WinTAK, copy the file into `C:\ProgramData\WinTAK\Imagery` (a hidden folder: type the path into Explorer's address bar), start WinTAK; or drag the file onto the WinTAK window (File → Import Manager also works). Then **Maps & Favorites** → **MOBILE** tab → set the source type to **Online** → **OpenStreetMap**.
- **ATAK:** copy the file to the phone, ☰ → **Import** → **Local SD**, pick it; then ☰ → **Maps** (*Map Manager*) → **Mobile** → **OpenStreetMap**.

It zooms to street level (zoom 19, about 0.25 m per pixel at Antakya; closer in, WinTAK enlarges the same tiles). Do not use the devices' "download map area" with it: OpenStreetMap's tile servers allow viewing, not offline copies. **It may stop short of street level:** since early 2026 OpenStreetMap's tile servers refuse apps that do not send the identification they now require (a proper User-Agent and Referer), and WinTAK showed only the coarser levels in our test. Phase 6 has the ZBook draw and serve its own map of the incident area (streets, river, the collapsed buildings and the streets they close), which works without internet: [PHASE6.md](PHASE6.md#the-offline-map-in-wintak-and-atak).

**What you see.** Tap a casualty marker for its details: triage and reason, the summary, how sure the position is, which drones saw it, and the rescue state (`waiting for a free team`, `TEAM-2 en route, about 4 min`, `TEAM-2 on scene, heavy rescue, about 1 h 40 min left (pancake collapse, ...)`, `freed and handed over by TEAM-2`, or `NO STREET ROUTE`). Once a team has freed a Red or Yellow casualty its marker turns **green** and reads `C-007 RESCUED`. Routes are lines in the casualty's colour from the team; the orange lines are drones on their way to a close look.

### Chat that stays on screen

Chat messages arrive in WinTAK as notifications (the flag at the top of the screen, and a count on the Chat icon) that close on their own. To keep the conversation in view:
1. Open **Contacts** (the people icon) and select **All Chat Rooms**: its chat window opens with every dispatch, arrival and rescue message, newest at the bottom. SAR-OPS's direct messages to you are under **SAR-OPS** in the same list.
2. **Undock** the chat window (the down arrow in its title bar, or drag the title bar off the dock) and put it beside the map, or on a second screen. Do not use its **Pin** icon: that hides the window at the edge of the screen until you hover over it.
3. To see the list and the messages side by side, drag the left edge of the Contacts window wider.

WinTAK shows only messages received while it was connected; `rescue-dispatch status` on the ZBook lists the latest ten whatever happened.

### Being a team yourself

The release config lists your WinTAK, which joined taky as **VADER**, as a rescue team on foot:
```toml
[[dispatch.team]]
id = "vader"
callsign = "VADER"      # exactly the device's callsign (case does not matter)
mode = "foot"
source = "tak"
```
To use another callsign, change it in both places: WinTAK **Settings** → **My Preferences** → **Device Preferences** → **Callsign**, and `callsign` here (`sudo nano /etc/rubblesar/sar.toml`, then `sudo systemctl restart rescue-dispatch sar-web`). In WinTAK:
1. **Give the laptop a position at the incident.** It has no GPS, so it reports none, or one at your desk, too far from any street (E-RTE-002). **Settings** → **My Preferences** → **GPS Preferences** → **GPS Type**: **Stationary GPS**, and set its **Stationary Location** to the command post, **36.20160 N, 36.15850 E** (or select your self-marker and use **Fine Adjust** to move it there). WinTAK keeps it after a restart.
   Or leave WinTAK's position alone and add `at_command_post = true` to the team in `sar.toml` (then `sudo systemctl restart rescue-dispatch sar-web`): the team then stands at the command post whatever its device reports. The same works for a phone running ATAK on its own GPS, away from the incident.
2. Check you are a free team: `rescue-dispatch status` on the ZBook shows `vader VADER foot tak idle` once WinTAK reports the position (offline before that, and 3 minutes after WinTAK stops reporting).
3. **When SAR-OPS sends you a casualty** you get a direct message (Contacts → SAR-OPS) and your route on the map. Reply to SAR-OPS within 3 minutes (`tak_ack_timeout`), or the casualty goes to another team:
   - `ok` (or `roger`): you are going;
   - `status`: where you are with it;
   - `unable`: you cannot go; it goes to another team;
   - `done`: you have finished (the casualty is freed and handed over); its marker turns green.

   At a desk you can reply `ok`, then `done` whenever you like. To arrive properly, move your self-marker (Fine Adjust) to within 15 m of the casualty: SAR-OPS then tells you what freeing them will take, and you say `done` when you decide they are out.

During the acceptance run you take casualties like any team: reply to them, or close WinTAK and let the simulated teams do the work.

---

## What the tests prove

### `sar-check tak` (in 34-taky.sh and 90-phase5-test.sh)

| Check | Proves | Pass |
|---|---|---|
| T-01 | three clients connect to the TAK server (plain TCP) | FAIL if not |
| T-02 | each client's position reports reach the other two | FAIL if not within 10 s |
| T-03 | a red spot marker reaches the others with its colour | FAIL if not |
| T-04 | an All Chat Rooms message reaches everyone; a message addressed to one callsign reaches only it | FAIL if All Chat fails; WARN if addressed messages are not routed (teams still see All Chat Rooms) |
| T-05 | a delete reaches the others | FAIL if not |

### `sar-check phase5` (the acceptance run)

The run resets casualty-analyst (archiving its previous run), mission-planner and rescue-dispatch (teams back at the command post, the map cleared), then restarts the drones as in phase 4. sar-check is connected to taky as **CHECK-1** throughout: when it is given a casualty it walks its route (as fast as the simulated teams), and once on scene replies `done`. After the drones are done it waits until every Red and Yellow casualty has been reached and no team is busy (at most 25 minutes; once all are reached, at most 8 more minutes for teams still freeing a casualty), then for one more marker refresh, and checks:

| Check | Proves | Pass / warn |
|---|---|---|
| P5-01 | the drones and services as in P4-01; taky answers; rescue-dispatch runs with the streets, the rubble areas and a TAK connection; and sees CHECK-1's position reports through taky | FAIL if a piece is missing; WARN with a synthetic street grid |
| P5-02 | the TAK map as a device sees it: a marker for every casualty, in its triage colour (green and `RESCUED` once a team has freed it), within 2 m of the record, not stale; the drones (when they report) and the simulated teams are there | FAIL on any missing or wrong marker, drone or team; WARN on leftover markers of records gone |
| P5-03 | every route: from where the team joins the streets to where it leaves them, nowhere inside a rubble area (0.5 m tolerance); the route ends at its casualty; no walk off the streets across a river; its line reached the TAK map; how much longer than the straight line, detours, final walks over rubble | FAIL on a route through rubble or across a river, or not shown (under 90 %) |
| P5-04 | the dispatch order: no team for Black or Grey; no Yellow given a team while a Red was already waiting; every Red and Yellow reached; how long casualties waited for a free team and how long teams took | FAIL on an order problem or a casualty not reached; WARN at the time limit with ≥ 80 % reached |
| P5-05 | the TAK team loop: CHECK-1 seen online, given a casualty through TAK (route line and a message naming it), walked it, seen on scene, released after its `done` | FAIL if no loop completed; WARN if addressed messages did not arrive (only All Chat Rooms) |
| P5-06 | a chat message in All Chat Rooms for every dispatch and every arrival, as received by CHECK-1 | PASS ≥ 95 %, WARN ≥ 80 % |

---

## How dispatch decides

Each second rescue-dispatch:
1. moves the simulated teams along their routes, notices TAK teams arriving (within `arrive_m`), frees teams whose time on scene is over, and takes a casualty back from a TAK team that has neither answered nor moved towards it within `tak_ack_timeout` (3 min);
2. withdraws assignments whose casualty no longer needs a team (now Black or Grey after its close look, or gone) and re-routes teams whose casualty's position moved by more than 5 m;
3. lists the casualties that need a team and have none (listed for at least `min_age`, not yet reached), most urgent first: priority 1 settled Red, 2 provisional Red, 3 settled Yellow, 4 provisional Yellow;
4. while free teams remain: takes the most urgent priority, works out every free team's route to every casualty of that priority (one street search per team), and gives the pair with the shortest travel time; when no free team can reach any casualty of a priority, says so once in chat and moves on to the next priority.

A casualty is **provisionally Red** while warm and not yet watched close up: it gets a team at once (it may be alive) and also an investigation. The drone is usually there first; if the close look settles it Black, the team is called off. Once a team has finished at a casualty it is never sent there again.

### Freeing a casualty

A team that reaches its casualty first has to dig them out. The simulation knows how each victim is trapped (the scenario), and rescue-dispatch estimates the time as a USAR team would on arrival:

| Factor | Effect |
|---|---|
| the building | pancake collapse (floors stacked, a void to breach) 90 min; rubble mound 50 min; no collapse known there 20 min |
| depth of the rubble | × (0.6 + 0.15 per metre of pile), 0.7 to 1.5 |
| what shows of the person | head or back uncovered × 0.6; a whole arm free × 0.8; only a hand, foot or lower leg × 1.3 |
| how much of that is covered | × (1 + the share under debris) |
| the team | a vehicle team's heavier tools × 0.8 |
| the rest | × 0.75 to 1.35, different for each casualty and run |

The result, 15 minutes to 6 hours in 5-minute steps, is a **light** (under 45 min), **medium** (under 2 h) or **heavy** rescue; then `on_scene` (10 min) to treat and hand over. A casualty found Black on arrival takes 10 minutes to confirm and mark for recovery. For the installed scenario's Red and Yellow victims this gives 40 minutes to 3 h 25, 1 h 50 on average. Chat says it when the team arrives (`TEAM-2 reached C-007 RED (6 min after dispatch). Heavy rescue, about 2 h 20 min (pancake collapse, 2.6 m of rubble; only a forearm and hand showing, 7% covered).`) and when it is done (`TEAM-2 finished at C-007: freed after 2 h 20 min and handed over; free for the next casualty.`); the marker counts down the time left. Simulated teams do it `rescue_time_scale` (30) times faster, so a 2-hour rescue takes 4 minutes of the run; the times shown are real ones. A TAK team decides itself: it is told the estimate and replies `done`. The figures are illustrative, like the triage rules, until reviewed by USAR practitioners.

## How a route is made

`32-roads.sh` downloads every way tagged `highway` (motorways to footways and steps) within the incident radius plus 300 m, with their node ids (so crossings connect), one-way and access tags, and the centre lines of rivers and canals. rescue-dispatch loads it, cuts it into pieces of at most 10 m and closes every piece passing within `block_margin_m` (1 m) of a rubble area.

- **Access tags.** A way marked `access=no` is closed unless `foot=yes` (or `vehicle`, `motor_vehicle`) opens it again; `foot=no` closes it on foot, `motor_vehicle=no` by vehicle. In Antakya, OpenStreetMap marks the central bridge (Ata Köprüsü) and the footbridges by the north-west collapse closed since the earthquake, so teams cross the Asi by the bridges still open.
- **Getting on and off the streets.** A route starts on any open street within 60 m of the closest one to the team, and leaves the streets at any point within 60 m of the closest one to the casualty. Each metre walked beyond the closest point costs 10 s more, so the route takes the closest point unless a street a little further away saves a long way round (a dead end, a street reached only from the far side of a block). Cutting across blocks of damaged buildings is no shortcut.
- **Rivers.** A walk off the streets never crosses a river or canal: the team uses a bridge.
- The quickest path (Dijkstra) is then taken from the team over the open streets, and the rest is walked, slowly over rubble. A casualty within 50 m of the team (the next one in the same collapse) is also tried straight across, and the quicker way wins. A casualty more than 200 m from any open street, or with every way in closed, has no route: its marker and the chat say so.

The command post (`staging_lat`, `staging_lon`) is on the west bank, near four of the five collapses; with the central bridge closed, teams starting on the east bank went a long way round to the north-west collapse (the sum of the routes on foot from the command post to the five collapses: 6.2 km before, 2.0 km now).

`rescue-dispatch roads` shows the network and the closed streets; `rescue-dispatch route --from LAT,LON --to LAT,LON [--mode vehicle]` any route, point by point.

## What is on the TAK map

| Object | CoT type | uid | Refreshed |
|---|---|---|---|
| casualty | `b-m-p-s-m` spot marker, colour Red / Yellow / Black / Grey; green, label `C-007 RESCUED`, once a team has freed it | `rubblesar.<incident>.cas.C-007` | on change, every 30 s; deleted when merged or gone |
| drone | `a-f-A-M-H-Q` | `rubblesar.<incident>.uav.scout-1` | every 2 s while it moves, every 10 s otherwise |
| investigation | `u-d-f` line, orange | `rubblesar.<incident>.inv.<task>` | with its drone; deleted when the task ends |
| simulated team | `a-f-G-U-C`, its own TAK connection | `rubblesar.<incident>.team.team-1` | every 2 s en route, every 30 s otherwise |
| route | `u-d-f` line in the casualty's colour | `rubblesar.<incident>.route.team-1` | on dispatch and re-route; deleted on arrival or withdrawal |
| SAR-OPS | `a-f-G-U-C` at the command post | `rubblesar.<incident>.ops` | every 30 s |
| chat | `b-t-f` GeoChat from SAR-OPS | `GeoChat.rubblesar.<incident>.ops.…` | — |

The dispatch state itself is `sar.v1.DispatchState` (`proto/sar/v1/dispatch.proto`) on `sar/<incident>/dispatch`: every team (mode, source, state, position, casualty, done), every assignment (casualty, team, triage then and now, priority, route with every point, lengths on streets, off them and over rubble, travel time, created / on scene / done times, note), the casualties waiting, the streets and the TAK link.

```bash
rescue-dispatch status                                   # streets, TAK link, teams, waiting, assignments, chat
curl -s 127.0.0.1:9104/v1/state | python3 -m json.tool | less
journalctl -u rescue-dispatch -f                         # "team dispatched", "team on scene", "team done"
docker logs -f rubblesar-taky                            # taky: clients connecting
```

---

## New configuration

All keys, with defaults, are in [ENVIRONMENT.md](ENVIRONMENT.md#configuration-reference). ZBook `[dispatch]`: the street file and how close rubble closes a street, the TAK server and rescue-dispatch's callsign, marker refresh and expiry, which triage colours get a team, when a team has arrived, freeing the casualty (`extrication`) and handing over (`on_scene`), the simulated teams' time scales (`time_scale`, `rescue_time_scale`), when a TAK team is offline or has not answered (`tak_ack_timeout`), the command post, and `[[dispatch.team]]` (id, callsign, foot or vehicle, sim or tak).

Ports: **8087/tcp** taky (open to the network for the devices; `34-taky.sh` adds the ufw rule), **9104/tcp** rescue-dispatch's status API (127.0.0.1 only).

---

## Known limitations

- **No encryption.** Plain TCP to taky (decision D10): anyone on the network can connect and see the map. Phase 6 adds TLS (taky's SSL port with client certificates).
- **No re-planning for urgency.** A team on its way to a Yellow is not turned round for a new Red; the Red waits for the next free team (P5-04 reports the waits). Simple and predictable; preemption can come later if wanted.
- **Streets from OpenStreetMap, rubble from the simulation.** The collapsed buildings are procedural and placed without regard to the real streets, so some real streets are blocked by a simulated collapse. Debris in the streets beyond the spill areas and traffic are not known; closures come from OpenStreetMap's access tags, as mapped (some may be out of date, and some paths are not joined to the streets around them, which makes a route go round).
- **On-foot legs are straight lines.** The walk from the street to the casualty, and from a casualty back to the street, does not know about walls (rivers it does know); it is short by construction (at most 60 m beyond the closest point the streets reach).
- **Simulated teams are 5× real time** (`time_scale`) on the move and 30× (`rescue_time_scale`) freeing casualties, so a run takes under an hour; times in messages and markers are real-time estimates.
- **Extrication times are a model**, from the simulated collapse and what shows of each victim, not from experience on site; they await review by USAR practitioners.
- **taky is a small server.** It routes messages and positions; it has no user accounts here (plain TCP) and keeps no history: a device connecting late sees each marker within 30 s (the next refresh).
- **Triage is illustrative** (D9), and casualties found late in the run are reached late: P5-04 shows how long each waited.

---

## Command reference

```
rescue-dispatch run|status [--json]|reset --yes|check-config|version
rescue-dispatch route --from LAT,LON --to LAT,LON [--mode foot|vehicle]
rescue-dispatch roads
scenario-tool fetch-roads --config FILE --out FILE [--synthetic] [--extra-m 300] [--endpoint URL]
sar-check tak [--server HOST:PORT]                                   T-01..05
sar-check phase5 [--quick] [--max-minutes 45] [--allow-oracle] [--report-dir DIR]
scripts/zbook/32-roads.sh [--force] [--synthetic] [--from FILE]
scripts/zbook/34-taky.sh [--rebuild]
scripts/zbook/90-phase5-test.sh [--quick] [--max-minutes N]
```
API (127.0.0.1 only): rescue-dispatch `GET /v1/status`, `GET /v1/state`, `POST /v1/reset`.
