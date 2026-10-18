//go:build !wasm

package command_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	control "github.com/xtls/xray-core/app/proxyman/command"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	vlessinbound "github.com/xtls/xray-core/proxy/vless/inbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/xerahttp"
)

func TestProfileRetirementKeepsHTTP2StreamsAndDeniesRemovedCachedUser(t *testing.T) {
	a, b := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	port := availableRuntimePort(t)
	certificate, hash := cert.MustGenerate(nil, cert.CommonName("localhost"))
	security := &tls.Config{Certificate: []*tls.Certificate{tls.ParseCertificate(certificate)}, PinnedPeerCertSha256: [][]byte{hash[:]}, NextProtocol: []string{"h2"}, ServerName: "localhost"}
	transportConfig := &xerahttp.Config{Path: "/retirement", Mode: xerahttp.ModeStreamOne, Xmux: &xerahttp.XmuxConfig{MaxConnections: &xerahttp.RangeConfig{From: 1, To: 1}, MaxConcurrency: &xerahttp.RangeConfig{From: 32, To: 32}, CMaxReuseTimes: &xerahttp.RangeConfig{From: 1000, To: 1000}}}
	stream := &internet.StreamConfig{ProtocolName: "xera-http", TransportSettings: []*internet.TransportConfig{{ProtocolName: "xera-http", Settings: serial.ToTypedMessage(transportConfig)}}, SecurityType: serial.GetMessageType(security), SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(security)}}
	build := func(destination int) *core.Config {
		config := runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "none"},
			"policy":    map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "H2", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "h2-retirement-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
		receiver, err := config.Inbound[0].ReceiverSettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		receiver.(*proxyman.ReceiverConfig).StreamSettings = stream
		config.Inbound[0].ReceiverSettings = serial.ToTypedMessage(receiver)
		return config
	}
	base := runtimeBuildJSON(t, map[string]interface{}{"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"}})
	initial, err := core.WithProfileBindings(base, []core.ProfileBinding{{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: build(a)}})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := core.NewProfile(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.SetRuntimeJournal(filepath.Join(t.TempDir(), "state", "h2.pb"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	settings := &internet.MemoryStreamConfig{ProtocolName: "xera-http", ProtocolSettings: transportConfig, SecurityType: stream.SecurityType, SecuritySettings: security}
	open := func(settings *internet.MemoryStreamConfig) net.Conn {
		conn, err := xerahttp.Dial(context.Background(), xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(port)), settings)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		id, _ := hex.DecodeString(strings.ReplaceAll(runtimeTestUUID, "-", ""))
		request := append([]byte{0}, id...)
		request = append(request, 0, 1, byte(a>>8), byte(a), 1, 127, 0, 0, 1)
		if _, err := conn.Write(request); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	old := open(settings)
	runtimePing(t, old, "A:", true)
	target, err := core.WithProfileBindings(base, []core.ProfileBinding{{BindingID: enrolledA, ProfileID: enrolledB, InboundID: enrolledB, Config: build(b)}})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := core.ProfilePlanDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	target, err = core.WithProfileDelivery(target, core.ProfileDelivery{Epoch: "1", Owner: enrolledA, Revision: "1", Enabled: true, InputsHash: strings.Repeat("a", 64), PlanHash: digest})
	if err != nil {
		t.Fatal(err)
	}
	newProfile, err := core.NewStagedManagedProfile(owner, build(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := newProfile.Start(); err != nil {
		_ = newProfile.Close()
		t.Fatal(err)
	}
	owner.ManagementAccess().Lock()
	err = owner.CommitManagedProfileTopology(target, map[string]*core.Instance{enrolledA: newProfile})
	owner.ManagementAccess().Unlock()
	if err != nil {
		_ = newProfile.Close()
		t.Fatal(err)
	}
	runtimePing(t, old, "A:", false)
	// The same client pool must use its existing physical HTTP/2 connection.
	late := open(settings)
	runtimePing(t, late, "A:", true)
	freshSettings := &internet.MemoryStreamConfig{ProtocolName: "xera-http", ProtocolSettings: transportConfig, SecurityType: stream.SecurityType, SecuritySettings: security}
	fresh := open(freshSettings)
	runtimePing(t, fresh, "B:", true)
	removed := newProfile.Configuration()
	native, err := removed.Inbound[0].ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	native.(*vlessinbound.Config).Users = nil
	removed.Inbound[0].ProxySettings = serial.ToTypedMessage(native)
	if _, err := control.ApplyEnrolledProfileBatch(context.Background(), owner, []control.EnrolledProfileTarget{{BindingID: enrolledA, Config: removed, ExpectedHash: enrolledHash(t, newProfile.Configuration())}}); err != nil {
		t.Fatal(err)
	}
	denied := open(settings)
	_, _ = denied.Write([]byte("ping"))
	_ = denied.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := io.ReadAll(denied)
	if bytes.Contains(data, []byte("A:ping")) {
		t.Fatal("retired cached HTTP/2 session restored removed credentials")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("removed cached identity was not disconnected")
	}
}
