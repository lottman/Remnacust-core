package conf

import (
	"encoding/json"
	"testing"

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
