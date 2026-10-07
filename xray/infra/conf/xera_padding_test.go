package conf

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/transport/internet/xerahttp"
	"google.golang.org/protobuf/proto"
)

func TestCustomDownlinkPaddingBuild(t *testing.T) {
	for _, extra := range []bool{false, true} {
		value := `{"serverMaxBufferedBytes":67108864,"serverMaxSessions":128,"customDownlinkPadding":{"header":"X-Local-Profile","token":"0123456789abcdef","bytes":"24-192","blockBytes":"4096-8192","budgetPercent":1,"burstBytes":4096,"uplink":true}}`
		if extra {
			value = `{"extra":` + value + `}`
		}
		var c XeraHTTPConfig
		if err := json.Unmarshal([]byte(value), &c); err != nil {
			t.Fatal(err)
		}
		message, err := c.Build()
		if err != nil {
			t.Fatal(err)
		}
		wire, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		var decoded xerahttp.Config
		if err := proto.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		p := decoded.CustomDownlinkPadding
		if decoded.ServerMaxBufferedBytes != 67108864 || decoded.ServerMaxSessions != 128 || p == nil || !p.Uplink || p.BudgetPercent != 1 || p.BurstBytes != 4096 {
			t.Fatal("resource limits or adaptive settings lost")
		}
		if p == nil || p.Bytes.From != 24 || p.Bytes.To != 192 || p.BlockBytes.To != 8192 || p.Header != "X-Local-Profile" {
			t.Fatal("custom config lost during serialization")
		}
	}
}

func TestCustomDownlinkPaddingRejectsInvalidConfig(t *testing.T) {
	for _, fields := range []string{
		`"header":"Content-Length","token":"0123456789abcdef"`,
		`"header":"X-Padding","token":"0123456789abcdef"`,
		`"header":"X-Session","token":"0123456789abcdef"`,
		`"header":"X-Accel-Buffering","token":"0123456789abcdef"`,
		`"header":"X-Local","token":"short"`,
		`"header":"X-Local","token":"0123456789abcdef","bytes":"0-1025"`,
		`"header":"X-Local","token":"0123456789abcdef","blockBytes":"1-8192"`,
		`"header":"X-Local","token":"0123456789abcdef","blockBytes":"4096-65535"`,
		`"header":"X-Local","token":"0123456789abcdef","budgetPercent":101`,
		`"header":"X-Local","token":"0123456789abcdef","burstBytes":1`,
		`"header":"X-Local","token":"0123456789abcdef","budgetPercent":1,"burstBytes":1048577`,
	} {
		var c XeraHTTPConfig
		input := `{"sessionIDPlacement":"header","customDownlinkPadding":{` + fields + `}}`
		if err := json.Unmarshal([]byte(input), &c); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Build(); err == nil {
			t.Fatalf("accepted invalid config: %s", input)
		}
	}
}

func TestXeraRejectsInvalidRuntimeRanges(t *testing.T) {
	for _, value := range []string{
		`{"scMaxEachPostBytes":"-1-2"}`,
		`{"sessionIDLength":"-1-2","sessionIDTable":"abc"}`,
		`{"xmux":{"maxConnections":"-1-2"}}`,
		`{"scMaxEachPostBytes":16777217}`,
		`{"sessionIDLength":257,"sessionIDTable":"abc"}`,
		`{"serverMaxHeaderBytes":1048577}`,
		`{"serverMaxSessions":65537}`,
		`{"sessionIDLength":"1-2147483647","sessionIDTable":"abc"}`,
	} {
		var c XeraHTTPConfig
		if err := json.Unmarshal([]byte(value), &c); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Build(); err == nil {
			t.Fatalf("accepted invalid runtime range: %s", value)
		}
	}
}
