[Русский](README.md) · [English](README.en.md)

# Remnacust configuration editor

The validator and schema are built from the current **Remnacust Core 1.1.16 / Xray v26.10.31**. Rebuild and ship them together after changing Core; updating the source version does not replace the panel's previous WASM. The production editor interface lives in [Remnacust-panel](https://github.com/lottman/Remnacust-panel/tree/main/panel/frontend). This directory contains the Go/WASM build and standalone Monaco example.

## Install ready components

Download the latest stable installer and open its interactive menu:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/lottman/Remnacust-installer/main/bootstrap.sh) @ install
```

## Updating WASM and the schema

The panel selects custom or default assets from its global mode. `go/build-remnacust.sh` prepares custom Core; `python3 go/build-default.py` from this directory builds `default.wasm`, `xray.default.schema.json` and provenance from pinned pristine upstream v26.10.31. The two asset sets are not interchangeable; switching editor mode requires a page reload.

Development requires Go 1.27.3, Python 3, Node.js 26.13 LTS and the panel frontend dependencies. Clone the panel beside Core, then run from this repository root:

```bash
bash editor/go/build-remnacust.sh
```

For another location set `REMNACUST_PANEL_SOURCE=/path/to/Remnacust-panel` first. The builder prepares browser-compatible Core source, generates types/schema, builds `main.wasm`, copies the matching `wasm_exec.js` and runs the panel's `verify-editor-assets.mjs`.

Use the WASM runtime from the same Go version. Do not replace only `.wasm` while keeping an old schema or `wasm_exec.js`. Rebuild the panel image after updating assets; update node Core separately.

The browser uses Core configuration builders but does not open native sockets. Successful validation does not confirm DNS reachability, certificate validity or tunnel startup in Happ/INCY. Unreleased runtime fixes require their own Core/Node release and are not installed by updating editor documentation.

[Core README](../README.en.md) · [Build details](../docs/BUILDING.md) · [Build script](go/build-remnacust.sh) · [Licenses](../NOTICE.md).
