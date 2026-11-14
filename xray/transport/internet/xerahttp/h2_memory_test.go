package xerahttp

import (
	"context"
	"fmt"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttp2 "github.com/bogdanfinn/fhttp/http2"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	"golang.org/x/net/http2"
)

type h2ScratchProbe struct {
	observed chan [2]int
	closed   chan struct{}
	once     sync.Once
}

func (p *h2ScratchProbe) Read(buffer []byte) (int, error) {
	select {
	case p.observed <- [2]int{len(buffer), cap(buffer)}:
	default:
	}
	<-p.closed
	return 0, io.EOF
}

func (p *h2ScratchProbe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func TestH2ClientUploadScratchBuffers(t *testing.T) {
	selected := os.Getenv("REMNACUST_H2_SCRATCH_CASE")
	for _, client := range []struct {
		name   string
		upload func(context.Context, stdnet.Conn, io.ReadCloser) error
	}{
		{name: "x-net", upload: func(ctx context.Context, conn stdnet.Conn, body io.ReadCloser) error {
			client, err := new(http2.Transport).NewClientConn(conn)
			if err != nil {
				return err
			}
			defer client.Close()
			if err := client.Ping(ctx); err != nil {
				return err
			}
			request, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/upload", body)
			if err != nil {
				return err
			}
			response, err := client.RoundTrip(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			return err
		}},
		{name: "fhttp", upload: func(ctx context.Context, conn stdnet.Conn, body io.ReadCloser) error {
			client, err := new(fhttp2.Transport).NewClientConn(conn)
			if err != nil {
				return err
			}
			defer client.Close()
			if err := client.Ping(ctx); err != nil {
				return err
			}
			request, err := fhttp.NewRequestWithContext(ctx, "POST", "http://localhost/upload", body)
			if err != nil {
				return err
			}
			response, err := client.RoundTrip(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			return err
		}},
	} {
		for _, frameSize := range []int{0, 65536} {
			name := fmt.Sprintf("%s/configured=%d", client.name, frameSize)
			if selected != "" && selected != name {
				continue
			}
			t.Run(name, func(t *testing.T) {
				if selected == "" {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestH2ClientUploadScratchBuffers$", "-test.v")
					command.Env = append(os.Environ(), "REMNACUST_H2_SCRATCH_CASE="+name)
					if output, err := command.CombinedOutput(); err != nil {
						t.Fatalf("isolated HTTP/2 scratch regression: %v\n%s", err, output)
					}
					return
				}
				config := &Config{}
				if frameSize != 0 {
					config.H2Params = []byte(fmt.Sprintf(`{"MaxReadFrameSize":%d}`, frameSize))
				}
				parsed, err := config.http2Config()
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					io.Copy(io.Discard, r.Body)
				}))
				server.Config.HTTP2 = parsed
				server.Config.Protocols = new(http.Protocols)
				server.Config.Protocols.SetUnencryptedHTTP2(true)
				server.Start()
				defer server.Close()
				conn, err := stdnet.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				body := &h2ScratchProbe{observed: make(chan [2]int, 1), closed: make(chan struct{})}
				defer body.Close()
				done := make(chan error, 1)
				go func() { done <- client.upload(ctx, conn, body) }()
				select {
				case observed := <-body.observed:
					t.Logf("actual upload Body.Read buffer: len=%d cap=%d", observed[0], observed[1])
					want := frameSize
					if want == 0 {
						want = 16384
					}
					if observed[0] != want {
						t.Errorf("first upload scratch length=%d, want %d", observed[0], want)
					}
				case err := <-done:
					t.Fatalf("upload ended before Body.Read: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				body.Close()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	}
}

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
