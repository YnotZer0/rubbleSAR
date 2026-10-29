# Phase 2 — The body-part model

**Goal.** A segmentation model that outlines hands, arms, legs, heads and torsos sticking out of rubble in the simulated drone imagery. It is trained on the ZBook GPU and runs on the Jetson through TensorRT, called from Go. Both machines measure it on the same held-out frames, so you get numbers rather than impressions: how much it finds, how often it is wrong, whether the Jetson agrees with the ZBook, and how fast the Jetson is. Phase 3 builds the edge pipeline on top of it.

**Accepted when** `scripts/zbook/90-phase2-test.sh` **and** `scripts/jetson/90-phase2-test.sh` both end with `RESULT: PASS` (warnings allowed) and `PHASE 2 checks passed`, and you have agreed with the accuracy and speed numbers they print.

**Time needed.**
- ZBook: about 30 minutes of commands, plus waiting. The first image build is about 10 minutes and an 8 GB download. The dataset takes 15–25 minutes, training 1–3 hours, evaluation 5–10 minutes.
- Jetson: about 15 minutes. Building the engine takes 5–15 minutes.

**Disk.**
- ZBook: about 15 GB (PyTorch image 8–9 GB, dataset 1–2 GB, training runs 0.5 GB each).
- Jetson: about 200 MB.

> **Everything here is simulated.** The model has only seen procedurally drawn people in procedurally drawn rubble. It is not a validated casualty detector and must not be used on real imagery for decisions.

---

## What this phase adds

| Piece | What it does |
|---|---|
| `scenario-tool dataset` | Builds the training set. It makes 36 new incidents (30 for training, 6 for validation), varying the seeds, sites, victims, time of day, weather and the share of living victims. It photographs each one from 80 random drone poses: investigators at 10–18 m, the scout at 30–50 m, nadir and oblique, with noise and blur. Each 1080p frame is cut into 640 px tiles, and every body part is written as an exact outline in YOLO format. The installed `antakya-demo` scenario is **never** used for training: it is the held-out test (71 fixed frames). |
| `training/` (Docker, ZBook GPU) | Fine-tunes Ultralytics YOLO11-seg (pinned 8.3.253, starting weights checksum-verified). It then exports ONNX (opset 17, dynamic batch and size) and writes the model card `model.json` with the metrics. |
| `sar-infer` | Runs the model from Go: tiling, batching, decoding the network outputs, class-aware NMS, masks, and merging across tiles. Back ends: **TensorRT** (Jetson, FP16) and **ONNX Runtime 1.22** (both machines, CPU). Commands: `info`, `bench`, `frame`, `eval`, `package`, `golden`, `parity`, `stub-model`. |
| golden set | Twelve reference tiles with ONNX Runtime results, made on the ZBook. The Jetson must reproduce them with TensorRT. Ultralytics' own predictions on the same tiles prove that the Go decoding matches Python. |
| `sar-check phase2` | The acceptance suite (below). |
| `sar-doctor` | New checks: DOC-10 (ONNX Runtime) and DOC-11 (model installed). DOC-J3 now also checks the TensorRT headers and `trtexec`, and DOC-Z2 the NVIDIA Container Toolkit. |

The phase 0 and 1 programs are unchanged.

---

## Before you start

### ZBook: GPU inside Docker

Training runs in Docker with `--gpus all`, so Docker needs the NVIDIA Container Toolkit. Test it:
```bash
docker run --rm --gpus all ubuntu nvidia-smi
```
If that prints your Quadro RTX 4000, go on. If it says `could not select device driver "" with capabilities: [[gpu]]`, install the toolkit (NVIDIA's apt repository):
```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
docker run --rm --gpus all ubuntu nvidia-smi
```
To run docker without `sudo`, run `sudo usermod -aG docker $USER`, then log out and back in. The scripts fall back to `sudo docker` otherwise.

The training image is based on `pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime`, which needs NVIDIA driver 570 or newer (`nvidia-smi` shows yours). With an older driver, the script uses the CUDA 12.4 image instead and says so.

### Jetson: power mode

Speed is part of the acceptance. Use the highest power mode. On an Orin Nano with JetPack 6.2 this is *MAXN SUPER*:
```bash
sudo nvpmodel -p --verbose | grep POWER_MODEL      # lists the modes and their numbers
sudo nvpmodel -m 2                                   # MAXN SUPER on the Orin Nano (check the number above)
sudo jetson_clocks
```

---

## Steps

Unpack the release **next to** the phase 1 folder on both machines. Your `/etc/rubblesar` configs and the installed scenario are kept.
```bash
tar -xzf rubblesar-0.3.0-phase2.tar.gz && cd rubblesar-0.3.0-phase2
```
Run every script from that folder as your normal user.

### ZBook

| Step | Command | What it does | Expect |
|---|---|---|---|
| P2-Z1 | `scripts/zbook/12-onnxruntime.sh` | installs ONNX Runtime 1.22.0 (CPU) into `/opt/onnxruntime-1.22.0` | `DONE` |
| P2-Z2 | `scripts/zbook/20-build.sh` | rebuilds with ONNX Runtime and runs all tests, including the new inference and training-script tests | `DONE` |
| P2-Z3 | `scripts/zbook/30-install.sh` | installs `sar-infer` | `DONE` |
| P2-Z4 | `scripts/zbook/80-dataset.sh --quick` | a small dataset (2 + 2 incidents) to test the chain, about 3–5 minutes | `DONE` and the next command |
| P2-Z5 | `scripts/zbook/81-train.sh --quick` (uses `data/datasets/rubblesar-v1-quick`) | builds the training image (first time: about 10 min), checks the GPU in Docker, trains 3 epochs | `DONE` and a `mask mAP50` line |
| P2-Z6 | the `82-evaluate.sh … --quick` command printed by P2-Z5 | packages, golden set, Ultralytics comparison, evaluation | `DONE` |
| P2-Z7 | `scripts/zbook/80-dataset.sh` | the full dataset (15–25 minutes; continues where it stopped if interrupted) | `DONE`; look at `data/datasets/rubblesar-v1/preview.png` |
| P2-Z8 | `tmux new -s train`, then `scripts/zbook/81-train.sh` | full training (up to 80 epochs; stops early when it stops improving): **1–3 hours**. Detach with Ctrl-b d; reattach with `tmux attach -t train` | `DONE` and the val/test metrics |
| P2-Z9 | `scripts/zbook/82-evaluate.sh` | turns the newest run into the model folder and installs it as the current model | `DONE` |
| P2-Z10 | `scripts/zbook/90-phase2-test.sh` | acceptance suite and frame speed | `RESULT: PASS`, `PHASE 2 checks passed on the zbook` |
| P2-Z11 | `scripts/zbook/85-push-model.sh --jetson <user>@<jetson-ip>` | copies the model folder to the Jetson over SSH | `ok: copied` |

The quick run (P2-Z4 to P2-Z6) only proves that every step works on your machine before you spend hours on training. Its model is barely trained, so do not run the acceptance test on it: P2-04 would fail. If any quick step fails, send me its block and we fix it before the long run.

### Jetson

| Step | Command | What it does | Expect |
|---|---|---|---|
| P2-J1 | `scripts/jetson/12-onnxruntime.sh` | ONNX Runtime 1.22.0 (CPU, the fallback back end) | `DONE` |
| P2-J2 | `scripts/jetson/75-tensorrt.sh` | makes sure the TensorRT headers, `trtexec` and the CUDA headers from JetPack are installed; shows the power mode | `DONE` |
| P2-J3 | `scripts/jetson/20-build.sh` | native rebuild **with TensorRT** and all tests | `DONE` |
| P2-J4 | `scripts/jetson/30-install.sh --zbook-ip <ZBook IP>` | installs `sar-infer`, restarts edge-eye | `ok: edge-eye is running and connected` |
| P2-J5 | `scripts/jetson/80-engine.sh --stub` | **can run while the ZBook trains.** Builds a TensorRT engine from a stand-in network and runs whole frames through it from Go. This proves TensorRT works here before the real model exists | `TensorRT works on this Jetson` |
| P2-J6 | `scripts/jetson/80-engine.sh --from ~/rubblesar-data/models/<name>` | after P2-Z11: verifies the copy, builds the FP16 engine (5–15 min), compares it with the ZBook's golden results, installs it | `DONE` |
| P2-J7 | `scripts/jetson/90-phase2-test.sh` | acceptance suite and frame speed | `RESULT: PASS`, `PHASE 2 checks passed on the jetson` |

Without SSH between the machines, copy `/var/lib/rubblesar/models/<name>` from the ZBook to a USB stick and pass that folder to `--from`.

An engine only works with the TensorRT version and GPU it was built on. It is therefore always built on the Jetson and never copied.

### What to send back

Both `90-phase2-test.sh` runs print a `COPY FROM HERE … COPY TO HERE` block **even when everything passes**, because the numbers are what we need to agree on. P2-04 in that block lists accuracy per class and per view. Send both blocks, the `median per frame` line printed after each, and the two metric lines (`val`, `test`) from the end of `81-train.sh`. If any other step fails, send its block.

---

## What the acceptance suite proves

| Check | ZBook | Jetson |
|---|---|---|
| P2-01 | **model folder**: card valid (5 classes, 640 px), `model.onnx` matches the checksum in the card, golden set and test-frame list present | **model folder and engine**: same, plus the engine was built from this exact ONNX file (`engine.json`) and the ZBook evaluation is there |
| P2-02 | **ONNX Runtime runs the model**: output shapes as expected, time per tile | **TensorRT runs the engine**: FP16, batch up to 6, time per tile |
| P2-03 | **Go decoding = Ultralytics**: ≥ 90 % of Ultralytics' own detections on the golden tiles are found by the Go code (same class, box overlap ≥ 0.85 median) | **TensorRT = ZBook reference**: ≥ 95 % of the golden detections reproduced, median box overlap ≥ 0.9, confidence within 0.1 |
| P2-04 | **accuracy** on the 71 held-out frames of the installed scenario (it first confirms the frames belong to it; below). FAIL if overall lenient recall < 0.30 or investigator-nadir lenient recall < 0.50. WARN if investigator-nadir strict recall < 0.70 or lenient precision < 0.50 | the same evaluation with TensorRT; WARN if AP50 or recall differ from the ZBook's by more than 0.05 |
| P2-05 | **victims found after geolocation**: detections placed on the map with the drone's reported pose and scored like phase 1 (2 m). WARN only (see *Flat ground*) | **inference speed**: one 1080p frame (6 tiles) end to end. PASS ≤ 250 ms, WARN ≤ 600 ms |

**How accuracy is counted.**
- A detection matches a labelled body part when their masks overlap by IoU ≥ 0.5 (**strict**). The **lenient** score also accepts IoU ≥ 0.25 or a centre within 4 px. Lenient is the fairer score for slivers a few pixels wide, such as a hand behind a slab.
- Parts smaller than 6 px are not expected; detecting them is not counted against the model.
- Results are given per class and per view: investigator nadir (13 m), investigator oblique (15 m, pitch −70°, two hours later), scout nadir (40 m) and scout oblique (pitch −60°).
- *Mask AP50* is the usual detection average precision at IoU 0.5. *False alarms per frame* counts detections that match nothing.

The thresholds are a starting point for us to agree on. They are deliberately modest for the small scout views, where a hand is 10–20 pixels across.

---

## Choosing the model size

| | `yolo11s-seg` (default) | `yolo11n-seg` |
|---|---|---|
| Size | about 10 M parameters | about 3 M parameters |
| Accuracy | better, especially on small parts | lower |
| Jetson speed | slower; usually fits the 250 ms target in MAXN SUPER | about 2–3× faster |

If P2-05 on the Jetson warns or fails, train the small model with `scripts/zbook/81-train.sh --model yolo11n-seg`, then repeat P2-Z9 onwards. Both can be installed; `current` points at the one in use.

## Flat ground (known limitation)

Phase 2 geolocates a detection by intersecting the camera ray with flat ground at street level. A hand on top of a 3 m rubble pile, seen from an angle, therefore lands a metre or two away from where it is. That is why P2-05 only warns: the ZBook prints the result with both the reported and the true drone pose, so you can see how much comes from GPS noise and how much from the flat-ground assumption. Phase 3 adds height-aware geolocation and tracking over several frames.

---

## sar-infer reference

```
sar-infer info     --model DIR|FILE
sar-infer bench    --model DIR|FILE [--frames 20] [--scenario DIR]
sar-infer frame    --model DIR|FILE --out DIR [--victim vNN | --e M --n M] [--alt 40] [--yaw 0] [--pitch -90] [--t S]
sar-infer eval     --model DIR [--scenario DIR] [--json FILE] [--overlays DIR] [--quick]
sar-infer package  --export DIR --dataset DIR --out DIR      (ZBook)
sar-infer golden   --model DIR --dataset DIR [--tiles 12]     (ZBook)
sar-infer parity   --model DIR|FILE [--golden DIR]
sar-infer stub-model --out DIR
```
`--model` defaults to `/var/lib/rubblesar/models/current`. A folder uses `model.engine` when the build has TensorRT, otherwise `model.onnx`. Exit codes: 0 success, 1 a comparison failed, 2 could not run.

To see what the model does on any view:
```bash
sar-infer frame --out /tmp/d1 --victim v03 --alt 15
sar-infer frame --out /tmp/d2 --victim v10 --alt 40 --pitch -60 --yaw 120
xdg-open /tmp/d1/detections.png       # outlines and labels; detections.json has the latitude/longitude of each
```
Evaluation overlays (white: labelled parts; colour: model) are in `data/overlays/<name>/` on the ZBook.

## Files and formats

```
data/datasets/<name>/                 (ZBook) the training set
  data.yaml                           Ultralytics dataset file (train / val / test, 5 classes)
  images/{train,val,test}/*.jpg       640 px tiles
  labels/{train,val}/*.txt            one line per body part: class x1 y1 x2 y2 … (outline, 0–1)
  test/frames.json                    the 71 held-out frames as poses; re-rendered from the scenario when needed
  preview.png                         36 labelled examples; check that outlines sit on the body parts
  manifest.json                       generator version, seeds, counts per split and class
data/runs/<name>/                     (ZBook) Ultralytics run: results.png, confusion matrix, val_batch*_pred.jpg
  export/{model.onnx,model.json,best.pt}

/var/lib/rubblesar/models/<name>/     (both) an installed model; 'current' points at the one in use
  model.onnx        the network: input images [B,3,640,640]; outputs output0 [B,41,8400], output1 [B,32,160,160]
  model.json        model card: classes, input size, dataset, epochs, GPU, training time, metrics, ONNX SHA-256
  testset.json      the held-out frame list
  golden/           12 reference tiles (PNG), golden.json (ONNX Runtime results), ultralytics.json
  eval-zbook.json   the ZBook evaluation (the Jetson compares with it)
  model.engine      Jetson only: TensorRT FP16 engine, batch 1–6
  engine.json       Jetson only: TensorRT version, precision, batch, device, source ONNX SHA-256
```
Classes: 0 hand, 1 arm, 2 leg, 3 head, 4 torso.

**Licence note.** Ultralytics YOLO11 is AGPL-3.0. That is fine for in-house use, as agreed (D2). Distributing the trained model or the programs outside your organisation would need an Ultralytics licence or a different network.
