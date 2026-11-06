package xerahttp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

type browserH2GuardTestConn struct {
	net.Conn
	reader   io.Reader
	closed   bool
	deadline time.Time
}

func (c *browserH2GuardTestConn) Read(data []byte) (int, error) { return c.reader.Read(data) }
func (c *browserH2GuardTestConn) Close() error                  { c.closed = true; return nil }
func (c *browserH2GuardTestConn) SetReadDeadline(deadline time.Time) error {
	c.deadline = deadline
	return nil
}

func browserH2TestFrame(kind, flags byte, stream uint32, payload []byte) []byte {
	frame := make([]byte, 9+len(payload))
	frame[0], frame[1], frame[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	frame[3], frame[4] = kind, flags
	binary.BigEndian.PutUint32(frame[5:], stream)
	copy(frame[9:], payload)
	return frame
}

func TestBrowserH2GuardPreservesFragmentedFrames(t *testing.T) {
	wire := browserH2TestFrame(4, 0, 0, nil)
	wire = append(wire, browserH2TestFrame(1, 0, 1, []byte{0x88})...)
	wire = append(wire, browserH2TestFrame(9, 4, 1, []byte{0x5c, 1, '7'})...)
	wire = append(wire, browserH2TestFrame(0, 1, 1, []byte("payload"))...)
	wire = append(wire, browserH2TestFrame(6, 1, 0, make([]byte, 8))...)
	for _, fragmented := range []bool{false, true} {
		var source io.Reader = bytes.NewReader(wire)
		if fragmented {
			source = iotest.OneByteReader(source)
		}
		conn := &browserH2GuardTestConn{reader: source}
		guard := newBrowserH2Guard(conn)
		got, err := io.ReadAll(guard)
		if err != nil || !bytes.Equal(got, wire) || !conn.deadline.IsZero() {
			t.Fatal("guard altered valid frames or retained a header deadline", err)
		}
	}
}

func TestBrowserH2GuardRejectsFloodAndInvalidSequences(t *testing.T) {
	header := browserH2TestFrame(1, 0, 1, []byte{0x88})
	flood := append([]byte(nil), header...)
	for range browserH2MaxHeaderFrames {
		flood = append(flood, browserH2TestFrame(9, 0, 1, nil)...)
	}
	oversized := browserH2TestFrame(0, 0, 1, make([]byte, browserH2MaxFrameBytes+1))
	tests := []struct {
		name    string
		wire    []byte
		message string
	}{
		{"flood", flood, "budget"},
		{"oversized", oversized, "frame size"},
		{"interleaved", append(append([]byte(nil), header...), browserH2TestFrame(0, 0, 1, nil)...), "interleaved"},
		{"wrong stream", append(append([]byte(nil), header...), browserH2TestFrame(9, 4, 3, nil)...), "interleaved"},
		{"unexpected continuation", browserH2TestFrame(9, 4, 1, nil), "unexpected"},
		{"stream zero", browserH2TestFrame(1, 4, 0, nil), "stream zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := &browserH2GuardTestConn{reader: bytes.NewReader(test.wire)}
			_, err := io.Copy(io.Discard, newBrowserH2Guard(conn))
			if err == nil || !strings.Contains(err.Error(), test.message) || !conn.closed {
				t.Fatal("unsafe frame sequence was not rejected and closed", err)
			}
		})
	}
}

func TestBrowserH2GuardBoundsIncompleteHeaderTime(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	release := make(chan struct{})
	defer close(release)
	go func() { peer.Write(browserH2TestFrame(1, 0, 1, []byte{0x88})); <-release }()
	guard := newBrowserH2Guard(client)
	guard.headerTimeout = 50 * time.Millisecond
	_, err := io.Copy(io.Discard, guard)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("unfinished header block did not time out", err)
	}
}

func FuzzBrowserH2Guard(f *testing.F) {
	f.Add(browserH2TestFrame(1, 4, 1, []byte{0x88}))
	f.Add(browserH2TestFrame(9, 0, 1, nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		conn := &browserH2GuardTestConn{reader: bytes.NewReader(data)}
		io.Copy(io.Discard, newBrowserH2Guard(conn))
	})
}
