//go:build !wasm

package dispatcher

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestRevokeUserLinksIsSelective(t *testing.T) {
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	readerA, writerA := pipe.New()
	readerB, writerB := pipe.New()
	linkA := &transport.Link{Reader: readerA, Writer: writerA}
	linkB := &transport.Link{Reader: readerB, Writer: writerB}
	trackUserLink(ctxA, "1~aaaaaaaaaaaaaaaaaaaaaaaa", linkA, nil)
	trackUserLink(ctxB, "1~bbbbbbbbbbbbbbbbbbbbbbbb", linkB, nil)

	RevokeUserLinks("1~aaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := readerA.ReadMultiBuffer(); err == nil {
		t.Fatal("revoked device link remained readable")
	}
	payload := buf.New()
	_, _ = payload.Write([]byte("still connected"))
	if err := writerB.WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Fatalf("other device link was interrupted: %v", err)
	}
	data, err := readerB.ReadMultiBuffer()
	if err != nil || data.Len() != int32(len("still connected")) {
		t.Fatalf("other device could not receive data: %v", err)
	}
	buf.ReleaseMulti(data)
	AllowUserLinks("1~aaaaaaaaaaaaaaaaaaaaaaaa")
}

func TestRetireSharedCredentialClosesExistingStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sharedReader, sharedWriter := pipe.New()
	personalReader, personalWriter := pipe.New()
	trackUserLink(ctx, "42", &transport.Link{Reader: sharedReader, Writer: sharedWriter}, nil)
	trackUserLink(ctx, "42~devicecredential", &transport.Link{Reader: personalReader, Writer: personalWriter}, nil)

	RevokeUserLinks("42")
	if _, err := sharedReader.ReadMultiBuffer(); err == nil {
		t.Fatal("old shared credential remained connected after cutover")
	}
	payload := buf.New()
	_, _ = payload.Write([]byte("device still connected"))
	if err := personalWriter.WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Fatalf("personal device was interrupted: %v", err)
	}
	data, err := personalReader.ReadMultiBuffer()
	if err != nil || data.Len() != int32(len("device still connected")) {
		t.Fatalf("personal device could not receive data: %v", err)
	}
	buf.ReleaseMulti(data)
	AllowUserLinks("42")
}
