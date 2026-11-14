package splithttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/browser_dialer"
	"github.com/xtls/xray-core/transport/internet/reality"
)

func stockBrowserEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv(platform.BrowserDialerAddress, address)
	browser_dialer.Reload()
	t.Cleanup(func() {
		t.Setenv(platform.BrowserDialerAddress, "")
		browser_dialer.Reload()
	})
	client := &http.Client{Transport: &http.Transport{}, Timeout: 250 * time.Millisecond}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/")
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			match := regexp.MustCompile(`/websocket\?token=([a-f0-9-]+)`).FindSubmatch(data)
			if response.StatusCode != http.StatusOK || len(match) != 2 {
				t.Fatal("local browser capability page unavailable")
			}
			return "ws://" + address + "/websocket?token=" + string(match[1])
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stockBrowserPeer(t *testing.T, endpoint string, serve func(*websocket.Conn) error) <-chan error {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	peer, response, err := dialer.Dial(endpoint, nil)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	peer.SetWriteDeadline(time.Now().Add(5 * time.Second))
	result := make(chan error, 1)
	go func() {
		defer peer.Close()
		result <- serve(peer)
	}()
	return result
}

func stockBrowserAwait(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("browser operation did not finish")
		return nil
	}
}

func TestStockBrowserDialerDialGuards(t *testing.T) {
	for _, separateDownload := range []bool{false, true} {
		name := "upload-browser"
		if separateDownload {
			name = "download-browser"
		}
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("enabled Browser Dialer panicked in Dial: %v", p)
				}
			}()
			endpoint := stockBrowserEndpoint(t)
			config := &Config{Mode: "packet-up", Path: "/upload"}
			settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
			t.Cleanup(func() { settings.Close() })
			path := "/upload/"
			if separateDownload {
				settings.SecurityType = "reality"
				settings.SecuritySettings = &reality.Config{}
				config.DownloadSettings = &internet.StreamConfig{
					Address: xnet.NewIPOrDomain(xnet.LocalHostIP), Port: 1, ProtocolName: protocolName,
					TransportSettings: []*internet.TransportConfig{{ProtocolName: protocolName, Settings: serial.ToTypedMessage(&Config{Path: "/download"})}},
				}
				path = "/download/"
			}
			get := stockBrowserPeer(t, endpoint, func(peer *websocket.Conn) error {
				var task struct {
					Method, URL    string
					StreamResponse bool
				}
				if err := peer.ReadJSON(&task); err != nil {
					return err
				}
				target, err := url.Parse(task.URL)
				if err != nil || task.Method != "GET" || !task.StreamResponse || !strings.HasPrefix(target.Path, path) {
					return fmt.Errorf("unexpected download task: %+v", task)
				}
				if err := peer.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
					return err
				}
				return peer.WriteMessage(websocket.BinaryMessage, []byte("download"))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
			conn, err := Dial(ctx, dest, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			data := make([]byte, len("download"))
			if _, err := io.ReadFull(conn, data); err != nil || string(data) != "download" {
				t.Fatalf("download payload=%q error=%v", data, err)
			}
			if err := stockBrowserAwait(t, get); err != nil {
				t.Fatal(err)
			}
			globalDialerAccess.Lock()
			_, nativeMain := globalDialerMap[dialerConf{dest, settings}]
			globalDialerAccess.Unlock()
			if nativeMain != separateDownload {
				t.Fatal("test did not select the intended upload/download guard path")
			}
			if !separateDownload {
				post := stockBrowserPeer(t, endpoint, func(peer *websocket.Conn) error {
					var task struct{ Method string }
					if err := peer.ReadJSON(&task); err != nil {
						return err
					}
					if task.Method != "POST" {
						return fmt.Errorf("unexpected upload method: %s", task.Method)
					}
					if err := peer.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
						return err
					}
					kind, payload, err := peer.ReadMessage()
					if err != nil || kind != websocket.BinaryMessage || string(payload) != "upload" {
						return fmt.Errorf("upload payload=%q type=%d error=%v", payload, kind, err)
					}
					return peer.WriteMessage(websocket.TextMessage, []byte("ok"))
				})
				if _, err := conn.Write([]byte("upload")); err != nil {
					t.Fatal(err)
				}
				if err := stockBrowserAwait(t, post); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestStockBrowserDialerPreflightPayload(t *testing.T) {
	client := &BrowserDialerClient{transportConfig: &Config{}}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		payload := buf.New()
		payload.Write([]byte("payload"))
		err := client.PostPacket(ctx, "://bad", "session", "0", buf.MultiBuffer{payload})
		cancel()
		if !payload.IsEmpty() {
			payload.Release()
			t.Fatal("payload survived failed browser preflight")
		}
		if err == nil || canceled && !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled=%t error=%v", canceled, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := client.OpenStream(ctx, "://bad", "", nil, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream preflight lost cancellation: %v", err)
	}
}

func TestStockBrowserDialerWaitingCancellation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "no-browser"
		if enabled {
			name = "empty-browser-queue"
		}
		t.Run(name, func(t *testing.T) {
			if enabled {
				stockBrowserEndpoint(t)
			} else {
				t.Setenv(platform.BrowserDialerAddress, "")
				browser_dialer.Reload()
			}
			client := &BrowserDialerClient{transportConfig: &Config{}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, _, _, err := client.OpenStream(ctx, "http://127.0.0.1:1/", "", nil, false); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("stream queue wait lost deadline: %v", err)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			payload := buf.New()
			payload.Write([]byte("payload"))
			err := client.PostPacket(ctx, "http://127.0.0.1:1/", "session", "0", buf.MultiBuffer{payload})
			if !payload.IsEmpty() {
				payload.Release()
				t.Fatal("payload survived canceled browser queue wait")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("packet queue wait lost deadline: %v", err)
			}
		})
	}
}

func TestStockBrowserDialerPostCancellationAfterDispatch(t *testing.T) {
	endpoint := stockBrowserEndpoint(t)
	client := &BrowserDialerClient{transportConfig: &Config{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peerResult := stockBrowserPeer(t, endpoint, func(peer *websocket.Conn) error {
		var task struct{ Method string }
		if err := peer.ReadJSON(&task); err != nil {
			return err
		}
		if task.Method != "POST" {
			return fmt.Errorf("unexpected task: %+v", task)
		}
		if err := peer.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			return err
		}
		_, payload, err := peer.ReadMessage()
		if err != nil || string(payload) != "payload" {
			return fmt.Errorf("dispatched payload=%q error=%v", payload, err)
		}
		cancel()
		if _, _, err := peer.ReadMessage(); err == nil {
			return errors.New("canceled POST retained its helper socket")
		}
		return nil
	})
	payload := buf.New()
	payload.Write([]byte("payload"))
	result := make(chan error, 1)
	go func() {
		result <- client.PostPacket(ctx, "http://127.0.0.1:1/", "session", "0", buf.MultiBuffer{payload})
	}()
	if err := stockBrowserAwait(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("POST did not return cancellation: %v", err)
	}
	if !payload.IsEmpty() {
		payload.Release()
		t.Fatal("payload survived canceled dispatched POST")
	}
	if err := stockBrowserAwait(t, peerResult); err != nil {
		t.Fatal(err)
	}
}
