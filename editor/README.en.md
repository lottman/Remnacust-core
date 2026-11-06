[Русский](README.md) · [English](README.en.md)

# Remnacust configuration editor

The validator and schema are built from the current **Remnacust Core 1.1.14 / Xray v26.10.31**. Rebuild and ship them together after changing Core; updating the source version does not replace the panel's previous WASM. The production editor interface lives in [Remnacust-panel](https://github.com/lottman/Remnacust-panel/tree/main/panel/frontend). This directory contains the Go/WASM build and standalone Monaco example.

## Updating WASM and the schema

Development requires Go 1.27.3, Python 3, Node.js 24 and the panel frontend dependencies. Clone the panel beside Core, then run from this repository root:

```bash
bash editor/go/build-remnacust.sh
```

For another location set `REMNACUST_PANEL_SOURCE=/path/to/Remnacust-panel` first. The builder prepares browser-compatible Core source, generates types/schema, builds `main.wasm`, copies the matching `wasm_exec.js` and runs the panel's `verify-editor-assets.mjs`.

Use the WASM runtime from the same Go version. Do not replace only `.wasm` while keeping an old schema or `wasm_exec.js`. Rebuild the panel image after updating assets; update node Core separately.

The browser uses Core configuration builders but does not open native sockets. Successful validation does not confirm DNS reachability, certificate validity or tunnel startup in Happ/INCY. Unreleased runtime fixes require their own Core/Node release and are not installed by updating editor documentation.

[Core README](../README.en.md) · [Build details](../docs/BUILDING.md) · [Build script](go/build-remnacust.sh) · [Licenses](../NOTICE.md).
