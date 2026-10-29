# Phase 0 — Foundations: setup, build and acceptance

**Goal.** The ZBook receives telemetry from three simulated drones running on the Jetson at 1 Hz over Wi-Fi through Zenoh, can command each drone, and can throttle a drone's radio link. Phase 0 is accepted when `scripts/zbook/50-acceptance.sh` ends with `RESULT: PASS (8 passed ...)`.

**Time needed.** About 45 minutes the first time: 20 on the ZBook, 25 on the Jetson (most of it downloads and the Jetson build).

**What is installed.** Go 1.26.0, zenoh-c 1.9.0 (both machines), zenohd 1.9.0 (ZBook), the RubbleSAR programs, two systemd services (`zenohd` on the ZBook, `edge-eye` on the Jetson) and chrony time sync. Nothing uses Docker in this phase. Full details: [ENVIRONMENT.md](ENVIRONMENT.md).

---

## 0. Before you start

1. **Same network.** Put the ZBook and the Jetson on the same Wi-Fi network. Note the ZBook's address:
   ```bash
   hostname -I          # on the ZBook, e.g. 192.168.1.50
   ```
   A DHCP reservation or static address for the ZBook avoids re-running step J4 when it changes.
2. **Internet for the first install** on both machines (apt, Go, zenoh releases). If the Jetson has none, see [Offline Jetson](#offline-jetson).
3. **Copy the package** `rubblesar-0.1.0-phase0.tar.gz` to both machines and unpack it in your home directory:
   ```bash
   # on the ZBook (where you downloaded it)
   tar -xzf rubblesar-0.1.0-phase0.tar.gz && cd rubblesar-0.1.0-phase0
   # copy to the Jetson (replace user and address)
   scp rubblesar-0.1.0-phase0.tar.gz jetson-user@<jetson-ip>:~
   # on the Jetson
   tar -xzf rubblesar-0.1.0-phase0.tar.gz && cd rubblesar-0.1.0-phase0
   ```
4. Run every script **from the unpacked folder, as your normal user** (not with `sudo`); scripts ask for your password when they need it.

Every script ends with `DONE: ... completed successfully.` and tells you the next step. If one stops, it prints a block between
`=============== COPY FROM HERE` and `================ COPY TO HERE` — paste that block into the chat.

---

## 1. ZBook

| Step | Command | What it does | Expect |
|---|---|---|---|
| Z1 | `scripts/zbook/00-prereqs.sh` | apt packages, checks Go 1.26.0 (installs it only if missing), chrony | `DONE`; the doctor table at the end marks what later steps install as `TODO` |
| Z2 | `scripts/zbook/10-zenoh-c.sh` | zenoh-c 1.9.0 library into `/usr/local`, checksum-verified | `ok: pkg-config zenohc 1.9.0` |
| Z3 | `scripts/zbook/15-zenohd.sh` | zenoh router as service `zenohd`, port 7447/tcp (admin API on 127.0.0.1:8447), firewall rule if ufw is active | `ok: listening on 0.0.0.0:7447` and a list of this machine's addresses |
| Z4 | `scripts/zbook/20-build.sh` | builds the programs, runs unit tests, zenoh tests and the loopback self-test | `RESULT: PASS (3 passed ...)` for loopback |
| Z5 | `scripts/zbook/30-install.sh` | installs `sar-check`, `sar-monitor`, `sar-doctor` to `/opt/rubblesar/bin`, config `/etc/rubblesar/sar.toml`, serves time to the Jetson | `DONE` |
| Z6 | `scripts/zbook/40-test.sh` | doctor, loopback, routing through zenohd, and the **full phase 0 suite with simulated drones** (no Jetson needed) | every section `ok:` and `RESULT: PASS (8 passed ...)` |

After Z6 the ZBook side is proven on its own. If Z6 passes but the acceptance test later fails, the problem is the Jetson or the Wi-Fi link.

---

## 2. Jetson

| Step | Command | What it does | Expect |
|---|---|---|---|
| J1 | `scripts/jetson/00-prereqs.sh` | checks JetPack 6.2.x, apt packages, Go 1.26.0 (native, `/usr/local/go`), chrony, adds you to the `video` group | `DONE` (later items shown as `TODO`); then **open a new terminal** so Go is on your PATH |
| J2 | `scripts/jetson/10-zenoh-c.sh` | zenoh-c 1.9.0 for aarch64 into `/usr/local` | `ok: pkg-config zenohc 1.9.0` |
| J3 | `scripts/jetson/20-build.sh` | native build, unit, zenoh and loopback tests (about 5 minutes) | `RESULT: PASS (3 passed ...)` |
| J4 | `scripts/jetson/30-install.sh --zbook-ip 192.168.1.50` | installs `edge-eye` as a systemd service pointed at the ZBook, time sync from the ZBook | `ok: edge-eye is running and connected` and a table of three drones |
| J5 | `scripts/jetson/40-test.sh` | doctor, loopback, messages through the ZBook's router, service health | every section `ok:` |

Optional, not needed for phase 0: `scripts/jetson/cam-test.sh` grabs one JPEG from each CSI camera (sensors 0 and 1) into `reports/camera/`.

---

## 3. Acceptance (on the ZBook)

```bash
scripts/zbook/50-acceptance.sh
```

It lists the zenohd sessions (the Jetson appears as a `client`), shows 10 seconds of live telemetry, then runs `sar-check phase0`:

| Check | Proves | Pass condition |
|---|---|---|
| CHK-01 Connect to zenoh router | the ZBook tools reach zenohd | session opens within 10 s |
| CHK-02 Discover expected drones | Jetson → ZBook over Wi-Fi works | `scout-1`, `inv-1`, `inv-2` heard within 20 s |
| CHK-03 Telemetry rate | edge-eye publishes on schedule, nothing throttles it | each drone 1.0 Hz ± 10% over 30 s |
| CHK-04 Message integrity | no corruption, loss or version mismatch | 0 decode errors, ≤ 1% loss, no reordering |
| CHK-05 Telemetry plausibility | simulation and geodesy are sane | within 1.5 × 805 m of Antakya centre, battery and altitude in range |
| CHK-06 Clock offset | clocks agree (needed for latency and ordering later) | median receive-minus-send ≤ 250 ms (warning up to 2 s) |
| CHK-07 Control round trip | ZBook → Jetson commands work | 15 of 15 pings answered within 2 s |
| CHK-08 Link throttle | bandwidth can be throttled per drone | `scout-1` falls to about 0.33 Hz at 400 bit/s and is restored |

Expected ending:

```
RESULT: PASS  (8 passed, 0 warnings, 0 failed, 0 skipped)
Report saved: reports/phase0-<zbook>-<time>.json
DONE: scripts/zbook/50-acceptance.sh completed successfully.
```

Send that RESULT line in the chat to move on to phase 1.

---

## 4. Everyday commands

| Where | Command | Purpose |
|---|---|---|
| ZBook | `sar-monitor` | live table of every drone (Ctrl+C to stop) |
| ZBook | `sar-monitor --once 10s` | one snapshot, exit code 1 if nothing heard |
| ZBook | `sar-check phase0` | re-run the acceptance test |
| ZBook | `systemctl status zenohd` · `journalctl -u zenohd -f` | router health and log |
| ZBook | `curl -s 'http://127.0.0.1:8447/@/*/router'` | who is connected to zenohd |
| Jetson | `edge-eye status` | drones, link budget, sent and dropped messages |
| Jetson | `edge-eye budget --kbps 64` | throttle all drones to 64 kbit/s now |
| Jetson | `edge-eye budget --kbps 32 --uav inv-1` | throttle one drone |
| Jetson | `edge-eye budget --kbps 0` | remove the limit |
| Jetson | `sudo systemctl restart edge-eye` · `journalctl -u edge-eye -f` | restart, follow the log |
| Either | `sar-doctor` | environment check with hints |
| Either | `scripts/collect-diagnostics.sh` | one block with versions, services, logs and doctor results to paste |

Budget changes made with `edge-eye budget` last until edge-eye restarts; the permanent value is `default_budget_kbps` in `/etc/rubblesar/edge-eye.toml`.

---

## 5. When something fails

1. Copy the block between `COPY FROM HERE` and `COPY TO HERE` and paste it into the chat. Every block names the script or check, the error code, the command and the last log lines.
2. If no block was printed (for example a service misbehaving later), run `scripts/collect-diagnostics.sh` on that machine and paste its block.
3. Error codes and first fixes are listed in [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

The most likely first-time problems:

| Symptom | Code | First thing to try |
|---|---|---|
| J4 waits 30 s, then fails | E-SETUP-SVC with E-BUS-010 in the log | Wrong `--zbook-ip`, zenohd not running (`systemctl status zenohd` on the ZBook), or the ZBook firewall blocks 7447/tcp |
| CHK-02 fails on the ZBook | E-CHK-002 | On the Jetson: `systemctl status edge-eye`, `edge-eye status` |
| CHK-06 warns | E-CHK-006 | Time sync not settled: `chronyc sources` on the Jetson should show the ZBook with `*`; wait a minute and re-run |
| `go: command not found` after J1 | — | Open a new terminal (or `export PATH=$PATH:/usr/local/go/bin`) |
| Download fails | E-SETUP-NET | No internet: use [Offline Jetson](#offline-jetson) |

---

## Offline Jetson

On a machine with internet (the ZBook), inside the unpacked folder:

```bash
scripts/fetch-downloads.sh
```

It downloads Go 1.26.0 (amd64 and arm64), zenoh-c 1.9.0 (both architectures) and zenohd into `downloads/`, verifying checksums. Copy the whole folder (with `downloads/`) to the Jetson; the setup scripts use those files instead of downloading. apt packages still need a network or a local mirror; JetPack already includes most of them.

---

## Uninstall

```bash
scripts/uninstall.sh            # services and binaries; keeps /etc/rubblesar and zenoh-c
scripts/uninstall.sh --purge    # everything RubbleSAR installed except Go
```
