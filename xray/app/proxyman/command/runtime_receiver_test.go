//go:build !wasm

package command

import (
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func receiverWithWireOrder(reverse bool) *serial.TypedMessage {
	// Same protobuf map encoded in two legal orders, nested inside a transport.
	headers := [][2]string{{"X-One", "one"}, {"X-Two", "two"}}
	if reverse {
		headers[0], headers[1] = headers[1], headers[0]
	}
	var wire []byte
	for _, header := range headers {
		entry := protowire.AppendTag(nil, 1, protowire.BytesType)
		entry = protowire.AppendString(entry, header[0])
		entry = protowire.AppendTag(entry, 2, protowire.BytesType)
		entry = protowire.AppendString(entry, header[1])
		wire = protowire.AppendTag(wire, 3, protowire.BytesType)
		wire = protowire.AppendBytes(wire, entry)
	}
	return serial.ToTypedMessage(&proxyman.ReceiverConfig{
		PortList:       &xnet.PortList{Range: []*xnet.PortRange{{From: 443, To: 443}}},
		StreamSettings: &internet.StreamConfig{ProtocolName: "websocket", TransportSettings: []*internet.TransportConfig{{ProtocolName: "websocket", Settings: &serial.TypedMessage{Type: serial.GetMessageType(&websocket.Config{}), Value: wire}}}},
	})
}

func TestRuntimeReceiverIgnoresEncodingOrderButPreservesListenerChanges(t *testing.T) {
	a, b := receiverWithWireOrder(false), receiverWithWireOrder(true)
	if proto.Equal(a, b) {
		t.Fatal("fixture must have different opaque bytes")
	}
	before := proto.Clone(a)
	equal, err := sameRuntimeReceiver(a, b)
	if err != nil || !equal {
		t.Fatalf("equivalent listener rejected: %v", err)
	}
	if !proto.Equal(a, before) {
		t.Fatal("comparison modified the running configuration")
	}
	for _, change := range []string{"port", "transport", "sniffing", "header"} {
		value, _ := b.GetInstance()
		receiver := value.(*proxyman.ReceiverConfig)
		switch change {
		case "port":
			receiver.PortList.Range[0].From = 444
			receiver.PortList.Range[0].To = 444
		case "transport":
			receiver.StreamSettings.ProtocolName = "tcp"
		case "sniffing":
			receiver.SniffingSettings = &proxyman.SniffingConfig{Enabled: true}
		case "header":
			receiver.StreamSettings.TransportSettings[0].Settings = serial.ToTypedMessage(&websocket.Config{Header: map[string]string{"X-One": "changed", "X-Two": "two"}})
		}
		if same, err := sameRuntimeReceiver(a, serial.ToTypedMessage(receiver)); err != nil || same {
			t.Fatalf("%s change was accepted: %v", change, err)
		}
	}
	invalid := proto.Clone(b).(*serial.TypedMessage)
	invalid.Value = []byte{0xff}
	if _, err := sameRuntimeReceiver(a, invalid); err == nil {
		t.Fatal("malformed listener accepted")
	}
}
