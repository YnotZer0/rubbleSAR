# Changelog

## 0.9.3-phase8 (2026-10-21)

From the phase 7 run's recording (P5-04 explained) and the review of the operator view: the order the drones fly in, and names instead of numbers. Both machines: edge-eye changes (the scout's survey order), and the map must be drawn again.

- The scout surveys the collapse sites in the shortest round trip from its home (every order tried for up to 8 sites), not in the scenario's numbering: on the acceptance run it flew s01 → s02 → s03 across the river and back with s05 next to s02. `survey_sites` in edge-eye.toml still sets an order by hand. Its telemetry carries the order (`sar.v1.Telemetry.survey_sites`), and the operator view and the presenter draw it as a numbered dashed line from home round the sites, until the survey is done.
- mission-planner: among close looks of the same urgency, the one nearest a free investigator goes first (a look waiting 4 minutes goes first in its tier), so the investigators work through a site instead of crossing the incident in the order things were found.
- Collapse sites are named after their nearest street ("Gündüz Caddesi collapse"; two by the same street get the nearest other street too) on the offline map, the TAK map's rubble shapes, the live view and the casualty cards, instead of s01 to s05. Draw the map again (`scripts/zbook/36-map.sh`) for the tiles.
- Drone trails on the live map are the last minute and a half (three in the presenter), fading, so they no longer read as routes.
- P5-04 failed on the ZBook because a Red that already had a team (C-014, TEAM-3 from 11:15:40) was merged into another record (C-015) and its assignment followed it: the check looked the team up by the new id only and took C-014 for waiting. It now follows each assignment through merges. rescue-dispatch did nothing wrong.
- The demo video's TAK chapter takes its footage from WinTAK's recording, or the ATAK phone's when WinTAK was not recorded or its file has not been copied to the ZBook (`insert = "wintak|atak@…"` in the storyboard: run `rubblesar demo init` after moving your old `storyboard.toml` aside to get it).
- `rubblesar demo check` lists the TAK teams and whether their devices are online: only an online TAK team is sent casualties, so the TAK chapter needs one, and each online one must answer (the phase 7 run's VADER, WinTAK left open, lost three casualties by not answering). docs/PHASE8.md says how to make the ATAK phone a team.
- `rubblesar demo check` and P8-06 say what a failed OBS connection most likely means: "connection refused" (that machine answered, but OBS is not running there or its WebSocket server is off), no answer (wrong address, machine off, firewall), wrong password (close code 4009).

## 0.9.2-phase8 (2026-10-21)

From the phase 7 acceptance run on the ZBook (21 Oct 2026: P7-05 and P5-04 failed, everything else passed) and the first look at the operator view. ZBook only.

- The operator view leads with what the drones found: a card per casualty with the colour and thermal pictures, the body parts the model outlined and its confidence, the temperature, movement and what is being done for the person, most urgent first, and the latest reports above. The map is smaller, beside it, with the timeline, the chat, the drones and the teams in tabs. The presenter's "For everyone else" chapter shows the same cards. The presenter page is otherwise unchanged.
- P7-05: the timeline missed the "sent to" line of an assignment that rescue-dispatch reported for the first time already on scene (a team a few metres from its next casualty gets there between two dispatch states), and of one that followed its casualty into a merged record. Both now get their line ("TEAM-1 now sent to C-015: C-020 was merged into it").
- P5-04: when rescue-dispatch gives a Yellow a team while a Red waits, because no free team can reach the Red or every free team has declined it, the assignment's note now says so, and P5-04 reports it as a warning with that reason instead of a failure. Without such a note it still fails.
- The presenter's casualty list no longer ends with "and 0 more".

## 0.9.1-phase8 (2026-10-21)

From the first try on the ZBook: `90-phase7-test.sh` stopped at once with `tool_bin: command not found` (E-SETUP-127). ZBook only.

- `90-phase7-test.sh`, `90-phase8-test.sh` and `92-demo-setup.sh` now load the helper that finds the installed programs (`scripts/lib/scenario.sh`); 0.9.0's did not. A new test (`internal/setupscripts`) checks that every script loads the helpers it calls, and that every script parses, so this cannot ship again.
- A command failing inside `$(...)` or a pipeline printed the COPY block twice, one inside the other; the script now prints one.
- The demo video no longer names the laptop's make: the narration, the title card, the panels, the camera and model notes and the timeline say "the command post" or "a GPU-equipped laptop", set by `command_post` in `storyboard.toml`. A test keeps "ZBook" out of the pages and the built-in storyboard.
- The narration's blackout length is measured from the last message heard (it was 6 s short), and says how many reports were lost if any were.
- A deadlock between a replay and its picture, which could freeze a narrated take (seen once in about ten test runs), is fixed, with a test.

## 0.9.0-phase8 (2026-10-21)

Phases 7 and 8 in one release (0.8.0-phase7 was not released on its own): the live operator view, and the tooling to record a narrated demo video of the whole system. ZBook only; nothing changes in how the Jetson works (rebuild it only to match versions in `rubblesar status`). No configuration changes in `sar.toml`.

Phase 7 — the live operator view:
- sar-web serves `/ops/`, a live dashboard for people without TAK, in any browser on the network: the offline map with the rubble, drones (heading, height, trail), casualties in their triage colour (ring while provisional, green once freed) with error circles, close-look lines, teams and routes; casualty lists by urgency with a card per casualty (the reason for its colour, temperature against the rubble, close look, team, the colour and thermal crops the drones sent); drones (task, battery, link, reports waiting on board, model and frame rate); teams; drone report thumbnails; the chat; a plain-language timeline. Light and dark look; embedded Barlow fonts (OFL); no framework, no external requests.
- `internal/live`: the picture built from the bus (telemetry, detection packets with crops, casualty lists, tasks, dispatch state), seeded from the services' APIs at start; server-sent events (full picture, then changes, gzip). A drone that goes dark is noticed by its silence (6 s): "not heard", "heard again … N reports waiting", "queue empty: every report … acknowledged". API: `/v1/scene`, `/v1/picture`, `/v1/timeline`, `/v1/stream`, `/v1/crop/<id>`, `/v1/thermal/<id>`.
- sar-web is built with zenoh (Makefile `CGO_TOOLS`) and its unit starts after zenohd.
- The run recording (`events.jsonl`) also keeps the casualty details, every detection with its crops and frame position, the simulation's truth of those frames, each drone's link and perception figures, and the teams' state.
- `sar-check phase7` (P7-01 served and connected, the phase 6 run, P7-02 the picture against casualty-analyst, mission-planner and rescue-dispatch every 20 s, P7-03 the event stream, P7-04 the crops, P7-05 the timeline against the after-action report); `scripts/zbook/90-phase7-test.sh`.

Phase 8 — the demo video:
- `rubblesar demo`: `init` (settings and an editable storyboard in `~/.config/rubblesar/`), `check`, `obs-setup` (creates OBS scenes: the presenter page and the operator view as browser sources on the ZBook, a window capture on the WinTAK laptop), `test-record`, `live` (a mission recorded by every recorder, with a radio budget cut and a blackout staged on scout-1, beats marked), `media` (a scout frame rendered again exactly as the drone saw it, from its recorded pose, clock and seed, with the model run on it), `facts`, `script`, `replay` (the narrated take: 14 chapters on the presenter page, recorded by OBS with chapter marks; `--rehearse`, `--chapter`), `voice` (text to speech, optional), `cut` (ffmpeg rough cut: the take, the WinTAK footage over the TAK chapter, the voice), `takes`.
- Presenter page `/ops/?present=1`: the operator view with a caption bar, explanation panels (incident, drones, camera, model with tiles and outlines, report packet, radio charts, command post, triage rules, TAK chat, after-action figures), map moves and title cards, driven by cues from the director (`POST /v1/present`, `/v1/replay`, accepted from the ZBook itself only). Replays of recorded runs (`?src=replay`) with interpolated drone positions.
- Each replay take writes `cues.json`, `plan.json`, `narration.txt`, `narration.srt` (subtitles), `shotlist.md`; the narration's numbers come from the run (`rubblesar demo facts`), and say what was staged.
- OBS Studio driven over obs-websocket v5 (`internal/obs`, with a fake OBS for tests); scrcpy for an ATAK phone; `scripts/zbook/92-demo-setup.sh` installs ffmpeg, OBS (PPA, Flathub fallback), scrcpy and adb.
- `sar-check phase8` (P8-01 settings and storyboard, P8-02 a run with the storyboard's beats, P8-03 the frame rendered and the model run, P8-04 the narration, P8-05 sar-web's replay and presenter, P8-06 every recorder's test recording, P8-07 ffmpeg and text to speech, P8-08 a rehearsal with `--rehearse`); `scripts/zbook/90-phase8-test.sh`.
- New error codes E-WEB-002..003, E-DIR-001..007, E-CHK-701..705, E-CHK-801..808, E-TEST-PHASE7, E-TEST-PHASE8.

## 0.7.2-phase6 (2026-10-20)

After the phase 6 acceptance run on 0.7.1 (20 Oct 2026: every check PASS, report `20261020T202209Z`). ZBook only; nothing changes on the Jetson.

- WinTAK did not list the offline map (copied into the Imagery folder: not shown; dragged on: "import failed"), nor the phase 5 `openstreetmap.xml`. Both files had a comment before `<customMapSource>` and the URL in CDATA; the map source is now written exactly like the files WinTAK users load (github.com/joshuafuller/ATAK-Maps): declaration, root element, plain URL. Confirmed on the acceptance laptop: listed under MOBILE → Online and drawn. The MBTiles route is documented as the WinTAK Quick Start Guide gives it (copy into `C:\ProgramData\WinTAK\Imagery`, MOBILE → Local); both routes work in WinTAK. sar-web also answers tiles of an installed map (any zoom 0 to 22, inside the tile grid) with a plain tile in the colour of the map's faded edge instead of 404, and the map source declares zoom 0 to 19. The sar-web page, `36-map.sh` and docs/PHASE6.md say to copy the file (quit WinTAK first) rather than drag it, link a test tile to open on the laptop, and docs/PHASE6.md has "WinTAK does not list the map".
- `20-build.sh`: when the Go toolchain itself crashes (a fault in the compiler or vet, damaged export data in the build cache) rather than a test failing, it clears the Go build cache and runs the step once more, saying so; twice in a row stops with E-BUILD-CACHE. Seen once on the ZBook with 0.7.1 (`fatal error: fault` in `go/internal/gcimporter` while vetting `internal/analyst`, which `go vet` had passed minutes before).

## 0.7.1-phase6 (2026-10-18)

Fixes for 0.7.0's `20-build.sh`, which stopped in the unit tests on both machines. Nothing changes in how the programs behave.

- Jetson: `TestPhase6InProcess` failed with `render_map.py: exit status 1`. The map renderer used `Image.Resampling`, which Pillow added in 9.1; Ubuntu 22.04 (JetPack 6) ships Pillow 9.0.1. It now works with either. Without a DejaVu font it stops with E-MAP-004 and says what to install, instead of failing later on Pillow's built-in font.
- ZBook: `TestPhase4InProcess` failed with `missing "[PASS] P4-05"`. P4-05 warns when an investigator held more than a tolerance away from the task height, judged from the height the drone reports. That height carries the simulated GPS height error (about 0.75 m, correlated over 30 s), and over a 30 s hold its median drifted past the 2 m tolerance on about 1 hold in 400 (seen once in testing: `held at 12.6 m instead of 15 m`). The tolerance is now 3 m, which 40,000 simulated holds never reached (worst 2.79 m).
- `20-build.sh`: when a test fails, the copy block now shows the failing test in full (its log, the acceptance table it printed, a Python traceback) instead of only the first line of each log entry. That is why the 0.7.0 blocks did not show the cause.

## 0.7.0-phase6 (2026-10-18)

Phase 6 — one application, end to end (scope agreed: the offline map, one-command operation, the after-action report; TLS later). Both machines (the Jetson only for the version in its telemetry). Includes 0.6.2.

- Offline incident map for WinTAK and ATAK: `scripts/zbook/36-map.sh` downloads the buildings, water and green areas once (`scenario-tool fetch-map`), takes the streets as rescue-dispatch routes over them (`rescue-dispatch map-layers`: the same closures by rubble and by OpenStreetMap's access tags) and the scenario's collapsed buildings and rubble, and draws tiles from zoom 13 to 19 with `tools/map/render_map.py` (Python and Pillow, offline tooling): street names, the river, buildings, hospitals, streets closed by rubble in red, streets closed in OpenStreetMap in red dashes, the collapses with hatched rubble, the command post; plus an MBTiles file, a legend and an overview. Works without internet once drawn. OpenStreetMap's own tile servers now refuse many apps, which is why WinTAK stopped short of street level.
- New ZBook service `sar-web` (`[web]`, port 8090, read-only): the tiles (a WinTAK/ATAK map source that streams from the ZBook, its XML generated for the address the device uses), the MBTiles download, the after-action reports, and an index page. `sar-web status`.
- The scenario's rubble as brown shapes on the live TAK map (`[dispatch] show_collapses`).
- New command `rubblesar`: `status` (every ZBook service, edge-eye on the Jetson over SSH, each drone with its software, with fixes), `up` / `down` (both machines, in order), `mission start` (new run: reset, missions restarted, recorded until the survey, the close looks and the rescues are finished, `[ops] max_mission` or Ctrl-C; then the report), `report` (write a recorded run's report again, `--list`). `scripts/zbook/38-jetson-link.sh`: SSH key and a sudo rule on the Jetson for `systemctl start|stop|restart edge-eye`, `[ops] jetson`.
- Run recording and after-action report (`internal/aar`): the run's casualty lists, merges, tasks, thinned telemetry, assignments, team positions and the chat (rescue-dispatch `GET /v1/chats`) in `events.jsonl`; a self-contained page with headline numbers, the map (offline tiles stitched at the zoom that fits, rubble, flight paths, routes, casualties green once rescued), a timeline per casualty and per team, the casualties, the comparison with the scenario's victims, teams, drones, assignments and the chat. `/var/lib/rubblesar/www/reports/<incident>/<run>/`.
- `sar-check phase6` (P6-01 both machines, P6-02 the offline map drawn and served, with the rubble and closed streets checked on the tiles, P5-01..06 the phase 5 run recorded, P6-03 the report written, served and consistent with rescue-dispatch and casualty-analyst); `scripts/zbook/90-phase6-test.sh`. The phase 5 suite can share a report (`Phase5Options.Report`).
- edge-eye reports its software version in the telemetry (`sar.v1.Telemetry.software`).
- `sar-doctor` DOC-Z10 (sar-web, the offline map). `30-install.sh` installs sar-web and rubblesar, creates `/var/lib/rubblesar/www/reports` for you, opens 8090/tcp; `uninstall.sh` and `collect-diagnostics.sh` know them.
- New config sections `[web]`, `[ops]`; new error codes E-MAP-001..003, E-WEB-001, E-OPS-001..002, E-AAR-001..003, E-CHK-601..603, E-TEST-PHASE6, E-SETUP-PKG.

## 0.6.2-phase5 (2026-10-18)

From the second acceptance run (passed) and the first look at it on WinTAK. ZBook only; the Jetson stays on 0.5.1.

- Extrication: a team on scene first frees its casualty from the rubble, for a time estimated from the scenario (pancake collapse or rubble mound, depth of the pile, what shows of the person and how much of it is covered), the team (vehicle teams carry heavier tools) and a variation per casualty and run: 15 min to 6 h, a light, medium or heavy rescue; then `on_scene` to hand over. A casualty Black on arrival takes 10 min to confirm and mark. Simulated teams do it `rescue_time_scale` (30) times faster; chat, markers, `rescue-dispatch status` and `sar-monitor` give the real times (`TEAM-2 reached C-007 RED ... Heavy rescue, about 2 h 20 min (pancake collapse, 2.6 m of rubble; only a forearm and hand showing, 7% covered).`). New `[dispatch]` keys `extrication`, `rescue_time_scale`; `sar.v1.Assignment` fields `extrication_s`, `extrication`, `free_unix_ns`.
- A Red or Yellow casualty a team has freed turns green on the TAK map, labelled `C-007 RESCUED` ("RESCUED by TEAM-2" in its details). `sar-check phase5` P5-02 expects it.
- A TAK team that neither answers SAR-OPS (`ok`, `status`) nor moves 20 m towards its casualty within `[dispatch] tak_ack_timeout` (3 min) loses it to another team, so a device left at a desk cannot hold a casualty up. On arrival a TAK team is told the expected extrication.
- The release config lists the WinTAK laptop (callsign VADER) as a TAK team; docs/PHASE5.md: giving WinTAK a position at the incident (Stationary GPS), replying to SAR-OPS, and keeping the chat window on screen.
- `sar-check phase5` waits up to 25 min (was 15) for every Red and Yellow to be reached, and once they are, at most 8 more minutes for teams still freeing a casualty.
- Docs: OpenStreetMap's tile servers now refuse many apps, so the online street map in WinTAK may stop short of street level; the next phase serves a map from the ZBook.

## 0.6.1-phase5 (2026-10-18)

Route fixes from the first acceptance run on the real Antakya streets (it passed; the routes were checked against the dispatch state and street file sent back). ZBook only; the Jetson stays on 0.5.1.

- Getting on and off the streets: a route may start on any open street within 60 m of the closest one to the team and leave the streets within 60 m of the closest one to the casualty, at 10 s per metre walked beyond the closest point. Before, it had to use the closest point, so a casualty beside a dead end or a street reached only from the far side of a block got a long way round (C-032: 1,859 m, now 356 m).
- OpenStreetMap access tags: `access=no` closes a way unless `foot`, `vehicle` or `motor_vehicle` opens it again; `foot=no` closes it on foot, `motor_vehicle=no` by vehicle. 0.6.0 dropped every `access=no` way, including pedestrian streets marked `foot=yes` (Uzunçarşı Caddesi), and ignored `foot=no` (the footbridges by the north-west collapse).
- Rivers: the street download includes `waterway=river|canal` centre lines, and no walk off the streets (to the first street, the final walk, a casualty tried straight across) crosses one. Without them a route could leave the streets on one bank and walk across the Asi. `32-roads.sh` downloads a street file from 0.6.0 again (it has no rivers), keeping the old one if the download fails; `rescue-dispatch roads` and the status show the rivers, and warn when an OpenStreetMap file has none.
- `sar-check phase5` P5-03 fails a route whose walk off the streets crosses a river.
- The release config's command post (`staging_lat`, `staging_lon`) is now 36.20160 N, 36.15850 E, on the west bank of the Asi by four of the five collapses. With the central bridge (Ata Köprüsü) closed in OpenStreetMap, teams from the east bank went round by the next open bridge: the sum of the routes on foot from the command post to the five collapses is 2.0 km instead of 6.2 km.
- TAK teams count as offline after `team_offline` (3 min) without a position, in the TAK user list as well as for dispatch; a merged casualty's route is sent to the TAK map again under the surviving record.

## 0.6.0-phase5 (2026-10-18)

Phase 5 — rescue routing and the TAK map (phases 5 and 6 of the original plan, combined; ZBook only, the Jetson stays on 0.5.1).

- New ZBook service `rescue-dispatch` (`[dispatch]`, API on 127.0.0.1:9104): rescue teams (`[[dispatch.team]]`: simulated, or TAK users by callsign), assignments most urgent first (settled Red, provisional Red, Yellow; Black and Grey never), the free team with the quickest route within a priority, no taking a team off a casualty; arrival within `arrive_m`, `on_scene` time, release; withdrawal when triage changes or a TAK team goes offline; assignments follow merged records. State on `sar/{incident}/dispatch` (`sar.v1.DispatchState`, `proto/sar/v1/dispatch.proto`); casualties reached kept in `/var/lib/rubblesar/dispatch/<incident>.json`. `rescue-dispatch status|reset|route|roads|check-config`.
- Built-in street router (instead of Valhalla, which the plan had; nothing extra to install): OpenStreetMap streets downloaded once (`scenario-tool fetch-roads`, `scripts/zbook/32-roads.sh`; `--synthetic` street grid without internet), cut into 10 m pieces, closed within `block_margin_m` of the scenario's rubble spill areas; Dijkstra on foot (1.4 m/s) or by vehicle (by street class, one-way streets, no footways); the route leaves the streets at the closest reachable point and walks the rest (1.0 m/s, 0.5 m/s over rubble); a casualty within 50 m is also tried straight across.
- TAK: taky 0.10 in Docker (`rubblesar-taky` service, plain TCP 8087, daily restart for taky's memory leak; `scripts/zbook/34-taky.sh`). rescue-dispatch is a TAK client (reconnecting, whole picture again on every connection): casualty spot markers in the triage colour with reason, summary, CEP and rescue state; drones; investigation lines; simulated teams (each on its own TAK connection); route lines; SAR-OPS. Chat in All Chat Rooms for every dispatch, arrival, finish and withdrawal, direct messages to TAK teams; teams reply `done`, `unable`, `status`, `ok`. How to connect ATAK-CIV and WinTAK (TCP 8087, no SSL): docs/PHASE5.md.
- `sar-check phase5` (P5-01..06): the phase 4 run with rescue-dispatch reset; sar-check joins taky as team CHECK-1, walks the routes it is given and replies `done`; checks the TAK map, the routes against the rubble, the dispatch order and arrival of every Red and Yellow, the TAK team loop and the chat. `--quick` reuses the last run's casualties. `sar-check tak` (T-01..05): TAK server routing. `scripts/zbook/90-phase5-test.sh`.
- `sar-monitor`: rescue team table. `sar-doctor` DOC-Z9 (taky, street file, rescue-dispatch). `scripts/zbook/30-install.sh` installs rescue-dispatch; `uninstall.sh` and `collect-diagnostics.sh` know the new services.
- New error codes E-DSP-001..002, E-TAK-001..002, E-TAK-004, E-RTE-001..003, E-CHK-511..516, E-TEST-PHASE5, E-SETUP-TAKY, E-SETUP-ROADS, E-SETUP-CFG.
- Phase 4 hooks for phase 5: `Phase4Options.ExtraProgress` and `StartedRun`; the in-process phase 4 set-up is shared by the phase 4 and phase 5 tests.

## 0.5.1-phase4 (2026-10-17)

Fixes from the first acceptance run with the real model (P4-02 failed: victims split into several records; most still victims were seen moving, so triage was 41 %).

- Movement: a changed outline counts only when the picture changed where it did. The real model's outlines change on their own (a forearm's outline takes in the hand in one frame and not the next, a part's class flips, a track picks up a second outline of the same limb), which made 13 of 17 still victims "move". The pixels where a part's outlines disagree are compared with the hover's first frame (within the outline tolerance, after matching the exposure on the ring of rubble around the part); more than `movement_change` of them, beyond the ring's own share, must have changed. casualty-analyst calls a casualty moved when its parts moved in `[analyst.triage] moved_frames` (2) frames of one hover aimed at it in all, and only hovers aimed at that casualty count for its movement and watch time.
- Edge tracking: every part seen during a hover carries the hover's task (before, only those in the middle of the frame did, and the others were registered as a separate stretch of observation, duplicating people); each hover has its own tracks (a part seen again from elsewhere gets a new track: positions from two viewpoints disagree by the unknown height of the rubble, and one track mixing them put one leg in five places over 4 m); outlines of one class that mostly overlap in a frame are one part (the model sometimes outputs a whole leg and its lower half); the object-height error of an off-centre sighting no longer shrinks as its frames are averaged, so its CEP shows it.
- Fusion: parts of one class within `[analyst.fusion] duplicate_m` (1.5 m) of each other count once against a person's limits (two hands, arms and legs, one head and torso); a real model reports one leg as two or three pieces, which split one victim into two or three records. In a hover, a part reported in only a few frames (a quarter of the best-seen part's or fewer) joins a part followed throughout within 3 m, and with none near it does not start a casualty of its own (it can still support one another window found).
- Skin-temperature trend: per kind of part (a hand with a hand, never with a sleeve), one value per look (the median of its frames), at least three looks over `trend_min_span`, slope as the median of the slopes between looks; a trend beyond ±10 °C/h compares readings of different things and is not reported. Mixed parts and every frame counted gave trends of ±10–34 °C/h.
- `edge-eye selftest` ST-06 hovers as an investigation does (target off by up to a metre, any heading) over up to two moving and five still victims with the installed model, instead of one of each centred.
- `sar-check phase4`: records closed by the system ("nothing found on 2 close looks") are listed but no longer count towards the false-record warning of P4-02; default time limit 45 min (the run took 33).

## 0.5.0-phase4 (2026-10-17)

Phase 4 — casualties, triage and investigator drones.

- casualty-analyst merges the stored reports into casualty records (`sar.v1.Casualty`, `proto/sar/v1/casualty.proto`): per drone, parts seen together within 2 m (2.5 m for related parts) are one person if they look like one; across drones and passes, each stretch of observation (a hover, or up to 30 s of a survey) is matched one to one (Hungarian method) after finding the shift between its GPS error and the casualties known so far, and registered to a common frame. Stable ids with `merged_into`; state kept in `casualties-state.json`. Published on `sar/{incident}/casualty/{id}` and `sar/{incident}/casualties`; API `GET /v1/casualties`, `POST /v1/reset`; `casualty-analyst status` lists the casualties, `casualty-analyst reset --yes` archives a run.
- START triage from the thermal readings and the close look (Red, Yellow, Black, Grey; provisional until seen close up); warm needs bare skin in the skin band and 3 °C above ambient or, when only clothing is read, 15 °C and 8 °C above ambient (`[analyst.triage] clothed_min_c`, `clothed_delta_c`: sun-warmed clothing on a cooled body is not warm); skin-temperature trend reported. New sections `[analyst.fusion]`, `[analyst.triage]`.
- New ZBook service `mission-planner` (`[planner]`, API on 127.0.0.1:9103): investigation tasks for casualties that need a close look, by tier (provisional Red, unknown, uncertain position), to the nearest free investigator with the battery for it (25 % reserve); pre-emption of less urgent tasks; requeue on refusal, no answer, abort or no progress; at most 2 looks per casualty; task list on `sar/{incident}/tasks` (`sar.v1.TaskList`), kept in `/var/lib/rubblesar/planner/<incident>.json`.
- edge-eye: behaviour `standby`; `Investigate` and `CancelTask` commands; a task flies there, holds over the target with a fixed heading (drifting `hover_wander_m`), reports every part seen within the radius (reason `INVESTIGATED`, with movement and task id), waits `task_loiter_s` and flies home or resumes its survey; telemetry carries the task, flight time left, cruise speed and home. `edge-eye status` shows the tasks.
- Movement detection while hovering: frame registration (coarse to fine) plus local registration of each part against its surroundings (parallax), outline change with a pixel tolerance, only outlines in the middle of the frame, complete jumps ignored, one verdict per hover (`[perception] movement_change`, `movement_tolerance_px`, `movement_min_frames`). `[perception] detection_sigma_m` (1.0 m, from the phase 3 runs) is added to every CEP. `[perception] debris_height_m` (release config 2.5 m): parts are geolocated on the top of a typical pile instead of the ground.
- Tracker: the association gate allows for the GPS error drifting since the sightings a track's position comes from, so a part watched for minutes (or seen again on a later visit) keeps its track instead of getting a second one.
- `sar-check phase4` (P4-01..08): archives the previous run, restarts the drones, waits for the survey and the investigations, scores the records against the scenario (records per victim, positions, investigations triggered and flown, triage accuracy, movement, budget and storage). `edge-eye selftest` ST-06 (movement with the installed model). `sar-monitor` shows the casualties and tasks. `sar-doctor` DOC-J7, DOC-Z8.
- Release configs: investigators on standby at 10 m/s; ZBook config with the new sections. Scripts `90-phase4-test.sh` (both machines); `zbook/30-install.sh` installs mission-planner and moves reports stored before phase 4 to `archive/`.
- New error codes E-EDGE-007, E-ANL-003, E-PLN-001..002, E-CHK-306, E-CHK-411..418, E-TEST-PHASE4.

## 0.4.2-phase3 (2026-10-17)

Fix: reports could be lost on the ZBook after edge-eye restarted.

- Cause: track numbers started again at 1 whenever edge-eye started, so a new report could carry an id already stored (`inv-1-0001.1`). casualty-analyst, which remembers the stored ids across restarts, took it for a repeat: it acknowledged it, so the drone deleted it, but did not store it. Restarting a mission (what the acceptance run does) kept the numbering and was not affected, so the acceptance run could not see it.
- Track ids now carry a run token, the time edge-eye started in base 36: `inv-1-mgy5x1k2-0007.2`. They never repeat across restarts.
- casualty-analyst treats a report as a repeat only when both its id and its creation time match. The same id with another creation time (an older edge-eye) is stored as `<id>~<time>` with its own crops, counted as an id conflict and shown by `casualty-analyst status`.
- P3-06 (now "nothing lost (outage, storage)") also checks that casualty-analyst stored every report that arrived during the run, from its stored counts before and after.
- `sar-monitor`: wider id column.

## 0.4.1-phase3 (2026-10-17)

Fix: edge-eye crashed on the Jetson at start-up when the service account could not use the GPU.

- Cause: on JetPack 6.2 CUDA also needs the DRM render node `/dev/dri/renderD*` (group `render`); the service account `rubblesar` was only in `video`. TensorRT then failed with "operation not supported" and crashed the process (SIGSEGV), which the installer reported as a network problem.
- `scripts/jetson/30-install.sh` adds `rubblesar` to the groups that own the GPU device files (nvmap, nvgpu, host1x, render node; not the profiler/debugger nodes of group `debug`) and loads the model exactly as the service would (same account and sandbox, via `systemd-run`) before starting it. If that fails it stops with E-SETUP-GPUACC and shows the model loading as the service, as `rubblesar` and as you, with the device files and their groups. `scripts/jetson/90-phase3-test.sh` runs the same check.
- The TensorRT back end checks that CUDA works (device, context, a 1 MB allocation) before touching TensorRT, treats any TensorRT error message as a failure, and catches C++ exceptions. A GPU that cannot be used now gives E-INF-006 naming the account and its groups. edge-eye logs it and keeps flying without perception instead of crashing.
- When edge-eye does not become healthy, the installer reads the journal of this start. It says "crashed while starting" and shows the start of the crash (without the register dump) when edge-eye crashed, "cannot reach the zenoh router" only when that is the error, and otherwise shows the log.

## 0.4.0-phase3 (2026-10-16)

Phase 3 — the edge pipeline.

- edge-eye perception, per drone at `frame_hz` (default 1 Hz): virtual colour and thermal frames from the true pose (the reported pose drifts: Gauss–Markov GPS and heading errors); the TensorRT model shared between the drones; thermal cue (skin temperature under each mask against its surroundings, warm spots no mask covers, hot objects rejected); flat-ground geolocation with a CEP from the pose errors; a tracker that confirms a part after 2 sightings and reports updates only on material change; report packing (JPEG crop ≤ 128 px, 16-bit thermal PNG, outline ≤ 32 points, one-line summary).
- Survey missions: behaviour `survey` (lawnmower over the configured sites with the camera footprint and side overlap, then home and land); new `[[uav]]` keys `survey_sites`, `survey_speed_mps`, `side_overlap`, `yaw_noise_deg`, `frame_hz`.
- Store-and-forward queue on disk per drone (`[outbox]`): priorities (new warm parts first), payload reduction by backlog (10/30/60 s of budget), re-sending with backoff, probe mode in long outages, telemetry first; survives restarts.
- New ZBook service `casualty-analyst`: stores reports in `detections.jsonl` with crops, drops duplicates, acknowledges after writing to disk; status API on 127.0.0.1:9102 (`[analyst]`).
- New messages `Detection`, `DetectionAck`, `SimFrameTruth` (`proto/sar/v1/detection.proto`); control commands `RestartMission`, `SetLinkOutage`; telemetry `PerceptionStats` and queue fields in `LinkStats`. New keys `…/uav/{uav}/detection`, `…/uav/{uav}/ack`, `sar/{incident}/sim/uav/{uav}/truth`.
- `edge-eye selftest` (ST-01 … ST-05) and `sar-check phase3` (P3-01 … P3-08: readiness, packets, budget, position error, victims found, outage, delay, Jetson speed). `sar-monitor` shows the pipeline, queues and latest detections; `edge-eye status` the mission and pipeline.
- Phase 2 evaluation: the pieces of one victim's part count as one object; person-level recall, results by size, confusion table, `sar-infer eval --conf`. `90-phase2-test.sh` prints both counts.
- Dataset v2 config (`configs/dataset/v2.toml`): one label per victim's part (pieces joined, pieces under 3 px left out); v1 unchanged.
- `sar-doctor`: DOC-J6 (phase 3 config on the Jetson), DOC-Z7 (casualty-analyst running).
- Scripts: new `scripts/jetson/90-phase3-test.sh`, `scripts/zbook/90-phase3-test.sh`; `scripts/zbook/30-install.sh` installs casualty-analyst; `scripts/jetson/30-install.sh` creates the queue folder and warns when the config has no `[perception]`. `80-dataset.sh`, `81-train.sh` and `82-evaluate.sh` stop with E-SETUP-DATA when a new release folder has no `data/` but an earlier one next to it does, and print the `ln -s` command; Docker mounts use the real paths.
- `81-train.sh --quick` without `--dataset` now uses the quick dataset (`data/datasets/rubblesar-v1-quick`) instead of looking for the full one; when a dataset is missing it says which folder it looked in and lists the datasets that exist; it prints `--quick` in the suggested `82-evaluate.sh` command.
- Setup scripts accept relative paths from the release folder or the current folder (`--dataset`, `--run`, `--config`, `--from`), e.g. when run from inside `scripts/zbook`.
- New error codes E-EDGE-005…006, E-ANL-001…002, E-CHK-301…305, E-CHK-311…318, E-TEST-PHASE3, E-SETUP-DATA.

## 0.3.0-phase2 (2026-10-16)

Phase 2 — the body-part model.

- `scenario-tool dataset` (`configs/dataset/v1.toml`): training set from 36 varied simulated incidents (30 train, 6 validation), 80 drone poses each (investigator and scout, nadir and oblique, noise and blur), 640 px tiles, exact outline labels in YOLO format, preview, manifest; resumable. The installed `antakya-demo` scenario is the held-out test set (71 frames, re-rendered on demand).
- Training container (`training/`, Docker, GPU): Ultralytics YOLO11-seg 8.3.253, checksum-verified starting weights, ONNX export, model card with metrics; Ultralytics predictions for the decoding comparison.
- `sar-infer`: TensorRT 10 (Jetson, FP16) and ONNX Runtime 1.22 back ends behind one Go interface; tiling, batching, decoding, class-aware NMS, masks, cross-tile merging; `info`, `bench`, `frame`, `eval`, `package`, `golden`, `parity`, `stub-model`.
- `sar-check phase2`: P2-01 … P2-05 on each machine (model folder, back end, agreement with Ultralytics / with the ZBook, accuracy on held-out frames, geolocation / Jetson speed). It prints its results block even when everything passes, so the numbers can be shared and agreed.
- `sar-doctor`: DOC-10 ONNX Runtime, DOC-11 model installed; DOC-J3 checks TensorRT headers and `trtexec`; DOC-Z2 accepts the NVIDIA Container Toolkit via `nvidia-ctk`; `sar-infer` added to DOC-08.
- Scorer: parts too small to be expected are ignored rather than counted as false alarms.
- Virtual camera: optional extra sensor noise and blur (used for training variety).
- New scripts: `12-onnxruntime.sh` (both), `scripts/zbook/80-dataset.sh`, `81-train.sh`, `82-evaluate.sh`, `85-push-model.sh`, `90-phase2-test.sh`; `scripts/jetson/75-tensorrt.sh`, `80-engine.sh`, `90-phase2-test.sh`. The `20-build.sh` scripts also run the inference tests (and the training-script tests on the ZBook); `fetch-downloads.sh` includes ONNX Runtime.
- New error codes E-DS-001…002, E-INF-001…005, E-TRN-001…007, E-CHK-201…206, E-TEST-INFER, E-TEST-PYTHON, E-TEST-PHASE2, E-SETUP-ORT, E-SETUP-DOCKER, E-SETUP-GPU, E-SETUP-TRT, E-SETUP-MODEL.

## 0.2.0-phase1 (2026-10-16)

Phase 1 — scenario and imagery.

- `scenario-tool`: seeded scenario generator (`configs/scenario/antakya-demo.toml`: 5 sites, 23 victims, statuses, exposures, thermal and movement models, decoys, rubble-spill blockages; optional real building outlines from OpenStreetMap), procedural 2.5D site renderer with per-site layers and pose-B patches, virtual colour (1920×1080, 70°) and radiometric thermal (640×512, 50°, centikelvin) cameras with sun shadows, thermal inertia, sensor noise and pixel-exact labels, preview pages, detection scorer.
- `sar-check phase1`: eight acceptance checks P1-01 … P1-08 (no network needed); runs on the ZBook and the Jetson.
- `sar-doctor`: DOC-09 reports whether the scenario is installed; `scenario-tool` added to DOC-08.
- New scripts: `scripts/zbook/60-scenario.sh`, `65-push-scenario.sh`, `70-phase1-test.sh`; `scripts/jetson/60-scenario.sh`, `70-phase1-test.sh`.
- New config key `[site] scenario_dir` (optional; default `/var/lib/rubblesar/scenarios/<incident_id>`).
- New error codes E-SCN-001…006, E-RND-001…003, E-VCAM-001…002, E-SCR-001, E-CHK-101…108, E-TEST-PHASE1.
- Phase 0 fixes: the acceptance script no longer prints a Python traceback when zenohd's admin API answers with something else; script names in logs no longer show `scripts/./`.

## 0.1.1-phase0 (2026-10-15)

- `sar-doctor --stage prereqs`: after the 00-prereqs scripts, items that later steps install are shown as `TODO` instead of `FAIL`, and no copy-and-paste block is printed for them.
- zenohd's read-only REST admin API moved from 127.0.0.1:8000 to 127.0.0.1:8447 (port 8000 was already in use on the ZBook).
- `sar-doctor` (DOC-Z4) now says when a different program answers on the admin port.
- `scripts/zbook/15-zenohd.sh` checks that ports 7447 and 8447 are free before starting zenohd (new code E-SETUP-PORT).

## 0.1.0-phase0 (2026-10-15)

- First phase 0 release.
