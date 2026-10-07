package commander_test

import (
	"bytes"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
)

// A missing authority must be rejected by the transport before RPC dispatch.
func TestGRPCRejectsMissingAuthority(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	t.Cleanup(server.Stop)
	go server.Serve(listener)
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/test.Service/Call"}, {Name: "content-type", Value: "application/grpc"},
	} {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
			var status, grpcStatus string
			for _, field := range headers.Fields {
				if field.Name == ":status" {
					status = field.Value
				}
				if field.Name == "grpc-status" {
					grpcStatus = field.Value
				}
			}
			if status != "400" || grpcStatus != "13" {
				t.Fatalf("missing authority accepted: HTTP %s, gRPC %s", status, grpcStatus)
			}
			return
		}
	}
}
