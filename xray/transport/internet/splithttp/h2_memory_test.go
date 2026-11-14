package splithttp

import (
	"context"
	"io"
	stdnet "net"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"golang.org/x/net/http2"
)

func TestH2ServerFrameSizeSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
		frame  uint32
	}{
		{name: "absent", frame: 16384},
		{name: "empty-object", params: `{}`, frame: 16384},
		{name: "zero", params: `{"MaxReadFrameSize":0}`, frame: 16384},
		{name: "other-parameters", params: `{"MaxConcurrentStreams":128,"MaxDecoderHeaderTableSize":8192}`, frame: 16384},
		{name: "explicit", params: `{"MaxReadFrameSize":65536,"MaxConcurrentStreams":128,"MaxDecoderHeaderTableSize":8192}`, frame: 65536},
		{name: "null", params: `null`, frame: 16384},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{H2Params: []byte(test.params)}}
			t.Cleanup(func() { settings.Close() })
			listener, err := ListenXH(context.Background(), net.LocalHostIP, tcp.PickPort(), settings, func(conn stat.Connection) { conn.Close() })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			conn, err := stdnet.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
				t.Fatal(err)
			}
			framer := http2.NewFramer(conn, conn)
			if err := framer.WriteSettings(); err != nil {
				t.Fatal(err)
			}
			frame, err := framer.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			advertised, ok := frame.(*http2.SettingsFrame)
			if !ok || advertised.IsAck() {
				t.Fatalf("first server frame: %T", frame)
			}
			value, present := advertised.Value(http2.SettingMaxFrameSize)
			if !present || value != test.frame {
				t.Errorf("wire SETTINGS_MAX_FRAME_SIZE=%d, present=%t, want %d", value, present, test.frame)
			}
			if test.name == "other-parameters" || test.name == "explicit" {
				for id, want := range map[http2.SettingID]uint32{http2.SettingMaxConcurrentStreams: 128, http2.SettingHeaderTableSize: 8192} {
					if value, present := advertised.Value(id); !present || value != want {
						t.Errorf("wire %s=%d, present=%t, want %d", id, value, present, want)
					}
				}
			}
		})
	}
}

func TestInvalidH2ParametersReleaseListener(t *testing.T) {
	for _, params := range []string{`{`, `{"MaxReadFrameSize":"bad"}`} {
		t.Run(params, func(t *testing.T) {
			port := tcp.PickPort()
			settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{H2Params: []byte(params)}}
			t.Cleanup(func() { settings.Close() })
			listener, err := ListenXH(context.Background(), net.LocalHostIP, port, settings, func(conn stat.Connection) { conn.Close() })
			if listener != nil {
				listener.Close()
			}
			if err == nil {
				t.Fatal("invalid HTTP2Config accepted")
			}
			rebound, err := stdnet.Listen("tcp", stdnet.JoinHostPort("127.0.0.1", port.String()))
			if err != nil {
				t.Fatalf("failed HTTP2Config retained TCP listener: %v", err)
			}
			rebound.Close()
		})
	}
}
