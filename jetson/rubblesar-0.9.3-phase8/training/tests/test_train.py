"""Tests the training script's own logic with stand-ins for torch and ultralytics.

Run: python3 training/tests/test_train.py   (no GPU, no internet, no Docker needed)
"""
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE / "fake"))
sys.path.insert(0, str(HERE.parent))

import rubblesar_train as rt  # noqa: E402
import torch  # noqa: E402  (the fake)
import ultralytics  # noqa: E402  (the fake)


def make_dataset(root: Path, names=rt.CLASSES):
    for split in ("train", "val", "test"):
        (root / "images" / split).mkdir(parents=True)
        (root / "labels" / split).mkdir(parents=True)
        (root / "images" / split / "a.jpg").write_bytes(b"x")
    y = "train: images/train\nval: images/val\ntest: images/test\nnames:\n" + "".join(f"  {i}: {n}\n" for i, n in enumerate(names))
    (root / "data.yaml").write_text(y)
    (root / "manifest.json").write_text(json.dumps({"name": "rubblesar-v1"}))
    return root / "data.yaml"


def run(argv):
    out, err = io.StringIO(), io.StringIO()
    with redirect_stdout(out), redirect_stderr(err):
        rc = rt.main(argv)
    return rc, out.getvalue(), err.getvalue()


class TrainScript(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        ultralytics.CALLS.clear()
        torch.cuda.available = True
        torch.cuda.oom_batches = set()
        self.weights = self.tmp / "yolo11s-seg.pt"
        self.weights.write_bytes(b"w")

    def test_train_writes_card_and_onnx(self):
        data = make_dataset(self.tmp / "ds")
        rc, out, err = run(["train", "--data", str(data), "--weights", str(self.weights), "--out", str(self.tmp / "runs"), "--name", "v1", "--epochs", "7"])
        self.assertEqual(rc, 0, err)
        exp = self.tmp / "runs" / "v1" / "export"
        card = json.loads((exp / "model.json").read_text())
        self.assertEqual(card["classes"], rt.CLASSES)
        self.assertEqual(card["onnx_sha256"], rt.sha256(exp / "model.onnx"))
        self.assertEqual(card["epochs"], 7)
        self.assertEqual(card["arch"], "yolo11s-seg")
        self.assertAlmostEqual(card["metrics"]["val.all.mask.map50"], 0.6)
        self.assertIn("test.hand.mask.map50", card["metrics"])
        exports = [c for c in ultralytics.CALLS if c[0] == "export"]
        self.assertEqual(exports[0][1]["opset"], 17)
        self.assertTrue(exports[0][1]["dynamic"])
        tr = [c for c in ultralytics.CALLS if c[0] == "train"][0][1]
        self.assertEqual(tr["imgsz"], 640)
        self.assertEqual(tr["fraction"], 1.0)

    def test_quick_mode(self):
        data = make_dataset(self.tmp / "ds")
        rc, _, err = run(["train", "--data", str(data), "--weights", str(self.weights), "--out", str(self.tmp / "runs"), "--name", "q", "--quick"])
        self.assertEqual(rc, 0, err)
        tr = [c for c in ultralytics.CALLS if c[0] == "train"][0][1]
        self.assertEqual((tr["epochs"], tr["fraction"]), (3, 0.1))

    def test_oom_retries_with_smaller_batch(self):
        data = make_dataset(self.tmp / "ds")
        torch.cuda.oom_batches = {16, 8}
        rc, out, err = run(["train", "--data", str(data), "--weights", str(self.weights), "--out", str(self.tmp / "runs"), "--name", "o"])
        self.assertEqual(rc, 0, err)
        batches = [c[1]["batch"] for c in ultralytics.CALLS if c[0] == "train"]
        self.assertEqual(batches, [16, 8, 4])
        torch.cuda.oom_batches = {16, 8, 4}
        rc, out, err = run(["train", "--data", str(data), "--weights", str(self.weights), "--out", str(self.tmp / "runs"), "--name", "o2"])
        self.assertEqual(rc, 1)
        self.assertIn("E-TRN-004", err)

    def test_errors_give_copy_blocks(self):
        torch.cuda.available = False
        rc, _, err = run(["check"])
        self.assertEqual(rc, 1)
        self.assertIn("COPY FROM HERE", err)
        self.assertIn("E-TRN-001", err)
        torch.cuda.available = True
        rc, _, err = run(["train", "--data", str(self.tmp / "nope.yaml"), "--weights", str(self.weights), "--out", str(self.tmp), "--name", "x"])
        self.assertIn("E-TRN-002", err)
        data = make_dataset(self.tmp / "bad", names=["a", "b"])
        rc, _, err = run(["train", "--data", str(data), "--weights", str(self.weights), "--out", str(self.tmp), "--name", "x"])
        self.assertIn("expected", err)
        data = make_dataset(self.tmp / "ok")
        rc, _, err = run(["train", "--data", str(data), "--weights", str(self.tmp / "missing.pt"), "--out", str(self.tmp), "--name", "x"])
        self.assertIn("E-TRN-006", err)

    def test_predict_writes_json(self):
        d = self.tmp / "golden"
        d.mkdir()
        (d / "t0.png").write_bytes(b"x")
        out = self.tmp / "u.json"
        rc, o, err = run(["predict", "--model", str(self.weights), "--images", str(d), "--out", str(out)])
        self.assertEqual(rc, 0, err)
        j = json.loads(out.read_text())
        self.assertEqual(j["tiles"][0]["file"], "t0.png")
        self.assertEqual(j["conf"], 0.25)


if __name__ == "__main__":
    unittest.main(verbosity=1)
