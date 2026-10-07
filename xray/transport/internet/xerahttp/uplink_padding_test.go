package xerahttp

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"
	"time"
)

func TestUplinkBudgetPersistsAcrossPackets(t *testing.T) {
	config := testDownlinkConfig()
	config.BudgetPercent = 1
	config.BurstBytes = 128
	allowance := &sharedPaddingAllowance{}
	var padding int
	for i := range 100 {
		encoder := &paddedUplinkReader{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 68))), config: config, allowance: allowance}
		wire, err := io.ReadAll(encoder)
		encoder.Close()
		if err != nil || len(wire) < 4 {
			t.Fatal("packet encoding failed")
		}
		padding += int(binary.BigEndian.Uint16(wire[2:4]))
		if padding > 128+(i+1)*68/100 {
			t.Fatal("packet reset the session padding budget")
		}
	}
}

func TestPlannedPacketLengthAndFragmentedSource(t *testing.T) {
	for _, size := range []int{0, 1, 68, 65536, 1000000} {
		payload := bytes.Repeat([]byte{42}, size)
		encoder, length := planPaddedPacket(io.NopCloser(iotest.OneByteReader(bytes.NewReader(payload))), testDownlinkConfig(), &sharedPaddingAllowance{}, int64(size))
		wire, err := io.ReadAll(encoder)
		encoder.Close()
		if err != nil || int64(len(wire)) != length {
			t.Fatalf("planned length %d: actual %d, expected %d, error %v", size, len(wire), length, err)
		}
		decoder := &paddedDownlinkReader{ReadCloser: io.NopCloser(bytes.NewReader(wire))}
		got, err := io.ReadAll(decoder)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatal("planned packet corrupted payload")
		}
	}
	encoder, _ := planPaddedPacket(io.NopCloser(bytes.NewReader([]byte{42})), testDownlinkConfig(), nil, 2)
	defer encoder.Close()
	if _, err := io.ReadAll(encoder); err != io.ErrUnexpectedEOF {
		t.Fatal("truncated planned source accepted")
	}
}

func TestAdaptivePaddingBudget(t *testing.T) {
	config := testDownlinkConfig()
	config.BudgetPercent = 1
	config.BurstBytes = 128
	var allowance paddingAllowance
	var payload, padding int64
	for i := range 10000 {
		size := 68
		if i%3 == 0 {
			size = 16384
		}
		payload += int64(size)
		padding += int64(allowance.next(config, size))
		if padding > int64(config.BurstBytes)+payload/100 {
			t.Fatal("padding exceeded its cumulative budget")
		}
	}
	if padding == 0 {
		t.Fatal("padding was disabled")
	}
}

func TestPaddedUplinkFragmentationAndEOF(t *testing.T) {
	for _, size := range []int{0, 1, 68, 32768} {
		payload := bytes.Repeat([]byte{42}, size)
		source := iotest.OneByteReader(bytes.NewReader(payload))
		encoder := &paddedUplinkReader{ReadCloser: io.NopCloser(source), config: testDownlinkConfig()}
		decoder := &paddedDownlinkReader{ReadCloser: encoder}
		got, err := io.ReadAll(decoder)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("round trip %d: %v", size, err)
		}
		decoder.Close()
		if encoder.frame != nil {
			t.Fatal("encoder retained a frame")
		}
	}
}

func TestPaddedUplinkCloseInterruptsRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	encoder := &paddedUplinkReader{ReadCloser: reader, config: testDownlinkConfig()}
	finished := make(chan error, 1)
	go func() { _, err := encoder.Read(make([]byte, 1)); finished <- err }()
	encoder.Close()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("encoder survived close")
	}
}

func TestPaddedUplinkCloseReleasesPartialFrame(t *testing.T) {
	encoder := &paddedUplinkReader{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 8192))), config: testDownlinkConfig()}
	if _, err := encoder.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	encoder.Close()
	if encoder.frame != nil {
		t.Fatal("close retained a partially sent frame")
	}
	if _, err := encoder.Read(make([]byte, 1)); err != io.ErrClosedPipe {
		t.Fatal("read after close")
	}
}
