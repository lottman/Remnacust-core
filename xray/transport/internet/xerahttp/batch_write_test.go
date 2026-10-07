package xerahttp

import (
	"bytes"
	"io"
	"testing"

	"github.com/xtls/xray-core/common/buf"
)

type batchSink struct {
	bytes.Buffer
	maxWrite int
	fail     bool
}

func (w *batchSink) Close() error { return nil }
func (w *batchSink) Write(data []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(data))
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(data)
}

func TestTransportConnBatchWriteBoundAndOwnership(t *testing.T) {
	for _, fail := range []bool{false, true} {
		payload := bytes.Repeat([]byte{42}, 131089)
		mb := buf.MergeBytes(nil, payload)
		owned := append(buf.MultiBuffer(nil), mb...)
		writer := &batchSink{fail: fail}
		c := &transportConn{writer: writer}
		err := c.WriteMultiBuffer(mb)
		if fail && err != io.ErrClosedPipe || !fail && (err != nil || !bytes.Equal(payload, writer.Bytes())) {
			t.Fatalf("batch write: %v", err)
		}
		if writer.maxWrite > 65536 {
			t.Fatal("batch exceeded bound")
		}
		for _, b := range owned {
			if !b.IsEmpty() {
				t.Fatal("batch retained an owned buffer")
			}
		}
	}
}
