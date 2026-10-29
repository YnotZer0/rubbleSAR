#!/usr/bin/env python3
"""RubbleSAR body-part model training (runs inside the rubblesar-train Docker image).

Commands:
  check                         GPU and library versions
  train   --data D --weights W --out O --name N [--epochs E] [--batch B] [--quick]
  predict --model M --images DIR --out FILE      Ultralytics predictions for parity checks

Every failure prints a framed block (COPY FROM HERE ... COPY TO HERE) with an
error code, the versions and the end of the traceback, like the Go tools.
"""

import argparse
import datetime as dt
import hashlib
import json
import os
import platform
import shutil
import sys
import time
import traceback
from pathlib import Path

CLASSES = ["hand", "arm", "leg", "head", "torso"]
VERSION = "0.3.0"

HINTS = {
    "E-TRN-001": "Docker cannot see the GPU. On the ZBook: 'docker run --rm --gpus all ubuntu nvidia-smi' must list the Quadro. "
    "If it does not, install or reconfigure the NVIDIA Container Toolkit (docs/PHASE2.md, GPU in Docker).",
    "E-TRN-002": "The dataset is missing or incomplete; run scripts/zbook/80-dataset.sh first.",
    "E-TRN-003": "Training stopped with an error; the traceback below shows where. Paste this block into the chat.",
    "E-TRN-004": "The GPU ran out of memory even with a small batch; close other GPU programs (nvidia-smi) and retry.",
    "E-TRN-005": "Exporting the trained model to ONNX failed; paste this block into the chat.",
    "E-TRN-006": "The starting weights are missing or damaged; rebuild the image with scripts/zbook/81-train.sh --rebuild.",
    "E-TRN-007": "Prediction for the parity check failed; paste this block into the chat.",
}


class TrainError(Exception):
    def __init__(self, code, msg):
        super().__init__(msg)
        self.code = code


def versions():
    out = {"rubblesar_train": VERSION, "python": platform.python_version()}
    try:
        import torch

        out["torch"] = torch.__version__
        out["cuda"] = str(torch.version.cuda)
        out["gpu"] = torch.cuda.get_device_name(0) if torch.cuda.is_available() else "none"
    except Exception as e:  # pragma: no cover - reported, not fatal
        out["torch"] = f"unavailable ({e})"
    try:
        import ultralytics

        out["ultralytics"] = ultralytics.__version__
    except Exception as e:
        out["ultralytics"] = f"unavailable ({e})"
    return out


def copy_block(code, msg, tb=""):
    lines = [
        "",
        "=============== COPY FROM HERE: RubbleSAR training error ===============",
        f"tool:    rubblesar_train.py {VERSION} ({' '.join(sys.argv[1:])})",
        f"time:    {dt.datetime.now(dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')}",
    ]
    for k, v in versions().items():
        lines.append(f"{k + ':':9}{v}")
    lines.append(f"error:   {code}: {msg}")
    lines.append(f"hint:    {HINTS.get(code, '')}")
    if tb:
        lines.append("--- traceback (last 30 lines) ---")
        lines.extend(tb.rstrip().splitlines()[-30:])
    lines.append("================ COPY TO HERE =========================================")
    lines.append("Paste everything between the two lines above into the chat to get help.")
    print("\n".join(lines), file=sys.stderr, flush=True)


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def chown_tree(path):
    """Give results to the host user (the container runs as root)."""
    uid, gid = os.environ.get("HOST_UID"), os.environ.get("HOST_GID")
    if not uid or not gid:
        return
    for root, dirs, files in os.walk(path):
        for n in dirs + files:
            try:
                os.chown(os.path.join(root, n), int(uid), int(gid), follow_symlinks=False)
            except OSError:
                pass
    try:
        os.chown(path, int(uid), int(gid))
    except OSError:
        pass


def need_gpu():
    import torch

    if not torch.cuda.is_available():
        raise TrainError("E-TRN-001", "torch.cuda.is_available() is False inside the container")
    return torch.cuda.get_device_name(0)


def check_dataset(data):
    p = Path(data)
    if not p.is_file():
        raise TrainError("E-TRN-002", f"{data} not found")
    root = p.parent
    for split in ("train", "val"):
        imgs = list((root / "images" / split).glob("*.jpg"))
        if not imgs:
            raise TrainError("E-TRN-002", f"no images in {root / 'images' / split}")
    names = []
    for line in p.read_text().splitlines():
        s = line.strip()
        if ":" in s and s.split(":", 1)[0].strip().isdigit():
            names.append(s.split(":", 1)[1].strip())
    if names != CLASSES:
        raise TrainError("E-TRN-002", f"{data} lists classes {names}, expected {CLASSES}")
    return root


def metric_dict(m, split, out):
    """Flatten Ultralytics segmentation metrics into model.json keys."""
    for kind, obj in (("mask", m.seg), ("box", m.box)):
        out[f"{split}.all.{kind}.map50"] = round(float(obj.map50), 4)
        out[f"{split}.all.{kind}.map50_95"] = round(float(obj.map), 4)
        out[f"{split}.all.{kind}.p"] = round(float(obj.mp), 4)
        out[f"{split}.all.{kind}.r"] = round(float(obj.mr), 4)
        for i, c in enumerate(list(m.ap_class_index)):
            name = CLASSES[int(c)] if int(c) < len(CLASSES) else str(c)
            out[f"{split}.{name}.{kind}.map50"] = round(float(obj.ap50[i]), 4)
            out[f"{split}.{name}.{kind}.p"] = round(float(obj.p[i]), 4)
            out[f"{split}.{name}.{kind}.r"] = round(float(obj.r[i]), 4)


def is_oom(e):
    s = f"{type(e).__name__}: {e}".lower()
    return "out of memory" in s or "outofmemory" in s


def cmd_check(args):
    v = versions()
    print(json.dumps(v, indent=2))
    need_gpu()
    print("ok: GPU visible inside the container")


def cmd_train(args):
    from ultralytics import YOLO

    gpu = need_gpu()
    root = check_dataset(args.data)
    if not Path(args.weights).is_file():
        raise TrainError("E-TRN-006", f"{args.weights} not found")
    out = Path(args.out) / args.name
    epochs, fraction = args.epochs, 1.0
    if args.quick:
        epochs, fraction = 3, 0.1
    batch = args.batch
    t0 = time.time()
    print(f"training {Path(args.weights).stem} on {gpu}: {epochs} epochs, batch {batch}, data {args.data}", flush=True)
    run_dir = None
    while True:
        model = YOLO(args.weights)
        try:
            model.train(
                data=args.data, epochs=epochs, imgsz=args.imgsz, batch=batch, workers=args.workers, device=0,
                project=str(args.out), name=args.name, exist_ok=True, seed=args.seed, patience=args.patience,
                fraction=fraction, flipud=0.5, fliplr=0.5, scale=0.3, translate=0.1, mosaic=1.0, close_mosaic=10,
                hsv_h=0.015, hsv_s=0.5, hsv_v=0.3, plots=True, verbose=True,
            )
            run_dir = Path(model.trainer.save_dir)
            best = Path(model.trainer.best)
            epochs_run = int(getattr(model.trainer, "epoch", epochs - 1)) + 1
            break
        except Exception as e:  # retry with a smaller batch on GPU memory exhaustion
            if is_oom(e) and batch > 4:
                batch //= 2
                print(f"GPU out of memory: retrying with batch {batch}", flush=True)
                import torch

                torch.cuda.empty_cache()
                continue
            if is_oom(e):
                raise TrainError("E-TRN-004", f"out of GPU memory at batch {batch}") from e
            raise TrainError("E-TRN-003", f"{type(e).__name__}: {e}") from e
    minutes = (time.time() - t0) / 60
    if not best.is_file():
        best = run_dir / "weights" / "last.pt"
    print(f"training finished in {minutes:.1f} min; best weights {best}", flush=True)

    metrics = {}
    trained = YOLO(str(best))
    for split in ("val", "test"):
        if split == "test" and not list((root / "images" / "test").glob("*.jpg")):
            continue
        try:
            m = trained.val(data=args.data, split=split, imgsz=args.imgsz, batch=max(1, batch), device=0, plots=False, verbose=False)
            metric_dict(m, split, metrics)
        except Exception as e:
            raise TrainError("E-TRN-003", f"validation on {split} failed: {type(e).__name__}: {e}") from e

    export_dir = out / "export"
    export_dir.mkdir(parents=True, exist_ok=True)
    try:
        onnx = Path(trained.export(format="onnx", imgsz=args.imgsz, dynamic=True, simplify=True, opset=17, half=False, device="cpu"))
    except Exception as e:
        raise TrainError("E-TRN-005", f"{type(e).__name__}: {e}") from e
    target = export_dir / "model.onnx"
    shutil.copyfile(onnx, target)
    shutil.copyfile(best, export_dir / "best.pt")
    v = versions()
    manifest = {}
    mp = root / "manifest.json"
    if mp.is_file():
        try:
            manifest = json.loads(mp.read_text())
        except ValueError:
            manifest = {}
    card = {
        "schema": 1,
        "name": args.name,
        "arch": Path(args.weights).stem,
        "classes": CLASSES,
        "input_size": args.imgsz,
        "onnx": "model.onnx",
        "onnx_sha256": sha256(target),
        "dataset": f"{manifest.get('name', root.name)} ({root})",
        "epochs": epochs_run,
        "ultralytics_version": v.get("ultralytics", ""),
        "torch_version": v.get("torch", ""),
        "gpu": gpu,
        "trained_at": dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "train_minutes": round(minutes, 1),
        "metrics": metrics,
        "notes": "quick smoke-test training (3 epochs, 10% of the data)" if args.quick else "",
    }
    (export_dir / "model.json").write_text(json.dumps(card, indent=2) + "\n")
    print(json.dumps({k: metrics[k] for k in sorted(metrics) if ".all." in k}, indent=2))
    print(f"wrote {export_dir}/model.onnx and model.json", flush=True)
    chown_tree(out)


def cmd_predict(args):
    from ultralytics import YOLO

    model = YOLO(args.model)
    images = sorted(Path(args.images).glob("*.png"))
    if not images:
        raise TrainError("E-TRN-007", f"no PNG tiles in {args.images}")
    out = {"schema": 1, "model": args.model, "conf": args.conf, "iou": args.iou, "tiles": []}
    for p in images:
        r = model.predict(str(p), imgsz=args.imgsz, conf=args.conf, iou=args.iou, retina_masks=True, verbose=False, device=args.device)[0]
        dets = []
        if r.boxes is not None and len(r.boxes):
            boxes = r.boxes.xyxy.cpu().numpy().tolist()
            confs = r.boxes.conf.cpu().numpy().tolist()
            clss = r.boxes.cls.cpu().numpy().astype(int).tolist()
            areas = [0] * len(boxes)
            cents = [(0.0, 0.0)] * len(boxes)
            if r.masks is not None:
                md = r.masks.data.cpu().numpy() > 0.5
                for i in range(min(len(boxes), md.shape[0])):
                    ys, xs = md[i].nonzero()
                    areas[i] = int(len(xs))
                    if len(xs):
                        cents[i] = (float(xs.mean() + 0.5), float(ys.mean() + 0.5))
            for b, c, k, a, ce in zip(boxes, confs, clss, areas, cents):
                dets.append({"class": k, "conf": round(c, 4), "box": [round(x, 2) for x in b], "mask_px": a, "cx": round(ce[0], 2), "cy": round(ce[1], 2)})
        out["tiles"].append({"file": p.name, "detections": dets})
    Path(args.out).write_text(json.dumps(out, indent=1) + "\n")
    print(f"wrote {args.out}: {sum(len(t['detections']) for t in out['tiles'])} detections on {len(images)} tiles")
    chown_tree(Path(args.out))


def main(argv=None):
    ap = argparse.ArgumentParser(prog="rubblesar_train.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("check")
    t = sub.add_parser("train")
    t.add_argument("--data", required=True)
    t.add_argument("--weights", required=True)
    t.add_argument("--out", required=True)
    t.add_argument("--name", required=True)
    t.add_argument("--epochs", type=int, default=100)
    t.add_argument("--batch", type=int, default=16)
    t.add_argument("--imgsz", type=int, default=640)
    t.add_argument("--workers", type=int, default=6)
    t.add_argument("--patience", type=int, default=30)
    t.add_argument("--seed", type=int, default=0)
    t.add_argument("--quick", action="store_true")
    p = sub.add_parser("predict")
    p.add_argument("--model", required=True)
    p.add_argument("--images", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--imgsz", type=int, default=640)
    p.add_argument("--conf", type=float, default=0.25)
    p.add_argument("--iou", type=float, default=0.6)
    p.add_argument("--device", default="0")
    args = ap.parse_args(argv)
    try:
        {"check": cmd_check, "train": cmd_train, "predict": cmd_predict}[args.cmd](args)
    except TrainError as e:
        copy_block(e.code, str(e), traceback.format_exc() if e.__cause__ else "")
        return 1
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return 130
    except Exception as e:  # unexpected: still give a pasteable block
        code = "E-TRN-007" if args.cmd == "predict" else "E-TRN-003"
        copy_block(code, f"{type(e).__name__}: {e}", traceback.format_exc())
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
