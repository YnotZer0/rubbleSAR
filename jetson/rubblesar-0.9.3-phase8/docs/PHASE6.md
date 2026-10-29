# Phase 6 — One application, end to end

**Goal.** Everything built in phases 0 to 5 works as one application. One command on the ZBook brings both machines up, shows their state on one screen, runs a mission and takes them down again. WinTAK and ATAK get a street-level map of the incident area, drawn on the ZBook and served from it, that works without internet. Every run ends with an **after-action report**: what happened when, on a map and a timeline, and how it compares with the scenario.

**Accepted when** `scripts/zbook/90-phase6-test.sh` ends with `RESULT: PASS` (warnings allowed) and you have agreed with the numbers it prints and the report it writes.

**Time needed.** ZBook: about 20 minutes of commands, plus the 30–50 minute acceptance run (5–15 with `--quick`). Jetson: about 10 minutes (rebuild and reinstall). Optional: 5 minutes to load the map into WinTAK.

**Disk.** The map: about 20–60 MB (tiles plus the same tiles as one MBTiles file). Each report: 1–3 MB.

> **Everything here is simulated.** The incident, the victims and the teams are simulated. The triage rules and the extrication times are illustrative and await review (decision D9). The TAK link and sar-web are plain TCP/HTTP without encryption: use them on a private network only.

---

## What this phase adds

| Piece | What it does |
|---|---|
| **Offline incident map** (`scripts/zbook/36-map.sh`) | Downloads the buildings, water and green areas around the incident once (OpenStreetMap, Overpass API), takes the streets as rescue-dispatch routes over them, and draws map tiles from zoom 13 (the whole area) to 19 (street level, 0.25 m per pixel): street names, the river, buildings, hospitals, **streets closed by rubble in red**, streets closed in OpenStreetMap in red dashes (the central bridge), the **collapsed buildings and their rubble**, the command post. Writes the tiles, the same tiles as one **MBTiles** file, a legend and an overview. Drawn by `tools/map/render_map.py` (Python and Pillow, offline tooling). |
| **sar-web** (ZBook, new service, port 8090) | Serves the map tiles to WinTAK and ATAK (a map source that streams from the ZBook), the MBTiles file to download, and the after-action reports, with an index page: open `http://<ZBook>:8090/` in any browser on the network. Read-only. |
| **Rubble on the live TAK map** | rescue-dispatch draws each collapse's rubble as a brown shape on the TAK map (`[dispatch] show_collapses`), with or without the offline map underneath. |
| **`rubblesar`** (ZBook, new command) | `rubblesar status`: every ZBook service, the Jetson's edge-eye and each drone (with the software it runs), on one screen, with the fix for anything down. `rubblesar up` / `down`: start or stop everything in order on both machines. `rubblesar mission start`: a run (new survey, close looks, rescue teams), recorded, ending with its after-action report. `rubblesar report`: write a recorded run's report again. |
| **After-action report** | One self-contained web page per run: headline numbers, a map (the offline map with the rubble, the drones' flight paths, the teams' routes, every casualty, green once rescued), a timeline (each casualty from first sighting to freed; each team's work), the casualties, the scenario comparison (each victim found or not, triage given and expected, position error), teams, drones, assignments and the whole chat. Kept with the run's recording in `/var/lib/rubblesar/www/reports/<incident>/<run>/`. |
| **The Jetson over SSH** (`scripts/zbook/38-jetson-link.sh`) | Once: an SSH key from the ZBook to the Jetson and a sudo rule there allowing exactly `systemctl start|stop|restart edge-eye`, so `rubblesar up/down` handle both machines. |
| **edge-eye 0.7.x** (Jetson) | Reports its software version in the telemetry, so `rubblesar status` and the report show what each drone runs. Nothing else changes on the Jetson. |
| `sar-check phase6` | The acceptance run: P6-01 both machines up, P6-02 the offline map drawn and served (and showing the rubble and the closed streets where they are), the whole phase 5 run (P5-01..06) recorded, P6-03 its after-action report written, served and consistent with rescue-dispatch and casualty-analyst. |
| `sar-doctor` | DOC-Z10: sar-web running, the offline map installed. |

---

## Before you start

- **0.6.2 accepted** on the ZBook (or 0.6.1: this release includes 0.6.2's changes).
- **Coming from 0.7.0?** Its `20-build.sh` stopped in the unit tests on both machines (see the CHANGELOG for 0.7.1). Start these steps again with 0.7.1 in a new folder; nothing from 0.7.0 needs undoing.
- **0.7.1 accepted?** (20 Oct 2026, every check PASS.) 0.7.2 changes only the ZBook's sar-web (the WinTAK map file) and `20-build.sh`; nothing on the Jetson. No need to install it on its own: it comes with the next phase.
- **Internet on the ZBook once** for the map's buildings (Overpass API, a few MB) and `python3-pil` (apt). Without it, `36-map.sh --no-download` draws the map from the street file alone.
- **The Jetson's address and login** (as for `65-push-scenario.sh`), for `38-jetson-link.sh`.
- The WinTAK laptop on the same network as the ZBook.

### Your data folder

As before, link it into the new release folder (both machines):
```bash
cd rubblesar-0.7.2-phase6
ln -s ../rubblesar-0.3.0-phase2/data data
```

---

## Steps (ZBook)

```bash
tar -xzf rubblesar-0.7.2-phase6.tar.gz && cd rubblesar-0.7.2-phase6
```

| Step | Command | What it does | Expect |
|---|---|---|---|
| P6-Z1 | `ln -s ../rubblesar-0.3.0-phase2/data data` | shares your datasets | — |
| P6-Z2 | `scripts/zbook/20-build.sh` | rebuilds and runs all tests (about 15 minutes) | `DONE` |
| P6-Z3 | `scripts/zbook/30-install.sh --force-config` | installs the programs (now with `sar-web` and `rubblesar`), **replaces** `/etc/rubblesar/sar.toml` with the phase 6 one (new sections `[web]`, `[ops]`; your old file is kept as `sar.toml.bak.<time>`), installs and starts the **sar-web** service, opens 8090/tcp in ufw | `ok: sar-web is running`, a warning that there is no offline map yet, `DONE` |
| P6-Z4 | `scripts/zbook/36-map.sh` | installs Pillow and the DejaVu fonts if needed, downloads the buildings, draws the map (about 2 minutes), installs it for sar-web, prints the address for WinTAK | `wrote 1877 tiles (... MB)`, `DONE` |
| P6-Z5 | `scripts/zbook/38-jetson-link.sh --jetson USER@JETSON-IP` | SSH key, the sudo rule on the Jetson, `[ops] jetson` in the config (asks the Jetson's password and your sudo password there once) | `the key works`, `installed /etc/sudoers.d/rubblesar-ops`, `DONE` |
| — | **the Jetson steps below** | | |
| P6-Z6 | `rubblesar up` then `rubblesar status` | everything started, then on one screen | `RESULT: everything up` |
| P6-Z7 | `scripts/zbook/90-phase6-test.sh` | **the acceptance run, 30–50 minutes**. Keep both machines awake | `RESULT: PASS`, `PHASE 6 checks finished` |

Run `38-jetson-link.sh` again after any later `30-install.sh --force-config`: replacing the config clears `[ops] jetson`.

### Steps (Jetson)

```bash
tar -xzf rubblesar-0.7.2-phase6.tar.gz && cd rubblesar-0.7.2-phase6
ln -s ../rubblesar-0.3.0-phase2/data data
scripts/jetson/20-build.sh
scripts/jetson/30-install.sh --zbook-ip <ZBOOK-IP>     # keeps your edge-eye.toml, restarts edge-eye
```
The model engine and the scenario are unchanged. `rubblesar status` on the ZBook then shows `edge-eye 0.7.2-phase6` for each drone.

### What to send back

`90-phase6-test.sh` prints a `COPY FROM HERE … COPY TO HERE` block **even when everything passes**: send it. Open the report it names (`http://<ZBook>:8090/` lists it) and tell me what reads well and what does not.

---

## The offline map in WinTAK and ATAK

On the laptop, open **`http://<ZBook-IP>:8090/`** in a browser (`36-map.sh` prints the address). Two ways to use the map:

**On the laptop itself (works away from the ZBook too; tested with WinTAK):**
1. Download **antakya-demo.mbtiles** from that page.
2. Quit WinTAK, copy it into `C:\ProgramData\WinTAK\Imagery`, start WinTAK (the WinTAK Quick Start Guide lists MBTiles in that folder under MOBILE → Local).
3. **Maps & Favorites** → **MOBILE** → source type **Local** → **RubbleSAR antakya-demo** (with the map on Antakya; **Show All** lists tilesets elsewhere).

**Streaming from the ZBook** (the laptop on the ZBook's network; tested with WinTAK):
1. On the laptop, download **rubblesar-map.xml** from that page. It points at the address you opened the page with, so open the page on the laptop by the ZBook's network address, not on the ZBook as `localhost`. Check that the page's **test tile** link opens on the laptop: if it does not, WinTAK cannot reach the ZBook either (see "WinTAK does not list the map" below).
2. Quit WinTAK, copy the file into `C:\ProgramData\WinTAK\Imagery` (a hidden folder: type the path into Explorer's address bar), start WinTAK. Dragging the XML onto WinTAK gives "import failed" on some versions; copying it into the folder works.
3. **Maps & Favorites** → **MOBILE** → source type **Online** → **RubbleSAR antakya-demo**, then zoom in on Antakya: the map is drawn from zoom 13 (about the whole town on screen); further out, and around the incident area, you see a plain beige background.

ATAK: copy either file to the phone, ☰ → **Import** → **Local SD**, then ☰ → **Maps** → **Mobile**.

The map covers the incident radius plus 300 m (the street download); outside it the map fades. It shows zoom 13 to 19; closer in, WinTAK enlarges zoom 19. **Red** streets are closed by rubble (the teams route around them), **red dashes** closed in OpenStreetMap (the central bridge, Ata Köprüsü), **brown hatching** the rubble around each collapsed building (dark brown). The legend is on the sar-web page.

The map is drawn once from the street file and the scenario. Run `36-map.sh` again after `32-roads.sh --force` or a new scenario (`--force` downloads the buildings again too), then copy the new `.mbtiles` onto the laptop again (WinTAK keeps the copy it was given).

### WinTAK does not list the map

WinTAK leaves a map source file out of MOBILE → Online when it cannot read it. The cause found on the acceptance laptop was the file's layout (step 4); work through these on the laptop:

1. **The test tile** `http://<ZBook-IP>:8090/map/antakya-demo/0/0/0.png` opens in the laptop's browser as a plain beige square. If it does not load, the laptop cannot reach sar-web: on the ZBook, `sudo ufw status` must list `8090/tcp ALLOW` (`30-install.sh` adds it) and `systemctl status sar-web` must be active; the laptop must be on the ZBook's network (it reaches the TAK server on the same address).
2. **The address in the file.** Open `rubblesar-map.xml` in Notepad: the `<url>` must start with the ZBook address the laptop uses (`http://192.168.x.x:8090/...`), not `localhost` or `127.0.0.1`. Download it again on the laptop if not.
3. **WinTAK restarted** after copying the file (quit it from the menu, not just the window).
4. **The file itself.** WinTAK ignored the map source files of 0.7.1 and earlier (and the phase 5 `openstreetmap.xml`), which had a comment before `<customMapSource>` and the URL in a CDATA section; rewritten without both, like the map sources WinTAK users load (github.com/joshuafuller/ATAK-Maps), the same map was listed and drawn. Open it in Notepad: the second line must be `<customMapSource>`.
5. Still missing: copy a known-good source next to it, e.g. `ESRI/esri_world_topo.xml` from ATAK-Maps, and restart. If "Esri - World Topo" is listed and RubbleSAR is not, send both files. If neither is, WinTAK is not reading that folder: check Settings → My Preferences → Directory Preferences.

From the next release sar-web also answers tiles it has not drawn (zooms below 13, the area around) with a plain tile instead of "not found", and the map source declares zoom 0 up, as most map sources do.

---

## Running the system

```bash
rubblesar up                 # zenohd, casualty-analyst, mission-planner, taky, rescue-dispatch, sar-web; then edge-eye on the Jetson
rubblesar status             # one screen; exit code 1 if something is down, with the fix
rubblesar mission start      # a run: new survey, close looks, teams; ends with the report
rubblesar down               # edge-eye first, then the ZBook's services (--keep-tak leaves taky up)
```

`rubblesar up` asks for your sudo password (it runs `systemctl start`). The services also start at boot; `up` is for after a `down`, or to check that everything answers.

**A mission** (`rubblesar mission start`) does what the acceptance run does without the checks: archives the last run's casualties, clears the tasks and the teams, restarts the drones' missions, and records everything until the survey is done, every close look is flown and every rescue is finished (or `[ops] max_mission`, 90 min, or Ctrl-C). It prints progress every 30 seconds and, at the end, where the report is:
```
After-action report: 21 of 23 victims found (28 casualty records); 20 of 20 Red/Yellow rescued, median 14 min from first sighting to freed; run 34 min
  /var/lib/rubblesar/www/reports/antakya-demo/20261019T091502Z/index.html
  open http://192.168.1.20:8090/reports/antakya-demo/20261019T091502Z/ in a browser (any machine on the network)
```
`--continue` keeps the casualties already found (no reset, no new survey). Watch it on WinTAK as it happens.

## The after-action report

| Section | Shows |
|---|---|
| Headline | victims found of the scenario's, triage as the scenario expects, Red/Yellow rescued, median first sighting to freed, median wait for a team, median freeing time (real-world estimate) |
| Map | the offline map at the zoom that fits the run, with the rubble, each drone's flight path, the teams' routes, the command post and every casualty (green once rescued); hover for details |
| Timeline | each Red/Yellow casualty: seen and waiting (grey), team on the way (blue), team freeing and handing over (orange), triage settled (◆), close look done (○); and each team's work |
| Casualties | each record: triage, the scenario victim it is and its expected triage (≠ when they differ) and position error, first seen, triage settled, close look, team, sent, reached, freed, route length, freeing time (real) |
| Against the scenario | each victim: found or **not found**, expected and given triage, position error, rescued |
| Teams, drones, assignments | per team: assignments, distance, time on the way and on scene; per drone: time airborne, distance, battery, reports, close looks, software; every assignment with its note |
| Chat | every message between SAR-OPS and the teams |

Times are run time from the start (`+mm:ss`). Simulated teams move 5× and free casualties 30× faster than real ones (as set in `[dispatch]`), so their parts of the timeline are compressed; the real-world estimates are in the tables and the tooltips.

Each run's folder keeps the recording (`events.jsonl`, `meta.json`): `rubblesar report --run <folder>` writes its report again (with the current map), `rubblesar report --list` lists the runs.

---

## What the tests prove

### `sar-check phase6` (the acceptance run)

| Check | Proves | Pass / warn |
|---|---|---|
| P6-01 | `rubblesar status`: every ZBook service running and answering, edge-eye on the Jetson (over SSH), each drone reporting, with its software | FAIL if anything is down; WARN if the Jetson is not linked (`38-jetson-link.sh`) or a drone runs other software than this release |
| P6-02 | the offline map: every tile of zoom 13 to 18 (and 19) drawn for the area, the MBTiles file, the legend and overview; served by sar-web with its map source XML; at the centre of each rubble area the tile shows rubble, and streets closed by rubble are red where they are | FAIL on anything missing or wrong; WARN without buildings (`--no-download`) |
| P5-01..06 | the whole phase 5 run, as in docs/PHASE5.md (the TAK map now also shows the rubble shapes) | as phase 5 |
| P6-03 | the run's after-action report: written with its map, timeline and scenario comparison; the same casualties as casualty-analyst and the same assignments, in the same states, as rescue-dispatch; the chat recorded; served by sar-web | FAIL on any difference |

---

## New configuration

All keys, with defaults, are in [ENVIRONMENT.md](ENVIRONMENT.md#configuration-reference). ZBook:
- `[web] listen` (`0.0.0.0:8090`), `root` (`/var/lib/rubblesar/www`): sar-web.
- `[ops] jetson` (`""`; `38-jetson-link.sh` sets `user@address`), `jetson_service` (`edge-eye`), `max_mission` (`90m`): the `rubblesar` command.
- `[dispatch] show_collapses` (`true`): the rubble shapes on the TAK map.

Ports: **8090/tcp** sar-web (open to the network; `30-install.sh` adds the ufw rule), as well as 8087/tcp taky and 7447/tcp zenohd.

---

## Known limitations

- **The map is drawn once**, from OpenStreetMap as mapped and the simulated collapses; it does not change as the drones look. After the 2023 earthquake much of central Antakya was demolished and may be mapped as it was. Draw it again after a new street file or scenario.
- **No encryption.** sar-web (HTTP), taky (TCP 8087) and Zenoh are plain on the network, as agreed for now (decision D10).
- **`rubblesar up` and `down` need the Jetson linked** (`38-jetson-link.sh`); without it they handle the ZBook and say what to start on the Jetson. The sudo rule on the Jetson allows only starting, stopping and restarting edge-eye.
- **The report's times are run times**, with team travel and rescue compressed (`time_scale`, `rescue_time_scale`); real-world estimates are given beside them.
- **A record is matched to a victim within 6 m**; two victims close together can swap records in the comparison.

---

## Command reference

```bash
rubblesar status | up [--no-jetson] | down [--no-jetson] [--keep-tak]
rubblesar mission start [--max-minutes N] [--continue]
rubblesar report [--run STAMP] [--list]
sar-web status                                   # what it serves, the addresses to open
scenario-tool fetch-map --config configs/scenario/antakya-demo.toml --out FILE
rescue-dispatch map-layers --out FILE [--extras FILE]
python3 tools/map/render_map.py --layers FILE --out DIR [--mbtiles FILE] [--max-zoom 19]
sar-check phase6 [--quick] [--max-minutes 45]
```
