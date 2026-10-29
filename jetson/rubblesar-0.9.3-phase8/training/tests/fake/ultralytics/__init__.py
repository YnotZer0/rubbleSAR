"""Stand-in for ultralytics: records calls and writes plausible files."""
import json
from pathlib import Path

import torch

__version__ = "8.3.253-fake"
CALLS = []


class _Obj:
    def __init__(self, base):
        self.map50, self.map, self.mp, self.mr = base, base / 2, base + 0.05, base - 0.05
        self.ap50 = [base] * 5
        self.p = [base + 0.05] * 5
        self.r = [base - 0.05] * 5


class _Metrics:
    def __init__(self):
        self.seg, self.box = _Obj(0.6), _Obj(0.7)
        self.ap_class_index = [0, 1, 2, 3, 4]


class _Trainer:
    pass


class YOLO:
    def __init__(self, weights):
        self.weights = weights
        CALLS.append(("init", weights))

    def train(self, **kw):
        CALLS.append(("train", kw))
        if kw["batch"] in torch.cuda.oom_batches:
            raise torch.OutOfMemoryError("CUDA out of memory. Tried to allocate 2.00 GiB")
        d = Path(kw["project"]) / kw["name"] / "weights"
        d.mkdir(parents=True, exist_ok=True)
        (d / "best.pt").write_bytes(b"fake-best")
        self.trainer = _Trainer()
        self.trainer.save_dir = str(d.parent)
        self.trainer.best = str(d / "best.pt")
        self.trainer.epoch = kw["epochs"] - 1

    def val(self, **kw):
        CALLS.append(("val", kw))
        return _Metrics()

    def export(self, **kw):
        CALLS.append(("export", kw))
        p = Path(self.weights).with_suffix(".onnx")
        p.write_bytes(b"fake-onnx")
        return str(p)

    def predict(self, path, **kw):
        CALLS.append(("predict", kw))
        return [type("R", (), {"boxes": None, "masks": None})()]
