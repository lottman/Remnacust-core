package buf_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestTimeoutWrapperPendingReadInterrupt(t *testing.T) {
	reader, writer := pipe.New()
	t.Cleanup(reader.Interrupt)
	t.Cleanup(func() { writer.Close() })
	wrapped := &buf.TimeoutWrapperReader{Reader: reader}
	if mb, err := wrapped.ReadMultiBufferTimeout(time.Millisecond); !errors.Is(err, buf.ErrReadTimeout) {
		buf.ReleaseMulti(mb)
		t.Fatalf("pending read: %v", err)
	}
	if err := common.Interrupt(wrapped); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		mb, err := wrapped.ReadMultiBuffer()
		buf.ReleaseMulti(mb)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted pending read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout wrapper retained its pending read after interrupt")
	}
}

func TestSocketReaderWrappersPropagateInterrupt(t *testing.T) {
	for _, kind := range []string{"single", "packet"} {
		t.Run(kind, func(t *testing.T) {
			reader, writer := io.Pipe()
			t.Cleanup(func() { reader.Close() })
			t.Cleanup(func() { writer.Close() })
			var wrapped buf.Reader = buf.NewReader(reader)
			if kind == "packet" {
				wrapped = buf.NewPacketReader(reader)
			}
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
				t.Fatal("socket reader retained a blocked read after interrupt")
			}
		})
	}
}

func TestReaderWrappersPropagateInterrupt(t *testing.T) {
	for _, endpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "endpoint-timeout"}[endpoint], func(t *testing.T) {
			reader, writer := pipe.New()
			t.Cleanup(reader.Interrupt)
			t.Cleanup(func() { writer.Close() })
			var wrapped buf.Reader = &buf.TimeoutWrapperReader{Reader: reader}
			if endpoint {
				wrapped = &buf.EndpointOverrideReader{Reader: wrapped}
			}
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
				t.Fatal("wrapper retained a blocked read after interrupt")
			}
		})
	}
}
