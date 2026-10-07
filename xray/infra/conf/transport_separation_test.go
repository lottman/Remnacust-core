package conf

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/splithttp"
	"github.com/xtls/xray-core/transport/internet/xerahttp"
)

func TestIndependentHTTPTransports(t *testing.T) {
	for _, test := range []struct{ network, protocol string }{
		{"xhttp", "splithttp"}, {"splithttp", "splithttp"}, {"xera", "xera-http"}, {"xera-http", "xera-http"},
	} {
		t.Run(test.network, func(t *testing.T) {
			var config StreamConfig
			input := `{"network":"` + test.network + `","xhttpSettings":{"path":"/legacy"},"xeraHttpSettings":{"path":"/custom","serverMaxBufferedBytes":67108864,"serverMaxSessions":128}}`
			if err := json.Unmarshal([]byte(input), &config); err != nil {
				t.Fatal(err)
			}
			built, err := config.Build()
			if err != nil {
				t.Fatal(err)
			}
			memory, err := internet.ToMemoryStreamConfig(built)
			if err != nil {
				t.Fatal(err)
			}
			if memory.ProtocolName != test.protocol {
				t.Fatalf("wrong protocol: %s", memory.ProtocolName)
			}
			switch settings := memory.ProtocolSettings.(type) {
			case *xerahttp.Config:
				if test.protocol != "xera-http" || settings.Path != "/custom" || settings.ServerMaxSessions != 128 || settings.ServerMaxBufferedBytes != 67108864 {
					t.Fatal("custom settings lost or routed to legacy transport")
				}
			case *splithttp.Config:
				if test.protocol != "splithttp" || settings.Path != "/legacy" {
					t.Fatal("legacy settings routed to custom transport")
				}
			default:
				t.Fatalf("unexpected config type: %T", settings)
			}
		})
	}
}

func TestXeraDownloadTransportValidation(t *testing.T) {
	for _, network := range []TransportProtocol{"xera", "xera-http", "xhttp", "tcp"} {
		var config XeraHTTPConfig
		if err := json.Unmarshal([]byte(`{"downloadSettings":{"address":"example.com","port":443,"network":"`+string(network)+`"}}`), &config); err != nil {
			t.Fatal(err)
		}
		_, err := config.Build()
		if (err == nil) != (network == "xera" || network == "xera-http") {
			t.Fatalf("download network %s: %v", network, err)
		}
	}
}

func TestXeraTransportBuildsWithReality(t *testing.T) {
	input := `{"network":"xera","security":"reality","realitySettings":{"target":"example.com:443","serverNames":["example.com"],"privateKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","shortIds":["00000000"]},"xeraHttpSettings":{"path":"/socket","mode":"stream-one"}}`
	var config StreamConfig
	if err := json.Unmarshal([]byte(input), &config); err != nil {
		t.Fatal(err)
	}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	memory, err := internet.ToMemoryStreamConfig(built)
	if err != nil {
		t.Fatal(err)
	}
	if memory.ProtocolName != "xera-http" || memory.SecurityType == "" {
		t.Fatalf("unexpected stream config: protocol=%s security=%s", memory.ProtocolName, memory.SecurityType)
	}
}

func TestXeraDefaultsAvoidLegacyHTTPMarkers(t *testing.T) {
	config := XeraHTTPConfig{}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	settings, ok := built.(*xerahttp.Config)
	if !ok {
		t.Fatalf("unexpected config type: %T", built)
	}
	if !settings.NoGRPCHeader || !settings.NoSSEHeader {
		t.Fatal("XERA defaults must omit legacy gRPC and SSE headers")
	}
	if settings.XPaddingKey != "p" || settings.XPaddingHeader != "X-Pad" || settings.XPaddingPlacement != "query" || settings.XPaddingMethod != "tokenish" {
		t.Fatal("XERA defaults must use the custom wire profile")
	}
}
