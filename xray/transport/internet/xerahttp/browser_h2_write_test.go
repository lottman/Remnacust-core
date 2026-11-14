package xerahttp

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xtls/xray-core/common/protocol/tls/cert"
)

type browserH2WriteProbe struct {
	net.Conn
	mu      sync.Mutex
	writes  [][]byte
	started chan []byte
	closes  atomic.Int32
	write   func([]byte) (int, error)
}

type browserH2WriteReadProbe struct {
	net.Conn
	read func([]byte) (int, error)
}

func (c *browserH2WriteReadProbe) Read(data []byte) (int, error) { return c.read(data) }

type browserH2WriteCloseProbe struct {
	net.Conn
	mu        sync.Mutex
	started   chan struct{}
	abort     chan struct{}
	startOnce sync.Once
	abortOnce sync.Once
	block     bool
	expired   bool
	closes    atomic.Int32
}

func (c *browserH2WriteCloseProbe) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.expired = !deadline.IsZero() && !time.Now().Before(deadline)
	if c.expired {
		c.abortOnce.Do(func() { close(c.abort) })
	}
	c.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

func (c *browserH2WriteCloseProbe) Close() error {
	c.closes.Add(1)
	c.startOnce.Do(func() { close(c.started) })
	if c.block {
		<-c.abort
	}
	return c.Conn.Close()
}

func TestBrowserH2WriteReadPreservesBytesAfterOppositeWriteError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		entered, release := make(chan struct{}), make(chan struct{})
		want := errors.New("write failed after read started")
		readProbe := &browserH2WriteReadProbe{Conn: raw, read: func(data []byte) (int, error) {
			close(entered)
			<-release
			return copy(data, "reply"), io.EOF
		}}
		probe := &browserH2WriteProbe{Conn: readProbe, write: func([]byte) (int, error) { return 0, want }}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		read := make(chan error, 1)
		go func() {
			data := make([]byte, 8)
			n, err := conn.Read(data)
			if n != 5 || string(data[:n]) != "reply" || !errors.Is(err, want) {
				read <- fmt.Errorf("read lost received bytes: n=%d data=%q err=%v", n, data[:n], err)
				return
			}
			read <- nil
		}()
		<-entered
		conn.Write([]byte("request"))
		<-conn.done
		close(release)
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WriteHealthyCloseDoesNotExpireDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteCloseProbe{Conn: raw, started: make(chan struct{}), abort: make(chan struct{})}
		conn := newBrowserH2WriteConn(probe)
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		probe.mu.Lock()
		expired := probe.expired
		probe.mu.Unlock()
		if expired {
			t.Fatal("healthy close preemptively expired deadlines")
		}
	})
}

func TestBrowserH2WriteForcedCloseInterruptsAlreadyRunningClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteCloseProbe{Conn: raw, started: make(chan struct{}), abort: make(chan struct{}), block: true}
		conn := newBrowserH2WriteConn(probe)
		result := make(chan error, 1)
		start := time.Now()
		go func() { result <- conn.Close() }()
		<-probe.started
		if err := conn.SetWriteDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
			t.Fatal("closing deadline was cleared", err)
		}
		if err := <-result; !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != browserH2WriteCloseTimeout {
			t.Fatalf("forced close took %v", elapsed)
		}
		for _, set := range []func(time.Time) error{conn.SetDeadline, conn.SetWriteDeadline, conn.SetReadDeadline} {
			if err := set(time.Time{}); !errors.Is(err, net.ErrClosed) {
				t.Fatal("forced deadline was cleared", err)
			}
		}
		if probe.closes.Load() != 1 {
			t.Fatal("raw Close was not idempotent")
		}
	})
}

func TestBrowserH2WriteTLSCloseNotifyAndForcedClose(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				certificate, _ := cert.MustGenerate(nil, cert.CommonName("localhost"), cert.DNSNames("localhost"))
				pem, key := certificate.ToPEM()
				pair, err := tls.X509KeyPair(pem, key)
				if err != nil {
					t.Fatal(err)
				}
				roots := x509.NewCertPool()
				roots.AppendCertsFromPEM(pem)
				raw, peer := net.Pipe()
				defer peer.Close()
				probe := &browserH2WriteProbe{Conn: raw}
				client := tls.Client(probe, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
				server := tls.Server(peer, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
				handshake := make(chan error, 1)
				go func() { handshake <- server.Handshake() }()
				if err := client.Handshake(); err != nil {
					t.Fatal(err)
				}
				if err := <-handshake; err != nil {
					t.Fatal(err)
				}
				conn := newBrowserH2WriteConn(client)
				read := make(chan error, 1)
				if !blocked {
					go func() { _, err := server.Read(make([]byte, 1)); read <- err }()
				}
				start := time.Now()
				err = conn.Close()
				if blocked {
					if !errors.Is(err, net.ErrClosed) {
						t.Fatal(err)
					}
					if elapsed := time.Since(start); elapsed != browserH2WriteCloseTimeout {
						t.Fatalf("blocked TLS close took %v", elapsed)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if err := <-read; err != io.EOF {
						t.Fatal(err)
					}
					probe.mu.Lock()
					last := bytes.Clone(probe.writes[len(probe.writes)-1])
					probe.mu.Unlock()
					if len(last) == 0 || last[0] != 21 {
						t.Fatal("TLS close_notify alert was not written")
					}
				}
			})
		})
	}
}

func (c *browserH2WriteProbe) Write(data []byte) (int, error) {
	copyOfData := bytes.Clone(data)
	c.mu.Lock()
	c.writes = append(c.writes, copyOfData)
	c.mu.Unlock()
	if c.started != nil {
		c.started <- copyOfData
	}
	if c.write != nil {
		return c.write(data)
	}
	return c.Conn.Write(data)
}

func (c *browserH2WriteProbe) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func TestBrowserH2WriteCoalescesReadyBytesAndCopiesInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 4)}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		if _, err := conn.Write([]byte("first")); err != nil {
			t.Fatal(err)
		}
		if got := <-probe.started; string(got) != "first" {
			t.Fatal(string(got))
		}
		for _, value := range []string{"two", "three", "four"} {
			data := []byte(value)
			if _, err := conn.Write(data); err != nil {
				t.Fatal(err)
			}
			clear(data)
		}
		first := make([]byte, 5)
		if _, err := io.ReadFull(peer, first); err != nil {
			t.Fatal(err)
		}
		if got := <-probe.started; string(got) != "twothreefour" {
			t.Fatalf("pending writes not coalesced: %q", got)
		}
		rest := make([]byte, len("twothreefour"))
		if _, err := io.ReadFull(peer, rest); err != nil || string(rest) != "twothreefour" {
			t.Fatal(string(rest), err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if probe.closes.Load() != 1 {
			t.Fatal("raw close was not idempotent")
		}
	})
}

func TestBrowserH2WriteSmallInteractiveHasNoBatchDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 1)}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		start := time.Now()
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if got := <-probe.started; string(got) != "ping" {
			t.Fatal(string(got))
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("interactive write waited %v for batching", elapsed)
		}
		if _, err := io.ReadFull(peer, make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WriteConcurrentCallsDoNotInterleave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		const writers = 12
		const size = browserH2WriteBatchBytes + 137
		results := make(chan error, writers)
		for i := range writers {
			go func() {
				n, err := conn.Write(bytes.Repeat([]byte{byte(i + 1)}, size))
				if err == nil && n != size {
					err = io.ErrShortWrite
				}
				results <- err
			}()
		}
		all := make([]byte, writers*size)
		if _, err := io.ReadFull(peer, all); err != nil {
			t.Fatal(err)
		}
		seen := make(map[byte]bool)
		for i := range writers {
			part := all[i*size : (i+1)*size]
			if seen[part[0]] || !bytes.Equal(part, bytes.Repeat(part[:1], size)) {
				t.Fatal("full Write calls interleaved")
			}
			seen[part[0]] = true
		}
		for range writers {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		for _, write := range probe.writes {
			if len(write) > browserH2WriteBatchBytes {
				t.Fatalf("raw batch too large: %d", len(write))
			}
		}
	})
}

func TestBrowserH2WriteBackpressureBoundsMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 1)}
		conn := newBrowserH2WriteConn(probe)
		if _, err := conn.Write(bytes.Repeat([]byte("a"), browserH2WriteBatchBytes)); err != nil {
			t.Fatal(err)
		}
		<-probe.started
		if _, err := conn.Write(bytes.Repeat([]byte("b"), browserH2WriteBatchBytes)); err != nil {
			t.Fatal(err)
		}
		blocked := make(chan error, 1)
		go func() { _, err := conn.Write(bytes.Repeat([]byte("c"), browserH2WriteBatchBytes)); blocked <- err }()
		synctest.Wait()
		select {
		case <-blocked:
			t.Fatal("producer escaped full queue")
		default:
		}
		conn.mu.Lock()
		pending, active, capacity := len(conn.pending), conn.active, cap(conn.pending)
		conn.mu.Unlock()
		if pending != browserH2WriteBatchBytes || active != browserH2WriteBatchBytes || capacity != browserH2WriteBatchBytes {
			t.Fatalf("unbounded buffers: pending=%d active=%d cap=%d", pending, active, capacity)
		}
		start := time.Now()
		if err := conn.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != browserH2WriteCloseTimeout {
			t.Fatalf("close drain took %v", elapsed)
		}
		if err := <-blocked; !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		select {
		case <-conn.done:
		default:
			t.Fatal("write worker survived close")
		}
		conn.mu.Lock()
		pending, active = len(conn.pending), conn.active
		conn.mu.Unlock()
		if pending != 0 || active != 0 {
			t.Fatal("buffers remained active after close")
		}
		if _, err := conn.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WriteDeadlineWakesQueueAndSerializedWriters(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				raw, peer := net.Pipe()
				defer peer.Close()
				probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 1)}
				conn := newBrowserH2WriteConn(probe)
				defer conn.Close()
				conn.Write([]byte("active"))
				<-probe.started
				conn.Write(make([]byte, browserH2WriteBatchBytes))
				results := make(chan error, 2)
				for range 2 {
					go func() { _, err := conn.Write([]byte("waiting")); results <- err }()
				}
				read := make(chan error, 1)
				go func() { _, err := conn.Read(make([]byte, 1)); read <- err }()
				synctest.Wait()
				deadline := time.Now().Add(time.Second)
				var err error
				if all {
					err = conn.SetDeadline(deadline)
				} else {
					err = conn.SetWriteDeadline(deadline)
				}
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := <-results; !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatal(err)
					}
				}
				if err := <-read; !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatal(err)
				}
				<-conn.done
				if _, err := conn.Write([]byte("late")); !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatal("write error was not sticky", err)
				}
			})
		})
	}
}

func TestBrowserH2WriteClearingDeadlineWakesBlockedProducer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 3)}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		conn.Write([]byte("a"))
		<-probe.started
		conn.Write(make([]byte, browserH2WriteBatchBytes))
		result := make(chan error, 1)
		go func() { _, err := conn.Write([]byte("z")); result <- err }()
		synctest.Wait()
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		select {
		case err := <-result:
			t.Fatal("cleared deadline ended blocked write", err)
		default:
		}
		if _, err := io.ReadFull(peer, make([]byte, browserH2WriteBatchBytes+2)); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WritePartialAndZeroResults(t *testing.T) {
	for _, scenario := range []string{"partial", "error", "zero", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				raw, peer := net.Pipe()
				defer peer.Close()
				want := errors.New("raw write failed")
				probe := &browserH2WriteProbe{Conn: raw}
				switch scenario {
				case "partial":
					probe.write = func(data []byte) (int, error) { return raw.Write(data[:min(3, len(data))]) }
				case "error":
					probe.write = func([]byte) (int, error) { return 2, want }
				case "zero":
					want = io.ErrNoProgress
					probe.write = func([]byte) (int, error) { return 0, nil }
				case "invalid":
					want = io.ErrShortWrite
					probe.write = func(data []byte) (int, error) { return len(data) + 1, nil }
				}
				conn := newBrowserH2WriteConn(probe)
				defer conn.Close()
				conn.Write([]byte("payload"))
				if scenario == "partial" {
					got := make([]byte, 7)
					if _, err := io.ReadFull(peer, got); err != nil || string(got) != "payload" {
						t.Fatal(string(got), err)
					}
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					return
				}
				<-conn.done
				if _, err := conn.Write([]byte("late")); !errors.Is(err, want) {
					t.Fatal("write failure lost", err)
				}
				if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, want) {
					t.Fatal("read failure lost", err)
				}
				if err := conn.Close(); !errors.Is(err, want) {
					t.Fatal("close failure lost", err)
				}
				if probe.closes.Load() != 1 {
					t.Fatal("raw connection not closed exactly once")
				}
			})
		})
	}
}

func TestBrowserH2WriteReadProgressWithSmallPeerWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		conn := newBrowserH2WriteConn(raw)
		defer conn.Close()
		payload := bytes.Repeat([]byte("data"), 32768)
		written := make(chan error, 1)
		go func() {
			n, err := conn.Write(payload)
			if n != len(payload) && err == nil {
				err = io.ErrShortWrite
			}
			written <- err
		}()
		peerDone := make(chan error, 1)
		go func() {
			var got bytes.Buffer
			for got.Len() < len(payload) {
				part := make([]byte, min(512, len(payload)-got.Len()))
				if _, err := io.ReadFull(peer, part); err != nil {
					peerDone <- err
					return
				}
				got.Write(part)
				if _, err := peer.Write([]byte("w")); err != nil {
					peerDone <- err
					return
				}
			}
			if !bytes.Equal(got.Bytes(), payload) {
				peerDone <- errors.New("payload differs")
				return
			}
			peerDone <- nil
		}()
		for range len(payload) / 512 {
			if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if err := <-peerDone; err != nil {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WriteCloseDrainsBeforeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 3)}
		conn := newBrowserH2WriteConn(probe)
		conn.Write([]byte("first"))
		<-probe.started
		conn.Write([]byte("second"))
		closed := make(chan error, 1)
		go func() { closed <- conn.Close() }()
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		got := make([]byte, len("firstsecond"))
		if _, err := io.ReadFull(peer, got); err != nil || string(got) != "firstsecond" {
			t.Fatal(string(got), err)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		if probe.closes.Load() != 1 {
			t.Fatal("raw close was not idempotent")
		}
		select {
		case <-conn.done:
		default:
			t.Fatal("worker survived close")
		}
	})
}

func TestBrowserH2WriteFailureUnblocksProducersAndRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		want := errors.New("partial TLS write failed")
		release := make(chan struct{})
		probe := &browserH2WriteProbe{Conn: raw, started: make(chan []byte, 1)}
		probe.write = func(data []byte) (int, error) {
			<-release
			return min(2, len(data)), want
		}
		conn := newBrowserH2WriteConn(probe)
		defer conn.Close()
		if _, err := conn.Write([]byte("active")); err != nil {
			t.Fatal(err)
		}
		<-probe.started
		if _, err := conn.Write(make([]byte, browserH2WriteBatchBytes)); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 3)
		for range 2 {
			go func() { _, err := conn.Write([]byte("waiting")); results <- err }()
		}
		go func() { _, err := conn.Read(make([]byte, 1)); results <- err }()
		synctest.Wait()
		close(release)
		for range 3 {
			if err := <-results; !errors.Is(err, want) {
				t.Fatal("actual write failure was masked", err)
			}
		}
		<-conn.done
		if err := conn.Close(); !errors.Is(err, want) {
			t.Fatal(err)
		}
	})
}

func TestBrowserH2WriteExpiredQueueDeadlineCanBeCleared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw, peer := net.Pipe()
		defer peer.Close()
		conn := newBrowserH2WriteConn(raw)
		defer conn.Close()
		if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		if n, err := conn.Write([]byte("expired")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(n, err)
		}
		if err := conn.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("valid")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(peer, got); err != nil || string(got) != "valid" {
			t.Fatal(string(got), err)
		}
	})
}

func TestBrowserH2WriteConcurrentCloseStopsIdleWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 32 {
			raw, peer := net.Pipe()
			probe := &browserH2WriteProbe{Conn: raw}
			conn := newBrowserH2WriteConn(probe)
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			workers.Wait()
			peer.Close()
			if probe.closes.Load() != 1 {
				t.Fatal("raw close was not idempotent")
			}
			select {
			case <-conn.done:
			default:
				t.Fatal("idle worker survived Close")
			}
		}
	})
}
