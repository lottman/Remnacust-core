package conf

import (
	"encoding/json"
	"testing"

	"github.com/xtls/xray-core/transport/internet/xerahttp"
	"google.golang.org/protobuf/proto"
)

func TestXeraAdaptiveContract(t *testing.T) {
	for _, mode := range []string{"balanced", "low-latency", "unstable-network"} {
		for _, extra := range []bool{false, true} {
			options := `"adaptive":{"mode":"` + mode + `","minStreams":4,"maxStreams":32},"xmux":{"cMaxReuseTimes":7},"scMinPostsIntervalMs":"5-9"`
			input := `{"mode":"stream-auto","path":"/test",` + options + `}`
			if extra {
				input = `{"mode":"stream-auto","path":"/test","extra":{` + options + `}}`
			}
			var config XeraHTTPConfig
			if err := json.Unmarshal([]byte(input), &config); err != nil {
				t.Fatal(err)
			}
			built, err := config.Build()
			if err != nil {
				t.Fatal(err)
			}
			settings := built.(*xerahttp.Config)
			if settings.Mode != "stream-auto" || settings.Path != "/test" || settings.Adaptive.Mode != mode || settings.Adaptive.MinStreams != 4 || settings.Adaptive.MaxStreams != 32 || settings.Xmux.CMaxReuseTimes.From != 7 || settings.ScMinPostsIntervalMs.From != 5 || settings.ScMinPostsIntervalMs.To != 9 {
				t.Fatalf("adaptive changed effective manual settings: %v", settings)
			}
			wire, err := proto.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			decoded := new(xerahttp.Config)
			if err := proto.Unmarshal(wire, decoded); err != nil || !proto.Equal(settings, decoded) {
				t.Fatalf("adaptive protobuf round trip: %v", err)
			}
		}
	}
}

func TestXeraAdaptivePreservesLegacyAndExtraPrecedence(t *testing.T) {
	for _, input := range []string{
		`{"mode":"stream-auto"}`,
		`{"mode":"stream-auto","adaptive":null}`,
		`{"mode":"stream-auto","adaptive":{"mode":"invalid","minStreams":0},"extra":{"xmux":{"hKeepAlivePeriod":-1}}}`,
	} {
		var config XeraHTTPConfig
		if err := json.Unmarshal([]byte(input), &config); err != nil {
			t.Fatal(err)
		}
		built, err := config.Build()
		if err != nil {
			t.Fatal(err)
		}
		if built.(*xerahttp.Config).Adaptive != nil {
			t.Fatal("legacy or shadowed settings implicitly enabled adaptation")
		}
	}
}

func TestXeraAdaptiveRejectsInvalidBounds(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"mode":"auto","minStreams":4,"maxStreams":32}`,
		`{"mode":"balanced","minStreams":0,"maxStreams":32}`,
		`{"mode":"balanced","minStreams":-1,"maxStreams":4}`,
		`{"mode":"balanced","minStreams":4,"maxStreams":3}`,
		`{"mode":"balanced","minStreams":4,"maxStreams":33}`,
	} {
		var config XeraHTTPConfig
		if err := json.Unmarshal([]byte(`{"adaptive":`+input+`}`), &config); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Build(); err == nil {
			t.Fatalf("accepted invalid adaptive config %s", input)
		}
	}
}

func TestXeraAdaptiveRejectsUnknownFieldsAndWrongTypes(t *testing.T) {
	for _, input := range []string{`{"mode":"balanced","minStreams":4,"maxStreams":32,"maxConnections":8}`, `{"mode":"balanced","minStreams":"4","maxStreams":32}`, `true`, `[]`} {
		var config XeraHTTPConfig
		if err := json.Unmarshal([]byte(`{"adaptive":`+input+`}`), &config); err == nil {
			t.Fatalf("accepted malformed adaptive object: %s", input)
		}
	}
}
