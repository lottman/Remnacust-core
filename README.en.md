[Русский](README.md) · [English](README.en.md)

# Remnacust Core

An Xray-core fork for Remnacust: client connections, routing, runtime configuration updates and host/device policies. The repository also contains the panel editor's schema and browser validator.

## Install ready components

Download the latest stable installer and open its interactive menu:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/lottman/Remnacust-installer/main/Install.sh) @ install
```

## Current components

| Component | Version |
| --- | --- |
| Core | [1.1.22](https://github.com/lottman/Remnacust-core/releases/tag/v1.1.22) |
| Xray base | [v26.11.9](https://github.com/XTLS/Xray-core/releases/tag/v26.11.9) |
| Node | [1.1.37](https://github.com/lottman/Remnacust-node/releases/tag/v1.1.37) |
| Panel and subscription page | [1.1.7.126](https://github.com/lottman/Remnacust-panel/releases/tag/v1.1.7.126) |
| Installer | [1.2.174](https://github.com/lottman/Remnacust-installer/releases/tag/v1.2.174) |

The base is published official Xray v26.11.9, commit `676eaf66f8f05d4d558ee7a74b4736623d3feeb9`. Provenance: [REMNACUST-UPSTREAM.json](xray/REMNACUST-UPSTREAM.json); see [Core 1.1.22 changes](docs/releases/1.1.22.md). New source in the branch does not mean the published binary has been updated.

## Installation and updates

For a VPS, use the ready components described in the [Node guide](https://github.com/lottman/Remnacust-node). Installer 1.2.172 supports Ubuntu 22.04/24.04/26.04 LTS and Debian 12/13, Linux amd64/arm64. The starting node baseline is 1 CPU and 1 GiB RAM without a capacity guarantee; Node.js and Go are not needed on the host.

Core releases include Linux amd64/arm64 binaries and `SHA256SUMS`. For a managed node, select a verified core through the panel or `sudo remnacust manage-node`; do not manually replace a running binary. Updating the panel does not update remote nodes. `sudo remnacust upgrade-node` updates the agent image with its bundled Core, but a separately selected managed core may remain pinned. Check the active version and mode.

Preserve `/opt/remnacust/node/.env`, `SECRET_KEY`, Compose and all private state: `/var/lib/remnacust/run`, `/var/lib/remnanode`, `/var/lib/remnawave/core-manager`. The Installer persists these in directories beside Compose; paths are listed in `/opt/remnacust/FILES.txt`. Stop older manual deployments without mounts and preserve state with ownership and permissions before recreation. Do not hide the only copy behind empty mounts or delete quota journals to force a downgrade.

## Features and compatibility

Node 1.1.37 contains two separate binaries: custom Core 1.1.22 and stock Xray v26.11.9. The panel's global mode defaults to custom. Default does not support Remnacust extensions; changing modes checks saved policies and quotas without deleting journals or silently removing limits. The editor uses separate custom and standard WASM/schema assets with pinned security dependency updates; browser validation does not prove a native connection works.

- Xera HTTP: `network: "xera-http"` and `xeraHttpSettings`; compatible client and server cores are required. Standard XHTTP is a separate transport. [Admission settings, resource limits and observable behavior](docs/XERA-HTTP.md).
- Host/tag policies, quotas and user/device speed limits. Additional connections do not increase the allocated budget.
- Access revocation using the authenticated key without disconnecting other devices behind the same NAT.
- Compatible runtime updates can preserve connections. Changing a listener, port, transport or process requires planning for interruption.

Adaptive Xera HTTP offers Balanced (balanced), Low latency (low-latency) and Unstable network (unstable-network). They adjust concurrency using measured HTTP responses, data transfer and request waiting. When adaptive is present, all three fields are required: mode, minStreams and maxStreams. Integer bounds range from 1 to 32, with the minimum no greater than the maximum; the panel form offers 4/32. Limits count simultaneously active HTTP requests separately for each direction, destination and generation of transport settings. They do not count TCP connections or users, or distribute CPU load.

Without adaptive, previous behavior, manual JSON and extra remain available. Adjustment admits new requests without aborting or replaying existing ones; auto/stream-auto transfer modes, ALPN, padding and quotas are preserved. Adaptation requires Core 1.1.22 or later in a client supporting Xera HTTP. The existing Xera wire format is retained. Profiles use measured feedback without AI training and do not guarantee DPI invisibility or an advantage in any particular country.

Standard configurations retain legacy compatibility within each client's/core's capabilities. Compatible XDNS, TUN, QUIC udpHop and outbound XDRIVE segment announcement variants are retained as listed in provenance. Ordinary upstream Xray does not automatically gain Remnacust extensions; do not bypass persisted policy checks to install an unsuitable core. Xray v26.11.9 disables MPTCP by default; updating Core does not replace Linux kernel updates.

[Compatibility](docs/COMPATIBILITY.md) · [Profile graphs](docs/PROFILE-GRAPHS.md) · [Limit scopes](https://github.com/lottman/Remnacust-panel/blob/main/docs/HOST-LIMITS.md).

## Offline operation

Existing connections and policies can operate without the panel while access rights and local grants remain valid. Access expiry, remaining applicable USER/HOST/TAG quotas and quota period deadlines continue to limit traffic. A new calendar period without the panel does not create fresh credit.

A day of offline operation requires enough validity time and local credit for traffic and recovery. Restart requires compatible Node/Core versions and persisted configuration, credentials and journals. There is no 24-hour guarantee for every older core or after state loss. [Offline operation requirements](https://github.com/lottman/Remnacust-panel/blob/main/docs/NODE-AUTONOMY.md).

## Core 1.1.22

Adds three Xera HTTP adaptation profiles with explicit request bounds and preserved manual settings. XDRIVE bounds pending WAL segments, ends stalled transfers at the session TTL and cleans closed-session data with retries after storage errors. Legacy gRPC user and statistics operations honor cancellation after waiting; cancellation itself does not roll back changes already applied. The WAL and gRPC cancellation fixes retain existing wire and configuration formats. The official Xray v26.11.9 baseline is retained. [1.1.22 changes](docs/releases/1.1.22.md).

## Core 1.1.21

Fixes Xera/XHTTP memory accounting and stream lifecycle, Browser Dialer, ECH acquisition, TLS certificate rotation and oversized Vision buffers. Updates compatible dependencies while preserving transport formats. [1.1.21](docs/releases/1.1.21.md).

## Core 1.1.20

Fixes Xera session retention, HTTP/3 header limits, Firefox versions and Client Hints. Adds the verified partial Chrome 155 HTTP/2 profile. [1.1.20](docs/releases/1.1.20.md).

## Core 1.1.19 changes

Updates stable Go dependencies and the standalone editor to Node.js 26.13 LTS, React 19 and Mantine 9; Monaco uses verified DOMPurify. Adds a separate pristine upstream browser validator build for default mode. Custom transports, patched uTLS/olcRTC and offline enforcement remain intact. [Release changes](docs/releases/1.1.19.md).

## Build and validation

**Go 1.27.4** is required. Clone the whole repository: `xray/go.mod` uses relative paths to `vendor/olcrtc` and `vendor/utls`. The module name `github.com/xtls/xray-core` is retained for import compatibility.

```bash
git clone https://github.com/lottman/Remnacust-core.git
cd Remnacust-core/xray
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/xray ./main
./bin/xray version
./bin/xray run -test -config /path/to/config.json
```

This builds the current checkout. The number in `VERSION` alone does not prove that a build matches the published release. `-test` validates configuration, not DNS, certificate or remote server availability. For a managed node, edit the profile through the panel: the agent prepares the internal runtime configuration.

From the `xray` directory:

```bash
go test -race ./infra/conf ./core ./app/dispatcher ./app/stats ./app/stats/command \
  ./app/proxyman/inbound ./app/proxyman/outbound ./app/proxyman/command \
  ./transport/internet/splithttp ./transport/internet/xerahttp ./transport/internet/xdrive
```

Run network checks in an isolated Linux environment. [Build guide](docs/BUILDING.md) · [Source checks](.github/workflows/check.yml). Updating Node's bundled Core requires separately repackaging the verified archive and rebuilding the image.

## Panel editor

The schema and WASM use the same configuration builders as native Core. Editor builds require Go 1.27.4, Node.js 26.13 LTS, Python 3 and the panel frontend dependencies; these are development tools rather than VPS requirements. An adjacent panel checkout is detected automatically; set `REMNACUST_PANEL_SOURCE` for another location.

From the repository root:

```bash
bash editor/go/build-remnacust.sh
```

The script updates the schema and WASM and checks panel resources; rebuild the frontend afterwards. [Editor README](editor/README.md). Documentation-only changes do not require rebuilding binaries or archives.

## Attribution and licenses

Remnacust is maintained by lottman. Attribution to [XTLS/Xray-core](https://github.com/XTLS/Xray-core) and its contributors, the [MPL-2.0](xray/LICENSE) license and original notices are retained. The editor and dependencies retain their own licenses. [NOTICE.md](NOTICE.md) · [Panel](https://github.com/lottman/Remnacust-panel) · [Node](https://github.com/lottman/Remnacust-node) · [Support](https://t.me/lottman).

XDRIVE keeps legacy segment announcements by default. `xdriveSettings.useSlots: true` enables the new upstream slot format and requires a compatible peer. Updating Core does not patch the Linux kernel; check your OS vendor status for CVE-2026-98432.
