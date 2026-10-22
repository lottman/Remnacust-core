package conf

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/transport/internet/finalmask/xdns"
	"google.golang.org/protobuf/proto"
)

func TestXDNSLegacyConfiguration(t *testing.T) {
	cases := [][2]string{
		{`{"domains":[{"name":"test.example.com","types":[16]}],"resolvers":[{"type":"tcp","settings":{"addr":"[::1]:5353"}}]}`, `{"domains":[{"names":["test.example.com"],"types":[16]}],"resolvers":[{"addrs":["tcp://[::1]:5353"]}]}`},
		{`{"resolvers":[{"type":"udp","settings":{"addr":"1.1.1.1:53"}}]}`, `{"resolvers":[{"addrs":["1.1.1.1"]}]}`},
		{`{"domains":[{"name":"old.example.com","names":["new.example.com"]}],"resolvers":[{"addrs":["udp://1.1.1.1"],"type":"tcp","settings":{"addr":"2.2.2.2:53"}}]}`, `{"domains":[{"names":["new.example.com"]}],"resolvers":[{"addrs":["udp://1.1.1.1"]}]}`},
	}
	for _, pair := range cases {
		var old, current XDNS
		if err := json.Unmarshal([]byte(pair[0]), &old); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(pair[1]), &current); err != nil {
			t.Fatal(err)
		}
		a, err := old.Build()
		if err != nil {
			t.Fatal(err)
		}
		b, err := current.Build()
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(a, b) {
			t.Fatalf("legacy and current configuration differ: %v / %v", a, b)
		}
	}
	var invalid XDNS
	if err := json.Unmarshal([]byte(`{"resolvers":[{"type":"http","settings":{"addr":"1.1.1.1:53"}}]}`), &invalid); err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Build(); err == nil {
		t.Fatal("unsupported legacy resolver protocol accepted")
	}
}

func TestXDNSPublishedReleasePreservesDoHOptions(t *testing.T) {
	var config XDNS
	if err := json.Unmarshal([]byte(`{"resolvers":[{"addrs":["https://resolver.invalid/dns-query?mode=wire"],"worker":4,"tlsConfig":{"serverName":"front.invalid","alpn":["h3"]},"quicConfig":{"congestion":"bbr","bbrProfile":"aggressive"}},{"type":"udp","settings":{"addr":"127.0.0.1:5353"}}]}`), &config); err != nil {
		t.Fatal(err)
	}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	resolvers := built.(*xdns.Config).Resolvers
	if len(resolvers) != 2 || resolvers[0].Type != "https" || resolvers[0].Worker != 4 || resolvers[0].TlsConfig.ServerName != "front.invalid" || resolvers[0].QuicConfig.BbrProfile != "aggressive" || resolvers[1].Type != "udp" {
		t.Fatalf("published DoH options or legacy UDP resolver lost: %v", resolvers)
	}
}
