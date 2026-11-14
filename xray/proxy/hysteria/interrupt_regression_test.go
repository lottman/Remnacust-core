package hysteria

import (
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

func TestUDPReaderPropagatesInterrupt(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close() })
	t.Cleanup(func() { writer.Close() })
	wrapped := &UDPReader{reader: reader, df: &Defragger{}}
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
		t.Fatal("UDP reader retained a blocked read after interrupt")
	}
}
