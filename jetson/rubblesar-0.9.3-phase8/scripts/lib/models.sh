#!/usr/bin/env bash
# Model-folder helpers for the phase 2 scripts (requires common.sh).

MODEL_ROOT=/var/lib/rubblesar/models

# install_model SRC NAME: copy a model folder into $MODEL_ROOT/NAME (read-only
# for services) and point $MODEL_ROOT/current at it.
install_model() {
  local src="$1" name="$2" dst="$MODEL_ROOT/$2"
  [[ -f "$src/model.json" && -f "$src/model.onnx" ]] || die E-SETUP-MODEL "$src is not a model folder (model.json, model.onnx)"
  [[ -d /var/lib/rubblesar ]] || sudo install -d -m 0755 /var/lib/rubblesar
  [[ -d "$MODEL_ROOT" ]] || sudo install -d -m 0755 "$MODEL_ROOT"
  sudo rm -rf "$dst.new"
  sudo cp -a "$src" "$dst.new"
  sudo chown -R root:root "$dst.new"
  sudo chmod -R u=rwX,go=rX "$dst.new"
  sudo rm -rf "$dst"
  sudo mv "$dst.new" "$dst"
  sudo ln -sfn "$name" "$MODEL_ROOT/current"
  ok "installed $dst ($(du -sh "$dst" | cut -f1)); $MODEL_ROOT/current -> $name"
}

# model_name DIR: the name in DIR/model.json.
model_name() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$1/model.json" 2>/dev/null \
    || die E-SETUP-MODEL "cannot read the model name from $1/model.json"
}
