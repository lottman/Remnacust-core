package dispatcher

import (
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestCachedReaderInterruptsDispatchLinkReader(t *testing.T) {
	reader, writer := pipe.New()
	t.Cleanup(reader.Interrupt)
	t.Cleanup(func() { writer.Close() })
	cached := &cachedReader{reader: &buf.TimeoutWrapperReader{Reader: reader}}
	done := make(chan error, 1)
	go func() {
		mb, err := cached.ReadMultiBuffer()
		buf.ReleaseMulti(mb)
		done <- err
	}()
	cached.Interrupt()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cached reader retained a blocked DispatchLink read")
	}
}
