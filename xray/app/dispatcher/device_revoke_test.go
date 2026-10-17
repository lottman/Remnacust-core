//go:build !wasm

package dispatcher

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

func TestRetiredProfileDenySurvivesLateLinksAndClearsAfterClose(t *testing.T) {
	profile, err := core.NewProfile(context.Background(), &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	other, err := core.NewProfile(context.Background(), &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	const email = "retired-cached-identity"
	denyRetiredProfileUser(profile, email)
	ctx, cancel := context.WithCancel(profile.RuntimeContext())
	defer cancel()
	if trackUserLink(ctx, email, nil, nil) {
		t.Fatal("late identity bypassed its retired profile deny")
	}
	otherCtx, otherCancel := context.WithCancel(other.RuntimeContext())
	defer otherCancel()
	if !trackUserLink(otherCtx, email, nil, nil) {
		t.Fatal("profile-local deny crossed an unrelated graph")
	}
	_ = profile.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		activeUserLinks.Lock()
		_, retained := retiredProfileRevocations[profile.RuntimeAccess()]
		activeUserLinks.Unlock()
		if !retained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed profile retained its deny registry")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRevokeProfileInboundUserLinksDoesNotCrossSameTagGraphs(t *testing.T) {
	profileA, err := core.NewProfile(context.Background(), &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profileA.Close()
	profileB, err := core.NewProfile(context.Background(), &core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer profileB.Close()
	generation := core.NewFeatureScope(profileA)
	defer generation.Close()
	const email = "same-tag-profile-revocation"
	var contexts []context.CancelFunc
	defer func() {
		for _, cancel := range contexts {
			cancel()
		}
	}()
	var readers []*pipe.Reader
	var writers []*pipe.Writer
	for _, instance := range []*core.Instance{profileA, generation.Instance, profileB} {
		reader, writer := pipe.New()
		readers = append(readers, reader)
		writers = append(writers, writer)
		defer reader.Interrupt()
		defer writer.Close()
		ctx, cancel := context.WithCancel(session.ContextWithInbound(instance.RuntimeContext(), &session.Inbound{Tag: "SAME"}))
		contexts = append(contexts, cancel)
		if !trackUserLink(ctx, email, &transport.Link{Reader: reader, Writer: writer}, nil) {
			t.Fatal("link rejected before revocation")
		}
	}
	RevokeProfileInboundUserLinks(profileA, "SAME", email)
	for _, reader := range readers[:2] {
		if _, err := reader.ReadMultiBuffer(); err == nil {
			t.Fatal("profile or draining generation remained connected")
		}
	}
	payload := buf.New()
	_, _ = payload.Write([]byte("other graph"))
	if err := writers[2].WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Fatal("same-tag sibling was revoked", err)
	}
	data, err := readers[2].ReadMultiBuffer()
	if err != nil || data.Len() != 11 {
		t.Fatal("same-tag sibling lost traffic", err)
	}
	buf.ReleaseMulti(data)
}

func TestRevokeUserLinksClosesAuthenticatedRawConnection(t *testing.T) {
	connA, peerA := net.Pipe()
	connB, peerB := net.Pipe()
	t.Cleanup(func() {
		connA.Close()
		peerA.Close()
		connB.Close()
		peerB.Close()
		AllowUserLinks("79~aaaaaaaaaaaaaaaaaaaaaaaa")
	})
	ctxA, cancelA := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Conn: connA}))
	defer cancelA()
	ctxB, cancelB := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Conn: connB}))
	defer cancelB()
	trackUserLink(ctxA, "79~aaaaaaaaaaaaaaaaaaaaaaaa", nil, nil)
	trackUserLink(ctxB, "79~bbbbbbbbbbbbbbbbbbbbbbbb", nil, nil)
	RevokeUserLinks("79~aaaaaaaaaaaaaaaaaaaaaaaa")
	peerA.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peerA.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked raw connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revocation did not close the raw connection")
	}
	peerB.SetReadDeadline(time.Now().Add(time.Second))
	go connB.Write([]byte{42})
	data := make([]byte, 1)
	if n, err := peerB.Read(data); err != nil || n != 1 || data[0] != 42 {
		t.Fatalf("other authenticated connection interrupted: %v", err)
	}
	// The guard must also close a connection accepted just before removal.
	raceConn, racePeer := net.Pipe()
	defer raceConn.Close()
	defer racePeer.Close()
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Conn: raceConn})
	trackUserLink(ctx, "79~aaaaaaaaaaaaaaaaaaaaaaaa", nil, nil)
	racePeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := racePeer.Read(make([]byte, 1)); err == nil {
		t.Fatal("late accepted raw connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("late accepted connection was not closed")
	}
}

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

func TestRevokeInboundUserLinksKeepsSameIdentityOnOtherPort(t *testing.T) {
	const email = "runtime-selective-port-user"
	readerA, writerA := pipe.New()
	readerB, writerB := pipe.New()
	ctxA, cancelA := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "PORT-A"}))
	ctxB, cancelB := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "PORT-B"}))
	defer cancelA()
	defer cancelB()
	defer readerB.Interrupt()
	defer writerB.Close()
	trackUserLink(ctxA, email, &transport.Link{Reader: readerA, Writer: writerA}, nil)
	trackUserLink(ctxB, email, &transport.Link{Reader: readerB, Writer: writerB}, nil)
	RevokeInboundUserLinks("PORT-A", email)
	if _, err := readerA.ReadMultiBuffer(); err == nil {
		t.Fatal("removed port identity remained connected")
	}
	payload := buf.New()
	_, _ = payload.Write([]byte("still connected"))
	if err := writerB.WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Fatal("the other port was revoked", err)
	}
	data, err := readerB.ReadMultiBuffer()
	if err != nil || data.Len() != int32(len("still connected")) {
		t.Fatal("the other port lost data", err)
	}
	buf.ReleaseMulti(data)
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
