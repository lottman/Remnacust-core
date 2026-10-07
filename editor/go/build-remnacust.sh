#!/usr/bin/env bash
set -euo pipefail
editor_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
workspace="$(cd -- "$editor_dir/../.." && pwd)"
if [[ -n ${REMNACUST_PANEL_SOURCE:-} ]]; then
    frontend_dir="$(cd -- "$REMNACUST_PANEL_SOURCE/panel/frontend" && pwd)"
elif [[ -d "$workspace/panel/frontend" ]]; then
    frontend_dir="$workspace/panel/frontend"
else
    frontend_dir="$workspace/../Remnacust-panel/panel/frontend"
fi
test -f "$frontend_dir/package.json" || { echo "Clone Remnacust-panel next to Remnacust-core or set REMNACUST_PANEL_SOURCE" >&2; exit 1; }
python3 "$editor_dir/prepare-remnacust.py"
cd "$editor_dir"
go run -buildvcs=false tools/schema-types.go ../../xray/infra/conf assets/remnacust-config-types.json
python3 update-schema.py assets/remnacust-config-types.json "$frontend_dir"
GOOS=js GOARCH=wasm go build -buildvcs=false -mod=mod -o "$frontend_dir/public/assets/main.wasm" .
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$frontend_dir/public/assets/wasm_exec.js"
cd "$frontend_dir"
node verify-editor-assets.mjs
