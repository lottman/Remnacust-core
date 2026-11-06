[Русский](README.md) · [English](README.en.md)

# Remnacust Core

An Xray-core fork for Remnacust: client connections, routing, runtime configuration updates and host/device policies. The repository also contains the panel editor's schema and browser validator.

## Stable components

| Component | Published version |
| --- | --- |
| Core | [1.1.13](https://github.com/lottman/Remnacust-core/releases/tag/v1.1.13) |
| Xray base | [v26.10.31](https://github.com/XTLS/Xray-core/releases/tag/v26.10.31) |
| Node | [1.1.22](https://github.com/lottman/Remnacust-node/releases/tag/v1.1.22) |
| Panel and subscription page | [1.1.7.97](https://github.com/lottman/Remnacust-panel/releases/tag/v1.1.7.97) |
| Installer | [1.2.136](https://github.com/lottman/Remnacust-installer/releases/tag/v1.2.136) |

The base is published official Xray v26.10.31, commit `a4a3dff5b20c2faae567b742c172c7b95e75c083`. Provenance: [REMNACUST-UPSTREAM.json](xray/REMNACUST-UPSTREAM.json); see [Core 1.1.13 changes](docs/RELEASE-1.1.13.md). New source in the branch does not mean the published binary has been updated.

## Installation and updates

For a VPS, use the ready components described in the [Node guide](https://github.com/lottman/Remnacust-node). Stable Installer 1.2.136 supports Ubuntu 22.04/24.04/26.04 LTS, Linux amd64/arm64. Debian 12/13 support has been added locally but is not yet published. The starting node baseline is 1 CPU and 1 GiB RAM without a capacity guarantee; Node.js and Go are not needed on the host.

Core releases include Linux amd64/arm64 binaries and `SHA256SUMS`. For a managed node, select a verified core through the panel or `sudo remnacust manage-node`; do not manually replace a running binary. Updating the panel does not update remote nodes. `sudo remnacust upgrade-node` updates the agent image with its bundled Core, but a separately selected managed core may remain pinned. Check the active version and mode.

Preserve `/opt/remnacust/node/.env`, `SECRET_KEY`, Compose and all private state: `/var/lib/remnacust/run`, `/var/lib/remnanode`, `/var/lib/remnawave/core-manager`. The Installer persists these in directories beside Compose; paths are listed in `/opt/remnacust/FILES.txt`. Stop older manual deployments without mounts and preserve state with ownership and permissions before recreation. Do not hide the only copy behind empty mounts or delete quota journals to force a downgrade.

## Features and compatibility

- Xera HTTP: `network: "xera-http"` and `xeraHttpSettings`; compatible client and server cores are required. Standard XHTTP is a separate transport.
- Host/tag policies, quotas and user/device speed limits. Additional connections do not increase the allocated budget.
- Access revocation using the authenticated key without disconnecting other devices behind the same NAT.
- Compatible runtime updates can preserve connections. Changing a listener, port, transport or process requires planning for interruption.

Standard configurations retain legacy compatibility within each client's/core's capabilities. Compatible XDNS, TUN, QUIC udpHop and outbound XDRIVE segment announcement variants are retained as listed in provenance. Ordinary upstream Xray does not automatically gain Remnacust extensions; do not bypass persisted policy checks to install an unsuitable core. Xray v26.10.31 disables MPTCP by default; updating Core does not replace Linux kernel updates.

[Compatibility](docs/COMPATIBILITY.md) · [Profile graphs](docs/PROFILE-GRAPHS.md) · [Limit scopes](https://github.com/lottman/Remnacust-panel/blob/main/docs/HOST-LIMITS.md).

## Offline operation

Existing connections and policies can operate without the panel while access rights and local grants remain valid. Access expiry, remaining applicable USER/HOST/TAG quotas and quota period deadlines continue to limit traffic. A new calendar period without the panel does not create fresh credit.

A day of offline operation requires enough validity time and local credit for traffic and recovery. Restart requires compatible Node/Core versions and persisted configuration, credentials and journals. There is no 24-hour guarantee for every older core or after state loss. [Offline operation requirements](https://github.com/lottman/Remnacust-panel/blob/main/docs/NODE-AUTONOMY.md).

## Not yet published

The local branch contains fixes for MUX session and quota lease ownership, a 300-second read limit for unfinished Xera packet-up bodies with cancellation cleanup (without a general timeout for streaming/long polling), certificate decompression fixes on a compatible uTLS base, and Brotli/compress updates. The new Xera profile requires coordinated client and server updates. Quota recovery after some grants expire requires coordinated new Node/Core builds. **These changes are not yet included in published Core 1.1.13**; a local build does not update releases or installed images.

## Build and validation

**Go 1.27.3** is required. Clone the whole repository: `xray/go.mod` uses relative paths to `vendor/olcrtc` and `vendor/utls`. The module name `github.com/xtls/xray-core` is retained for import compatibility.

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

The schema and WASM use the same configuration builders as native Core. Editor builds require Go 1.27.3, Node.js 24, Python 3 and the panel frontend dependencies; these are development tools rather than VPS requirements. An adjacent panel checkout is detected automatically; set `REMNACUST_PANEL_SOURCE` for another location.

From the repository root:

```bash
bash editor/go/build-remnacust.sh
```

The script updates the schema and WASM and checks panel resources; rebuild the frontend afterwards. [Editor README](editor/README.md). Documentation-only changes do not require rebuilding binaries or archives.

## Attribution and licenses

Remnacust is maintained by lottman. Attribution to [XTLS/Xray-core](https://github.com/XTLS/Xray-core) and its contributors, the [MPL-2.0](xray/LICENSE) license and original notices are retained. The editor and dependencies retain their own licenses. [NOTICE.md](NOTICE.md) · [Panel](https://github.com/lottman/Remnacust-panel) · [Node](https://github.com/lottman/Remnacust-node) · [Support](https://t.me/lottman).
