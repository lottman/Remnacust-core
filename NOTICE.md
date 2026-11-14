# Attribution and licenses

Remnacust is a fork maintained by lottman. Panel and node derive from Remnawave and retain AGPL-3.0-only; original notices are preserved in each component. The Xray fork retains its upstream MPL-2.0 license and notices. The online editor retains its own LICENSE. Bundled olcRTC source is a dependency of the preserved custom core, under WTFPL; it is not a panel/node integration. Third-party dependencies keep their respective licenses.

Upstream source: https://github.com/remnawave/backend, https://github.com/remnawave/frontend, https://github.com/remnawave/node, https://github.com/XTLS/Xray-core.

The subscription page derives from https://github.com/remnawave/subscription-page (8.0.0), under AGPL-3.0. Its original LICENCE and author notices are retained.


The Xera HTTP/2 client uses github.com/bogdanfinn/fhttp v0.7.3, a fork of the Go HTTP implementation. Its source retains The Go Authors copyright and BSD-style notices; the original Go license is available at https://go.dev/LICENSE. The transitive github.com/bogdanfinn/utls v1.8.2-barnius retains its LICENSE. The existing TLS/REALITY handshake and certificate verification remain provided by the Xray-side implementation.

The local vendor/utls module retains the source and licenses from refraction-networking/utls v1.8.3-0.20261108164504-a13c59ffe98b (a13c59ffe98bb01b2ca14143eb2e887664b1f138). The certificate decompression checks are included in this upstream revision. vendor/utls/LICENSE and vendor/utls/dicttls/LICENSE preserve the original notices. REMNACUST-PATCH.json records upstream source checksums and local modifications; certificate-decompression.patch is empty when no local backport is required.
