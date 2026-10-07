package xerahttp

import (
	"bytes"
	"context"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

func TestPacketUploadRanges(t *testing.T) {
	for _, interval := range []*RangeConfig{{From: -1, To: -1}, {From: 0, To: 100}} {
		t.Run(strconv.Itoa(int(interval.From)), func(t *testing.T) {
			type post struct {
				seq  int
				data []byte
				at   time.Time
			}
			posts := make(chan post, 128)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				parts := strings.Split(r.URL.Path, "/")
				seq, err := strconv.Atoi(parts[len(parts)-1])
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				posts <- post{seq: seq, data: data, at: time.Now()}
			}))
			defer server.Close()
			config := &Config{Mode: "packet-up", UplinkDataPlacement: PlacementBody, ScMaxEachPostBytes: &RangeConfig{From: 8192, To: 16384}, ScMinPostsIntervalMs: &RangeConfig{From: interval.From, To: interval.To}}
			settings := &internet.MemoryStreamConfig{ProtocolName: "xera-http", ProtocolSettings: config}
			port := net.Port(server.Listener.Addr().(*stdnet.TCPAddr).Port)
			conn, err := Dial(context.Background(), net.TCPDestination(net.LocalHostIP, port), settings)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte("packet-range"), 32768)
			buffers, err := buf.ReadFrom(bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			writer := conn.(*transportConn).writer.(uploadWriter)
			if err := writer.WriteMultiBuffer(buffers); err != nil {
				t.Fatal(err)
			}
			received := make(map[int]post)
			total := 0
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			for total < len(payload) {
				select {
				case p := <-posts:
					if _, exists := received[p.seq]; exists {
						t.Fatal("duplicate sequence")
					}
					received[p.seq] = p
					total += len(p.data)
				case <-deadline.C:
					t.Fatal("upload stalled")
				}
			}
			var joined []byte
			sizes := make(map[int]bool)
			for seq := 0; seq < len(received); seq++ {
				p, exists := received[seq]
				if !exists {
					t.Fatal("missing sequence")
				}
				if len(p.data) > 16384 || (seq < len(received)-1 && len(p.data) < 8192) {
					t.Fatal("packet outside configured range")
				}
				if seq < len(received)-1 {
					sizes[len(p.data)] = true
				}
				joined = append(joined, p.data...)
			}
			if !bytes.Equal(joined, payload) {
				t.Fatal("payload corrupted")
			}
			if len(sizes) < 2 {
				t.Error("POST size range sampled only once for the session")
			}
			if interval.From == 0 && received[len(received)-1].at.Sub(received[0].at) < 100*time.Millisecond {
				t.Error("positive interval range ignored because its lower bound is zero")
			}
		})
	}
}
