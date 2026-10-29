# Phase 8 — Recording the demo video

**Goal.** One video of a few minutes that explains the whole system to someone who has never seen it: the scenario and the place, how the drones are simulated and what their cameras see, how the model finds people in the images and how they are graded, what the Jetson does, the radio link (budget, shrinking reports, a blackout with nothing lost), what runs on the ZBook, WinTAK and ATAK teams receiving their tasks, and the after-action report. The ZBook directs the recording: it plays a recorded run as a narrated story on a presenter page, and starts and stops the screen recorders on the ZBook and the WinTAK laptop itself, so every take is repeatable and the recordings line up.

**Accepted when** `scripts/zbook/90-phase8-test.sh` ends with `RESULT: PASS` (warnings allowed), and you have made a rough cut with `rubblesar demo cut` and watched it.

**Time needed.** Setup: about 30 minutes (ZBook and the WinTAK laptop). The live take: one mission, 30–50 minutes. The narrated take: about 5 minutes; each retake the same. The rough cut: a few minutes. Your own editing: as long as you like.

> **Everything shown is simulated, and the video says so** in its first and last chapters: keep them. The numbers in the narration are the recorded run's own (`rubblesar demo facts`), not written in by hand. The link cut and blackout in the radio chapter are staged for the test, and the narration says so. Triage thresholds await medical review (D9), which the triage chapter says too.

---

## How it works

```
 rubblesar demo live        a normal mission, recorded as a run (events.jsonl), with a radio budget cut and a
   (30-50 min)              blackout staged on scout-1; OBS records the operator view on the ZBook and WinTAK on the
                            laptop, scrcpy an ATAK phone; the moments that matter are marked (beats)
          │
 rubblesar demo media       renders one of the scout's camera frames again on the ZBook, pixel for pixel as the
                            drone saw it (its recorded pose, clock and noise seed), and runs the model on it
          │
 rubblesar demo replay      plays the run on sar-web's presenter page as 14 chapters: it jumps to each beat, plays
   (about 5 min)            at 2-12x, moves the map, shows an explanation panel and captions; OBS records it, with a
                            chapter mark per chapter; writes the narration, subtitles and a shot list for the take
          │
 rubblesar demo voice       (optional) speaks the narration with a text-to-speech program, one file per chapter
 rubblesar demo cut         ffmpeg: the narrated take, with the WinTAK footage of the live take placed over the TAK
                            chapter (above the captions) and the voice, as rough-cut.mp4
          │
 your video editor          the rough cut, your own voice-over, music, titles
```

The presenter page is the operator view (phase 7) with a caption bar, a panel beside the map, and title cards; open `http://<ZBook>:8090/ops/?present=1` in a browser to see what OBS records.

### The chapters (built-in storyboard)

| # | Chapter | What is on screen | Covers |
|---|---|---|---|
| 1 | RubbleSAR | title card over the map; "everything is a simulation" | — |
| 2 | The incident | the collapse(s) on the offline map; the scenario: buildings, closed streets, teams, the people under the rubble by condition | the scenario, the location |
| 3 | Three drones, one small computer | the scout surveying along its planned tour (numbered, the shortest round trip); each drone's task, battery, survey progress; flight, camera and on-board facts | how the drones are simulated |
| 4 | What the drone sees | a real frame of the run: colour (1920 × 1080, cm a pixel) and radiometric thermal (640 × 512) | the images the drones "see" |
| 5 | Finding people in the pixels | the frame cut into six 640-pixel tiles; the body parts outlined by the model, with confidence; the model, its speed on the Jetson, its accuracy | ML inference |
| 6 | From a detection to a report | a body part's colour and thermal crops, its temperature against the rubble, the report's size against the whole frames | how the content is determined |
| 7 | Built for a poor radio link | each drone's budget, use and queue over time; the shrinking levels; the staged blackout and the queue draining; reports reached / waiting / lost | communications, reduced bandwidth, going dark with no data lost |
| 8 | The command post | the Jetson → Zenoh → the command post's services, with live counts | what the command-post programs do |
| 9 | The close look, and triage | the rules (warm, moving, cooling, rubble temperature) and the evidence for the casualty being watched | how results are graded |
| 10 | Sending the rescue teams | the casualty lists by urgency; teams and routes on the map | dispatch |
| 11 | On the rescuers' own devices | the TAK chat (task, arrival, "done"); in the rough cut, WinTAK's own screen from the live take | WinTAK and ATAK teams assigned tasks |
| 12 | For everyone else | the operator view's picture cards: what the drones found, the pictures, what the model outlined, what is being done | the browser view (phase 7) |
| 13 | After the mission | found, given the expected colour, freed; real-world freeing and travel times; false alarms; per team | the end report |
| 14 | RubbleSAR | end card | — |

The video never names the laptop's make: the command post's computer is "a GPU-equipped laptop" in the narration, the cards and the panels, set once by `command_post` at the top of `storyboard.toml` (e.g. `command_post = "a standard laptop"`). The model is trained on that GPU; during a mission nothing at the command post needs it.

With a real run the narration takes about 4 minutes 50 seconds (`rubblesar demo script` prints it with the run's numbers and the length). Edit `~/.config/rubblesar/storyboard.toml` to change the words, the order, the speeds or what a chapter shows (the file explains every setting; `rubblesar demo facts` lists the numbers you can use).

---

## Before you start

- **Phase 7 installed** (this release) and accepted, or at least `rubblesar up` and `rubblesar status` clean.
- **The model on the ZBook** (`/var/lib/rubblesar/models/current`, from `82-evaluate.sh`), for chapter 5. Without it the frame shows the scenario's own labels and says so.
- **A TAK device for the TAK chapter**: the WinTAK laptop, or an ATAK phone, on the ZBook's network and connected to the TAK server (docs/PHASE5.md), with a callsign that is a TAK team in rescue-dispatch's list ([An ATAK phone](#an-atak-phone-optional)).
- Disk: a few GB free on each recording machine; the live take (30–50 minutes of 1080p) is the long one.

---

## Setup on the ZBook

| Step | Command / action | Expect |
|---|---|---|
| P8-Z1 | `scripts/zbook/92-demo-setup.sh` | ffmpeg, OBS Studio (from the OBS project's PPA; the script prints the Flathub commands if the PPA has no build for this Ubuntu), scrcpy and adb (`--no-scrcpy` to skip), then `rubblesar demo init`: `~/.config/rubblesar/demo.toml` (readable by you only) and `storyboard.toml` | `DONE` |
| P8-Z2 | Start OBS. Auto-configuration wizard: **Optimise just for recording**, base and output **1920 × 1080**, **30 fps** | OBS running |
| P8-Z3 | OBS **Tools → WebSocket Server Settings**: ✓ Enable WebSocket server, port **4455**, ✓ Enable Authentication, **Show Connect Info** → copy the password into `demo.toml`, `[[recorder]] name = "zbook"`, `password = "..."` | — |
| P8-Z4 | OBS **Settings → Output → Recording**: Recording Format **Hybrid MP4** (OBS 30.2 or later: the director's chapter marks are kept) or mkv; a folder with space | — |
| P8-Z5 | `rubblesar demo obs-setup --recorder zbook` (OBS running) | `created scene "RubbleSAR presenter"`, `... "RubbleSAR operator view"`, each a 1920 × 1080 browser source on sar-web |
| P8-Z6 | `rubblesar demo check` | `[ok  ]` for settings, storyboard, sar-web, presenter page (OBS's browser source counts), recorder zbook, TAK teams (one TAK device online), ffmpeg |

Keep OBS open whenever you record; the director switches its scenes, starts and stops it. You do not press Record yourself.

## Setup on the WinTAK laptop (Windows)

1. Install **OBS Studio 30 or later** from obsproject.com (the WebSocket server is built in since OBS 28).
2. **Tools → WebSocket Server Settings**: enable, port 4455, authentication on, note the password.
3. When Windows asks, **allow OBS on private networks**. If it did not ask: Windows Security → Firewall & network protection → Allow an app through firewall → OBS Studio, Private ✓. The ZBook must reach `<laptop-IP>:4455`.
4. Same OBS settings as the ZBook: 1920 × 1080, 30 fps, Hybrid MP4 or mkv.
5. On the ZBook, in `demo.toml`, `[[recorder]] name = "wintak"`: `url = "ws://<laptop-IP>:4455"`, the password, and `enabled = true` (remove the `enabled = false` line).
6. `rubblesar demo obs-setup --recorder wintak`: creates the scene **WinTAK** with a window capture. In OBS on the laptop, open that source's properties and choose the WinTAK window (or replace it with a Display Capture).
7. **WinTAK as a rescue team.** The release config has one TAK team for a laptop, callsign **VADER**. For the live take, set WinTAK's callsign to VADER (Settings → My Preferences → Callsign) and give it a position at the incident (GPS Type: Stationary GPS, 36.20160, 36.15850), as in docs/PHASE5.md. Otherwise (callsign WINTAK-1) the laptop shows the map but never receives a casualty, and the TAK chapter has nothing to show.
8. `rubblesar demo check` on the ZBook: `recorder wintak (live)` and `(replay)` ok.

The WinTAK recording stays on the laptop. Copy it to the ZBook after the live take for the rough cut (any way you like: a USB stick, a share).

## An ATAK phone (optional)

ATAK on an Android phone can be a team too, and can stand in for WinTAK in the video (the TAK chapter's insert uses WinTAK's recording if there is one, otherwise the phone's). Recording the phone's screen is not enough: rescue-dispatch sends casualties only to a TAK team in its list whose device reports a position. So:

1. In ATAK: ☰ → **Settings** → **My Preferences** (or *Callsign Preferences*) → **My Callsign**, e.g. `ATAK-1`; and its TAK server: the ZBook's IP, port **8087**, protocol **TCP**, no SSL (docs/PHASE5.md).
2. Add it to `/etc/rubblesar/sar.toml` (`sudo nano /etc/rubblesar/sar.toml`), next to VADER:
```toml
[[dispatch.team]]
id = "atak-1"
callsign = "ATAK-1"     # exactly the phone's callsign
mode = "foot"
source = "tak"
at_command_post = true  # the phone's own GPS is not at the incident: the team stands at the command post
```
   Without `at_command_post` the team is wherever the phone's GPS says. From another town no street route reaches a casualty (E-RTE-002), so it is never sent one, and the TAK chapter has nothing to show.
3. `sudo systemctl restart rescue-dispatch sar-web` (sar-web reads the teams when it starts; P7-01 warns if it has not been restarted), then `rubblesar demo check`: `TAK teams  ATAK-1 online`, without a warning about its position. If WinTAK is open as VADER it is online too and gets casualties as well: answer it or close WinTAK.
4. During the live take, answer each casualty sent to the phone: `ok` in the chat within 3 minutes (`tak_ack_timeout`), `done` when finished. A team that does not answer loses the casualty to another team.

To record its screen on the ZBook: USB debugging on the phone (Settings → About → tap Build number 7 times; Developer options → USB debugging), connect it, accept the prompt, `adb devices` lists it; then `enabled = true` for `[[recorder]] name = "atak"` in `demo.toml` (it runs `scrcpy -N --record=<take>/atak.mkv` during the live take).

---

## Acceptance

```bash
scripts/zbook/90-phase8-test.sh              # a few minutes; no drones needed
scripts/zbook/90-phase8-test.sh --rehearse   # also plays the whole storyboard on the presenter page (5 more minutes)
```

It needs a recorded run: the phase 6/7 acceptance run will do; a live take (below) is better (it has the staged link events). Send back the COPY block.

| Check | Proves | Pass / warn |
|---|---|---|
| P8-01 | `demo.toml` and the storyboard load and are valid | FAIL on any error (the message names the setting) |
| P8-02 | the recorded run loads and has every beat the storyboard jumps to (survey, first casualty, first Red, outage, first team, first hover, first TAK team) | FAIL with no run; WARN for each beat missing (that chapter plays on from the previous one) |
| P8-03 | a scout frame rendered again, exactly as the drone saw it, and the model run on it | FAIL if it cannot be rendered; WARN if not exact, no model on the ZBook, or nothing found in the frame |
| P8-04 | the narration filled in with the run's numbers; its length | WARN on a chapter warning or outside 2–8 minutes |
| P8-05 | sar-web loads the run as a replay, has drones and casualties at its end, and the presenter page(s) receive a test cue | FAIL if sar-web cannot replay; WARN with no presenter page open |
| P8-06 | every enabled recorder connects, has its scenes, and makes a 4-second test recording | FAIL on any error |
| P8-07 | ffmpeg, and the text-to-speech program if set | WARN if missing |
| P8-08 | (`--rehearse`) the whole take played on the presenter page | FAIL if it stops |

---

## Recording the video

```bash
rubblesar up && rubblesar status                 # both machines up
rubblesar demo check                             # everything the takes need

# 1. The live take: a mission, 30-50 minutes. Keep the TAK device (WinTAK as VADER, or the ATAK phone)
#    on screen and play the team: when SAR-OPS sends it a casualty, reply "ok" within 3 minutes (or it
#    goes to another team), and later "done" (the casualty is then counted as freed by that team).
#    Every other online TAK team must answer too, or be closed.
rubblesar demo live

# 2. The frame for chapters 4-6, and the narration with this run's numbers.
rubblesar demo media
rubblesar demo script                            # read it; edit ~/.config/rubblesar/storyboard.toml if you like

# 3. The narrated take. Rehearse first and watch OBS's preview (or a browser at /ops/?present=1).
rubblesar demo replay --rehearse
rubblesar demo replay                            # recorded; repeat it as often as you like
rubblesar demo replay --chapter radio,tak        # retake only some chapters (a separate take)

# 4. The rough cut. The ATAK phone's recording is already on the ZBook (scrcpy runs here); copy a
#    WinTAK recording of the live take to the ZBook first and name it with --clip.
rubblesar demo voice                             # optional, needs [demo] tts
rubblesar demo cut                               # ATAK only
rubblesar demo cut --clip wintak=~/Videos/wintak-live.mkv   # with WinTAK
rubblesar demo takes                             # what you have
```

**During the live take** the director marks the beats as they happen (survey, first casualty, first close look and hover, first team and TAK team, first on scene, first freed, survey done, the link cut, the blackout, heard again, queue drained) and fires the staged link events of `demo.toml`: scout-1's radio budget cut to 24 kbit/s for 60 s at `first-red+45s` (its reports shrink to small crops, then thermal only), and a 60 s blackout at `first-red+150s` (nothing sent; every report delivered afterwards). The OBS on the ZBook records the operator view all along, which gives you real-time footage of any moment.

**The narrated take** starts with a 3-second slate ("Take …") on the presenter page so you can line up recordings, then plays the chapters; OBS gets a chapter mark per chapter. Its folder (`~/rubblesar-takes/<time>-replay/`) has:

| File | What |
|---|---|
| `cues.json`, `plan.json` | exactly what was shown when (the rough cut reads them) |
| `narration.txt` | the voice-over, with the time each chapter starts and how long it lasts: read it at about the pace of the captions |
| `narration.srt` | the captions as subtitles, timed to the recording |
| `shotlist.md` | what goes where, including the inserts from the live take and where in its recording they are |
| `voice/NN-<chapter>.wav` | (`demo voice`) the spoken narration per chapter |
| `rough-cut.mp4` | (`demo cut`) the take's recording, the inserts over the TAK chapter (between the header and the caption bar), the voice |

### Editing

The rough cut is meant to be finished in a video editor (Kdenlive or Shotcut on Ubuntu, or DaVinci Resolve):
- Import `rough-cut.mp4`, or the take's own OBS recording plus the WinTAK and ATAK recordings of the live take (the shot list says which seconds).
- **Voice**: a text-to-speech voice is fine for a draft. For the real video, record your own voice-over from `narration.txt` (each chapter lasts at least as long as its narration at 150 words a minute) and replace the voice track.
- **Subtitles**: import `narration.srt`, or keep the burnt-in captions of the presenter page.
- Cut freely: each chapter is self-contained and starts with the map moving to its subject, so chapters can be shortened, reordered or dropped.

---

## The settings: `~/.config/rubblesar/demo.toml`

| Key | Default | What |
|---|---|---|
| `[demo] takes_dir` | `~/rubblesar-takes` | where takes are written |
| `[demo] storyboard` | your copy (`demo init`) | `""` = the built-in one |
| `[demo] web` | `http://127.0.0.1:8090` | sar-web on this ZBook (the director's commands are accepted from this computer only) |
| `[demo] edge_seed` | `1` | edge-eye's `seed` on the Jetson: needed to render a frame exactly |
| `[demo] slate_s` | `3` | seconds of slate before and after a take |
| `[demo] tts` | `""` | text-to-speech command; narration on stdin, `{out}` the WAV, e.g. `espeak-ng -v en-gb -s 155 -w {out}` (`sudo apt install espeak-ng`; robotic, fine for timing) or a Piper voice |
| `[[live.event]]` | budget cut, blackout | `at` (a beat ± seconds, or `+mm:ss`), `uav`, and `budget_kbps` + `for`, or `blackout` |
| `[[recorder]]` | zbook (on), wintak, atak (off) | `kind = "obs"`: `url`, `password`, `scene_live`, `scene_replay`; `kind = "command"`: `start` (with `{file}`), `file`; `takes` (live, replay); `enabled` |

---

## Known limitations and honesty notes

- **The narrated take is a replay** of a recorded run, sped up 2–12 times (the clock in the caption bar says how fast); the live take is real time. Nothing on screen is invented: maps, numbers, crops and chat are the run's. The camera frames are rendered again on the ZBook from the drone's recorded pose and seed; they are the frames the drone processed, and the panel says so.
- **Chapter 5's model runs on the ZBook** (ONNX Runtime), the same model the Jetson runs with TensorRT; the speed quoted is the Jetson's, from its telemetry.
- **Simulated teams are faster than real ones** (`time_scale`, `rescue_time_scale`): the after-action chapter quotes real-world estimates and says so.
- **A run recorded before this release** has no frame poses or crops: chapters 4–6 need a run recorded by 0.9.
- **OBS is driven over its WebSocket** with a password on the local network (plain, like the rest of the system, D10).
- The presenter page is designed for 1920 × 1080; other sizes work but are not checked.
- WinTAK's own screen cannot be driven by the director: what WinTAK shows during the live take is what you do on the laptop.

---

## Command reference

```bash
rubblesar demo init | check | obs-setup [--recorder NAME] | test-record [--seconds 5]
rubblesar demo live [--max-minutes N]
rubblesar demo media [--run RUN] [--frame ID] | facts [--run RUN] | script [--run RUN]
rubblesar demo replay [--run RUN] [--rehearse] [--chapter ID,ID]
rubblesar demo voice [--take TAKE]
rubblesar demo cut [--take TAKE] [--live TAKE] [--main FILE] [--clip NAME=FILE ...] [--out FILE] [--dry-run]
rubblesar demo takes
sar-check phase8 [--demo FILE] [--run RUN] [--seconds 4] [--rehearse]
```
Options for all `demo` commands: `--demo FILE` (default `~/.config/rubblesar/demo.toml`), `--config FILE` (`sar.toml`).
