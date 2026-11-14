package encoding_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/proxy/vless/encoding"
)

func TestLengthPacketReaderAbortedPayloadReleasesBuffers(t *testing.T) {
	if os.Getenv("REMNACUST_PACKET_RELEASE_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLengthPacketReaderAbortedPayloadReleasesBuffers$", "-test.v")
		command.Env = append(os.Environ(), "REMNACUST_PACKET_RELEASE_CHILD=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated packet release regression: %v\n%s", err, output)
		}
		return
	}
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	measure := func(t *testing.T, data []byte) uint64 {
		t.Helper()
		decode := func() {
			mb, err := encoding.NewLengthPacketReader(bytes.NewReader(data)).ReadMultiBuffer()
			if err == nil || len(mb) != 0 {
				buf.ReleaseMulti(mb)
				t.Fatal("aborted packet returned payload or no error")
			}
		}
		for range 32 {
			decode()
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 512 {
			decode()
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / 512
	}
	control := measure(t, []byte{0})
	for _, test := range []struct {
		name   string
		data   []byte
		budget uint64
	}{
		{name: "current-buffer", data: []byte{0, 1}, budget: buf.Size / 2},
		{name: "accumulated-buffers", data: append([]byte{255, 255}, bytes.Repeat([]byte{42}, 65534)...), budget: 4 * buf.Size},
	} {
		t.Run(test.name, func(t *testing.T) {
			allocated := measure(t, test.data)
			t.Logf("allocated bytes per abort: %d; prefix-error control: %d", allocated, control)
			if allocated > control+test.budget {
				t.Fatalf("aborted payload did not recycle buffers: bytes=%d control=%d allowance=%d", allocated, control, test.budget)
			}
		})
	}
}

func TestLengthPacketReaderCompletePayloadControls(t *testing.T) {
	for _, size := range []int{0, 1, buf.Size, buf.Size + 1, 65535} {
		payload := bytes.Repeat([]byte{42}, size)
		wire := make([]byte, size+2)
		binary.BigEndian.PutUint16(wire, uint16(size))
		copy(wire[2:], payload)
		mb, err := encoding.NewLengthPacketReader(bytes.NewReader(wire)).ReadMultiBuffer()
		if err != nil {
			buf.ReleaseMulti(mb)
			t.Fatal(err)
		}
		actual := make([]byte, mb.Len())
		mb.Copy(actual)
		buf.ReleaseMulti(mb)
		if !bytes.Equal(actual, payload) {
			t.Fatalf("complete payload changed at size %d", size)
		}
	}
}
