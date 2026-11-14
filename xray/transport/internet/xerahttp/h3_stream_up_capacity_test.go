package xerahttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

func TestH3StreamUpCapacityPairedSessions(t *testing.T) {
	const sessions = 300
	for _, address := range []net.Address{net.LocalHostIP, net.LocalHostIPv6} {
		for _, explicitZero := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit-zero=%t", address, explicitZero), func(t *testing.T) {
				if address.Family().IsIPv6() {
					requireH3CapacityIPv6(t)
				}
				config := &Config{Path: "/h3-stream-up-capacity/", Mode: ModeStreamUp, ServerMaxSessions: sessions}
				var params *internet.QuicParams
				if explicitZero {
					params = &internet.QuicParams{Congestion: "reno"}
				}
				fixture := newH3CapacityFixture(t, address, config, params)
				downlinks := fixture.openDownlinks(t, sessions)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				t.Cleanup(cancel)
				uplinks := make([]*io.PipeWriter, 0, sessions)
				for index := range sessions {
					reader, writer := io.Pipe()
					t.Cleanup(func() { reader.Close(); writer.Close() })
					requestCtx, cancelRequest := context.WithCancel(ctx)
					t.Cleanup(cancelRequest)
					request, err := http.NewRequestWithContext(requestCtx, config.GetNormalizedUplinkHTTPMethod(), "https://localhost"+config.GetNormalizedPath(), reader)
					if err != nil {
						t.Fatal(err)
					}
					config.FillStreamRequest(request, fmt.Sprintf("capacity-%03d", index), "")
					timer := time.AfterFunc(2*time.Second, cancelRequest)
					response, err := fixture.client.RoundTrip(request)
					timer.Stop()
					if err != nil {
						t.Fatalf("uplink %d/%d with %d live downlinks on one QUIC connection: %v", index+1, sessions, sessions, err)
					}
					t.Cleanup(func() { response.Body.Close() })
					if response.StatusCode != http.StatusOK || response.ProtoMajor != 3 {
						t.Fatalf("uplink %d returned %s HTTP %d", index, response.Proto, response.StatusCode)
					}
					uplinks = append(uplinks, writer)
				}
				for round := range 2 {
					for index, writer := range uplinks {
						payload := []byte(fmt.Sprintf("paired-session-%03d-round-%d", index, round))
						if _, err := writer.Write(payload); err != nil {
							t.Fatalf("uplink %d: %v", index, err)
						}
						assertH3CapacityEcho(t, downlinks[index], payload)
					}
				}
				if fixture.active.Load() != sessions || fixture.listener.handler.sessionCount.Load() != sessions {
					t.Fatalf("paired sessions did not remain live: active=%d counted=%d", fixture.active.Load(), fixture.listener.handler.sessionCount.Load())
				}
				for index := range 256 {
					if _, err := fixture.connection.OpenStream(); err != nil {
						t.Fatalf("wire capacity missing headroom %d/256 after %d paired streams: %v", index+1, 2*sessions, err)
					}
				}
				stream, err := fixture.connection.OpenStream()
				var limited *quic.StreamLimitReachedError
				if stream != nil || !errors.As(err, &limited) {
					t.Fatalf("wire stream capacity exceeds %d: %v", 2*sessions+256, err)
				}
				t.Logf("%d paired sessions exchanged data with exact wire capacity %d", sessions, 2*sessions+256)
			})
		}
	}
}
