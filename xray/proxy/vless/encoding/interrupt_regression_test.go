package encoding_test

import (
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/proxy/vless/encoding"
)

func TestLengthPacketReaderPropagatesInterrupt(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close() })
	t.Cleanup(func() { writer.Close() })
	wrapped := encoding.NewLengthPacketReader(&buf.BufferedReader{Reader: buf.NewReader(reader)})
	done := make(chan error, 1)
	go func() {
		mb, err := wrapped.ReadMultiBuffer()
		buf.ReleaseMulti(mb)
		done <- err
	}()
	if err := common.Interrupt(wrapped); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("length packet reader retained a blocked read after interrupt")
	}
}
