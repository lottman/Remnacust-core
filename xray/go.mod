module github.com/xtls/xray-core

go 1.27

require (
	github.com/apernet/quic-go v0.61.1-0.20260806010916-184d081eef3e
	github.com/cloudflare/circl v1.6.5
	github.com/ghodss/yaml v1.0.1-0.20220118164431-d8423dcdf344
	github.com/golang/mock v1.7.0-rc.1
	github.com/google/go-cmp v0.7.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	github.com/klauspost/cpuid/v2 v2.4.0
	github.com/libp2p/go-nat v1.0.1-0.20250821073202-01afc089f138
	github.com/miekg/dns v1.1.73
	github.com/openlibrecommunity/olcrtc v0.0.0-00010101000000-000000000000
	github.com/pelletier/go-toml v1.9.5
	github.com/pion/stun/v3 v3.1.7
	github.com/pires/go-proxyproto v0.15.0
	github.com/refraction-networking/utls v1.8.3-0.20260301010127-aa6edf4b11af
	github.com/robfig/cron/v3 v3.0.1
	github.com/stretchr/testify v1.12.1
	github.com/vishvananda/netlink v1.3.1
	github.com/xtls/reality v0.0.0-20260908062103-8cdf7bf9c7f0
	go4.org/netipx v0.0.0-20231129151722-fdeea329fbba
	golang.org/x/crypto v0.57.0
	golang.org/x/exp v0.0.0-20260603202125-055de637280b
	golang.org/x/net v0.59.0
	golang.org/x/sync v0.23.0
	golang.org/x/sys v0.48.0
	golang.org/x/time v0.15.0
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb
	golang.zx2c4.com/wireguard/windows v1.1.1
	google.golang.org/grpc v1.85.0-dev.0.20260825072537-93e31b48545e
	google.golang.org/protobuf v1.36.12
	gvisor.dev/gvisor v0.0.0-20260122175437-89a5d21be8f0
	h12.io/socks v1.0.3
	lukechampine.com/blake3 v1.4.1
	mvdan.cc/gofumpt v0.12.0
)

replace github.com/openlibrecommunity/olcrtc => ../vendor/olcrtc

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260415201107-50325440f8f2.1 // indirect
	buf.build/go/protovalidate v1.2.0 // indirect
	buf.build/go/protoyaml v0.7.0 // indirect
	cel.dev/expr v0.25.3 // indirect
	codeberg.org/rape4me/kc v0.0.0-20260527074346-4cb2a45790c2 // indirect
	github.com/andybalholm/brotli v1.0.6 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bep/debounce v1.2.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/dennwc/iters v1.2.2 // indirect
	github.com/frostbyte73/core v0.1.1 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gammazero/deque v1.2.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/cel-go v0.30.0 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/huin/goupnp v1.2.0 // indirect
	github.com/jackpal/go-nat-pmp v1.0.2 // indirect
	github.com/juju/ratelimit v1.0.2 // indirect
	github.com/jxskiss/base62 v1.1.0 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/klauspost/reedsolomon v1.14.0 // indirect
	github.com/koron/go-ssdp v0.0.4 // indirect
	github.com/libp2p/go-netroute v0.2.1 // indirect
	github.com/lithammer/shortuuid/v4 v4.2.0 // indirect
	github.com/livekit/mageutil v0.0.0-20250511045019-0f1ff63f7731 // indirect
	github.com/livekit/mediatransportutil v0.0.0-20260605212259-862d4a7bcb1e // indirect
	github.com/livekit/protocol v1.50.1 // indirect
	github.com/livekit/psrpc v0.7.2 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/makiuchi-d/gozxing v0.1.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/nats.go v1.52.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/owenewans/owenlivekit/v2 v2.18.2 // indirect
	github.com/pion/datachannel v1.6.0 // indirect
	github.com/pion/dtls/v3 v3.1.5 // indirect
	github.com/pion/ice/v4 v4.2.7 // indirect
	github.com/pion/interceptor v0.1.45 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.2 // indirect
	github.com/pion/sctp v1.10.0 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.11 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	github.com/pion/turn/v5 v5.0.9 // indirect
	github.com/pion/webrtc/v4 v4.2.15 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.68.1 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/puzpuzpuz/xsync/v4 v4.5.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/redis/go-redis/v9 v9.20.0 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	github.com/twitchtv/twirp v8.1.3+incompatible // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/xtaci/kcp-go/v5 v5.6.72 // indirect
	github.com/xtaci/smux v1.5.57 // indirect
	github.com/zarazaex69/gr v0.0.1 // indirect
	github.com/zarazaex69/j v0.0.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	rsc.io/qr v0.2.0 // indirect
)
