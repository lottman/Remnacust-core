package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/proxy"
	grpcencoding "github.com/xtls/xray-core/transport/internet/grpc/encoding"
)

func visionPayload(size int, marker int) []byte {
	payload := bytes.Repeat([]byte{0xa5}, size)
	if marker >= 0 && marker+3 <= size {
		copy(payload[marker:], proxy.TlsApplicationDataStart)
	}
	return payload
}

func TestVisionReshapePreservesEveryByte(t *testing.T) {
	for _, size := range []int{0, 1, 8170, 8171, 8192, 8193, 12288, 16384, 65535, 65536, 131072} {
		for _, marker := range []int{-1, 21, 8171, 8190} {
			t.Run(fmt.Sprintf("size=%d/marker=%d", size, marker), func(t *testing.T) {
				payload := visionPayload(size, marker)
				input := buf.NewWithSize(int32(max(size, 1)))
				if n, err := input.Write(payload); err != nil || n != len(payload) {
					t.Fatalf("input: %d, %v", n, err)
				}
				output := proxy.ReshapeMultiBuffer(context.Background(), buf.MultiBuffer{input})
				defer buf.ReleaseMulti(output)
				var actual []byte
				for _, b := range output {
					if b.Len() > buf.Size-21 {
						t.Errorf("padding-unsafe chunk: %d", b.Len())
					}
					actual = append(actual, b.Bytes()...)
				}
				if !bytes.Equal(actual, payload) {
					t.Fatalf("reshape changed payload: got %d bytes, want %d", len(actual), len(payload))
				}
			})
		}
	}
}

type visionCapture struct {
	blocks [][]byte
}

func (w *visionCapture) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)
	for _, b := range mb {
		w.blocks = append(w.blocks, bytes.Clone(b.Bytes()))
	}
	return nil
}

func visionRoundTrip(t *testing.T, input buf.MultiBuffer, expected []byte, uplink bool, configure ...func(*proxy.TrafficState)) *visionCapture {
	t.Helper()
	uuid := bytes.Repeat([]byte{0x42}, 16)
	capture := new(visionCapture)
	writerState := proxy.NewTrafficState(uuid)
	for _, apply := range configure {
		apply(writerState)
	}
	writer := proxy.NewVisionWriter(capture, writerState, uplink, context.Background(), nil, nil, nil)
	if err := writer.WriteMultiBuffer(input); err != nil {
		t.Fatal(err)
	}
	state := proxy.NewTrafficState(uuid)
	var actual []byte
	for _, block := range capture.blocks {
		if len(block) > buf.Size {
			t.Fatalf("wire block exceeds buffer size: %d", len(block))
		}
		decoded := proxy.XtlsUnpadding(buf.FromBytes(block), state, uplink, context.Background())
		actual = append(actual, decoded.Bytes()...)
		decoded.Release()
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Vision round-trip changed payload: got %d bytes, want %d", len(actual), len(expected))
	}
	return capture
}

func TestVisionWriterOversizedRoundTrip(t *testing.T) {
	for _, uplink := range []bool{false, true} {
		for _, size := range []int{0, 8170, 8171, 8192, 8193, 12288, 16384, 65535, 65536, 131072} {
			for _, marker := range []int{-1, 21, 8171} {
				t.Run(fmt.Sprintf("uplink=%t/size=%d/marker=%d", uplink, size, marker), func(t *testing.T) {
					payload := visionPayload(size, marker)
					visionRoundTrip(t, buf.MultiBuffer{buf.FromBytes(bytes.Clone(payload))}, payload, uplink)
				})
			}
		}
	}
}

func TestVisionOversizedLegacyPaddingEndRoundTrip(t *testing.T) {
	for _, uplink := range []bool{false, true} {
		t.Run(fmt.Sprintf("uplink=%t", uplink), func(t *testing.T) {
			payload := visionPayload(65536, -1)
			capture := visionRoundTrip(t, buf.MultiBuffer{buf.FromBytes(bytes.Clone(payload))}, payload, uplink, func(state *proxy.TrafficState) {
				state.NumberOfPacketToFilter = 1
			})
			if capture.blocks[0][16] != proxy.CommandPaddingEnd {
				t.Fatal("legacy padding did not end on the first chunk")
			}
		})
	}
}

func TestVisionOversizedTLSRecordEndsOnLastChunk(t *testing.T) {
	for _, uplink := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			for _, size := range []int{8192, 12288, 16384} {
				t.Run(fmt.Sprintf("uplink=%t/direct=%t/size=%d", uplink, direct, size), func(t *testing.T) {
					payload := visionPayload(size, 0)
					payload[3], payload[4] = byte((size-5)>>8), byte(size-5)
					capture := visionRoundTrip(t, buf.MultiBuffer{buf.FromBytes(bytes.Clone(payload))}, payload, uplink, func(state *proxy.TrafficState) {
						state.IsTLS = true
						state.IsTLS12orAbove = true
						state.EnableXtls = direct
					})
					for i, block := range capture.blocks {
						offset := 0
						if i == 0 {
							offset = 16
						}
						want := proxy.CommandPaddingContinue
						if i == len(capture.blocks)-1 {
							want = proxy.CommandPaddingEnd
							if direct {
								want = proxy.CommandPaddingDirect
							}
						}
						if block[offset] != want {
							t.Fatalf("chunk %d command=%d, want %d", i, block[offset], want)
						}
					}
				})
			}
		}
	}
}

type visionHunkStream struct {
	data []byte
}

func (*visionHunkStream) Context() context.Context      { return context.Background() }
func (*visionHunkStream) Send(*grpcencoding.Hunk) error { return nil }
func (*visionHunkStream) SendMsg(any) error             { return nil }
func (*visionHunkStream) RecvMsg(any) error             { return io.EOF }
func (s *visionHunkStream) Recv() (*grpcencoding.Hunk, error) {
	if s.data == nil {
		return nil, io.EOF
	}
	data := s.data
	s.data = nil
	return &grpcencoding.Hunk{Data: data}, nil
}

type visionMultiHunkStream struct {
	data [][]byte
}

func (*visionMultiHunkStream) Context() context.Context           { return context.Background() }
func (*visionMultiHunkStream) Send(*grpcencoding.MultiHunk) error { return nil }
func (*visionMultiHunkStream) SendMsg(any) error                  { return nil }
func (*visionMultiHunkStream) RecvMsg(any) error                  { return io.EOF }
func (s *visionMultiHunkStream) Recv() (*grpcencoding.MultiHunk, error) {
	if s.data == nil {
		return nil, io.EOF
	}
	data := s.data
	s.data = nil
	return &grpcencoding.MultiHunk{Data: data}, nil
}

func TestVisionWriterActualGRPCHunks(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("multi=%t", multi), func(t *testing.T) {
			first := visionPayload(16384, -1)
			second := visionPayload(8193, 21)
			var reader buf.Reader
			var expected []byte
			if multi {
				reader = grpcencoding.NewMultiHunkReadWriter(&visionMultiHunkStream{data: [][]byte{bytes.Clone(first), bytes.Clone(second)}}, nil)
				expected = append(bytes.Clone(first), second...)
			} else {
				reader = grpcencoding.NewHunkReadWriter(&visionHunkStream{data: bytes.Clone(first)}, nil)
				expected = first
			}
			input, err := reader.ReadMultiBuffer()
			if err != nil {
				t.Fatal(err)
			}
			visionRoundTrip(t, input, expected, true)
		})
	}
}
