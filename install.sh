#!/bin/sh
# Installs the customizations into the Herdr app's config folder.
#   ./install.sh                  all features
#   ./install.sh devcontainers    only the named features (folders under features/)
# The `dev` layout always goes in: it is where the three panels live.
# Set HERDR_CONFIG to install somewhere other than ~/.config/herdr-app.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
config=${HERDR_CONFIG:-$HOME/.config/herdr-app}
bin=${HERDR_BIN:-$HOME/.local/bin}
lib=${HERDR_LIB:-$HOME/.local/lib/herdr}

[ $# -gt 0 ] || set -- $(ls "$here/features")

mkdir -p "$config/layouts" "$config/mods"
cp "$here/layout/dev.json" "$config/layouts/dev.json"
echo "layout: dev"

for feature in "$@"; do
  dir=$here/features/$feature
  [ -d "$dir" ] || { echo "no such feature: $feature" >&2; exit 1; }
  for mod in "$dir"/mods/*/; do
    name=$(basename "$mod")
    mkdir -p "$config/mods/$name"
    cp "$mod"* "$config/mods/$name/"
    echo "$feature: mod $name"
  done
  if [ -d "$dir/bin" ]; then
    mkdir -p "$bin"
    for f in "$dir"/bin/*; do
      install -m 755 "$f" "$bin/"
      echo "$feature: $bin/$(basename "$f")"
    done
  fi
  if [ "$feature" = devcontainers ]; then
    # herdr-dc was herdr-dc-ports before; drop the old builds.
    rm -f "$bin/herdr-dc-ports" "$lib"/herdr-dc-ports*
    # herdr-dc's config, only when there's none yet: the live one may differ.
    dcconf=${XDG_CONFIG_HOME:-$HOME/.config}/herdr-dc/config.json
    if [ ! -e "$dcconf" ]; then
      mkdir -p "$(dirname "$dcconf")"
      cp "$dir/herdr-dc.config.json" "$dcconf"
      echo "$feature: $dcconf"
    fi
  fi
  for mod in "$dir"/*/go.mod; do
    [ -f "$mod" ] || continue
    # A Go helper: static builds for both Linux architectures (one may be copied
    # into containers), the host's linked into $bin. Rebuilt only when changed.
    src=$(dirname "$mod")
    name=$(sed -n 's/^module //p' "$mod")
    if ! command -v go >/dev/null; then
      echo "$feature: go not found, skipping $name (https://go.dev/dl)" >&2
      continue
    fi
    version=$(cat "$src"/*.go "$mod" | sha256sum | cut -c1-12)
    mkdir -p "$lib" "$bin"
    if [ "$(cat "$lib/$name.version" 2>/dev/null)" != "$version" ]; then
      for arch in amd64 arm64; do
        (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$lib/$name-linux-$arch" .)
      done
      echo "$version" > "$lib/$name.version"
    fi
    host=$(go env GOARCH)
    ln -sf "$lib/$name-linux-$host" "$bin/$name"
    echo "$feature: $bin/$name ($version)"
  done
  if [ -f "$dir/keys.json" ]; then
    # Merge the feature's bindings into keys.json, keeping everything already there.
    python3 - "$config/keys.json" "$dir/keys.json" <<'PY'
import json, os, sys
target, extra = sys.argv[1], sys.argv[2]
keys = json.load(open(target)) if os.path.exists(target) else {}
for section, binds in json.load(open(extra)).items():
    keys.setdefault(section, {}).update(binds)
json.dump(keys, open(target, "w"), indent=2, ensure_ascii=True)
PY
    echo "$feature: keys merged"
  fi
done

if command -v herdr-app >/dev/null && [ -z "${HERDR_CONFIG:-}" ]; then
  for feature in "$@"; do
    for mod in "$here/features/$feature"/mods/*/; do
      herdr-app check "$config/mods/$(basename "$mod")"
    done
  done
fi
echo "Done. Switch to the layout with ctrl-shift-l → dev."
