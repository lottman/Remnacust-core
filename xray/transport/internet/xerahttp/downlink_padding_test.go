package xerahttp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

type paddingBuffer struct{ bytes.Buffer }

func (b *paddingBuffer) Close() error { return nil }

func testDownlinkConfig() *DownlinkPaddingConfig {
	return &DownlinkPaddingConfig{Header: "X-Test-Profile", Token: "0123456789abcdef", Bytes: &RangeConfig{From: 16, To: 128}, BlockBytes: &RangeConfig{From: 1024, To: 8192}}
}

func TestPaddedDownlinkRoundTrip(t *testing.T) {
	var wire paddingBuffer
	w := &paddedDownlinkWriter{WriteCloser: &wire, config: testDownlinkConfig()}
	payload := bytes.Repeat([]byte("downlink-padding"), 8192)
	for _, data := range [][]byte{nil, payload[:1], payload[1:68], payload[68:]} {
		if n, err := w.Write(data); n != len(data) || err != nil {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if wire.Len() <= len(payload) {
		t.Fatal("padding missing")
	}
	encoded := append([]byte(nil), wire.Bytes()...)
	for len(encoded) > 0 {
		n, pad := int(binary.BigEndian.Uint16(encoded)), int(binary.BigEndian.Uint16(encoded[2:]))
		if n < 1 || n > 8192 || pad < 16 || pad > 128 {
			t.Fatal("invalid generated frame")
		}
		encoded = encoded[4+n+pad:]
	}
	r := &paddedDownlinkReader{ReadCloser: io.NopCloser(iotest.OneByteReader(&wire))}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestPaddedDownlinkRejectsMalformedFrames(t *testing.T) {
	for _, input := range [][]byte{{0}, {0, 0, 0, 0}, {255, 255, 0, 0}, {0, 1, 255, 255}, {0, 1, 0, 0}, {0, 1, 0, 1, 42}} {
		r := &paddedDownlinkReader{ReadCloser: io.NopCloser(bytes.NewReader(input))}
		if _, err := io.ReadAll(r); err == nil {
			t.Fatalf("accepted malformed frame %x", input)
		}
		if _, err := r.Read(make([]byte, 1)); err == nil {
			t.Fatal("parse failure was not sticky")
		}
	}
}

type shortPaddingWriter struct{ calls int }

func (w *shortPaddingWriter) Close() error                { return nil }
func (w *shortPaddingWriter) Write(p []byte) (int, error) { w.calls++; return 2, nil }

func TestPaddedDownlinkShortWrite(t *testing.T) {
	sink := &shortPaddingWriter{}
	w := &paddedDownlinkWriter{WriteCloser: sink, config: testDownlinkConfig()}
	if _, err := w.Write([]byte("test")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("test")); n != 0 || !errors.Is(err, io.ErrShortWrite) || sink.calls != 1 {
		t.Fatal("resumed broken frame")
	}
}

func TestPaddedDownlinkCloseInterruptsRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	r := &paddedDownlinkReader{ReadCloser: reader}
	done := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
	if _, err := writer.Write([]byte{0, 1}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed reader returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("close left the decoder blocked")
	}
}

func TestPaddedDownlinkReturnsAvailableData(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	r := &paddedDownlinkReader{ReadCloser: reader}
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		data := make([]byte, 8)
		n, err := r.Read(data)
		if err == nil && (n != 1 || data[0] != 42) {
			err = errors.New("available data was not returned")
		}
		done <- err
	}()
	if _, err := writer.Write([]byte{0, 8, 0, 0, 42}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("decoder waited for the rest of the frame")
	}
}

func TestPaddedDownlinkConcurrentWrites(t *testing.T) {
	var wire paddingBuffer
	w := &paddedDownlinkWriter{WriteCloser: &wire, config: testDownlinkConfig()}
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := w.Write(bytes.Repeat([]byte{42}, 200)); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	r := &paddedDownlinkReader{ReadCloser: io.NopCloser(&wire)}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{42}, 6400)) {
		t.Fatalf("concurrent frame corruption: %v", err)
	}
}

func TestCustomDownlinkRequiresAcknowledgement(t *testing.T) {
	for _, acknowledge := range []bool{false, true} {
		p := testDownlinkConfig()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(p.Header) != p.wireValue() {
				t.Error("request negotiation missing")
			}
			if acknowledge {
				w.Header().Set(p.Header, p.wireValue())
				var wire paddingBuffer
				encoder := &paddedDownlinkWriter{WriteCloser: &wire, config: p}
				encoder.Write([]byte("test"))
				w.Write(wire.Bytes())
			} else {
				w.Write([]byte("unframed"))
			}
		}))
		client := &DefaultDialerClient{client: server.Client(), transportConfig: &Config{CustomDownlinkPadding: p}}
		reader, _, _, err := client.OpenStream(context.Background(), server.URL, "session", nil, false)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		server.Close()
		if acknowledge {
			if err != nil || string(data) != "test" {
				t.Fatalf("negotiation failed: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "did not confirm") || len(data) != 0 {
			t.Fatalf("unsafe fallback: %q %v", data, err)
		}
	}
}

func FuzzPaddedDownlinkReader(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 42, 0})
	f.Add([]byte{255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		r := &paddedDownlinkReader{ReadCloser: io.NopCloser(bytes.NewReader(data))}
		io.Copy(io.Discard, r)
	})
}

type paddingCounter struct{ bytes int64 }

func (w *paddingCounter) Close() error { return nil }
func (w *paddingCounter) Write(data []byte) (int, error) {
	w.bytes += int64(len(data))
	return len(data), nil
}

func BenchmarkPaddedDownlinkEncode(b *testing.B) {
	for _, size := range []int{68, 8192, 65536} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			sink := &paddingCounter{}
			config := testDownlinkConfig()
			config.Bytes = &RangeConfig{From: 24, To: 192}
			config.BlockBytes = &RangeConfig{From: 4096, To: 8192}
			w := &paddedDownlinkWriter{WriteCloser: sink, config: config}
			payload := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := w.Write(payload); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(100*(float64(sink.bytes)/float64(b.N*size)-1), "overhead_pct")
		})
	}
}
