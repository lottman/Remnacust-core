//go:build !wasm

package command_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	configserial "github.com/xtls/xray-core/infra/conf/serial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func runtimeBuildJSON(t *testing.T, document interface{}) *core.Config {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := configserial.DecodeJSONConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	built, err := parsed.Build()
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func runtimeTransportSettings(name string) map[string]interface{} {
	switch name {
	case "ws":
		return map[string]interface{}{"network": "ws", "wsSettings": map[string]interface{}{"path": "/live-ports-test"}}
	case "grpc":
		return map[string]interface{}{"network": "grpc", "grpcSettings": map[string]interface{}{"serviceName": "live-ports-test"}}
	case "xhttp":
		return map[string]interface{}{"network": "xhttp", "xhttpSettings": map[string]interface{}{"path": "/live-ports-test", "mode": "auto"}}
	case "xerahttp":
		return map[string]interface{}{"network": "xera-http", "xeraHttpSettings": map[string]interface{}{"path": "/live-ports-test", "mode": "auto"}}
	default:
		return map[string]interface{}{"network": "tcp"}
	}
}

func runtimeTransportServer(t *testing.T, config *core.Config, port int, name string) *core.Config {
	t.Helper()
	fragment := runtimeBuildJSON(t, map[string]interface{}{"inbounds": []interface{}{map[string]interface{}{
		"tag": "PORT-A", "listen": "127.0.0.1", "port": port, "protocol": "vless",
		"settings":       map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "runtime-user"}}},
		"streamSettings": runtimeTransportSettings(name),
	}}})
	config.Inbound = fragment.Inbound
	return config
}

func runtimeTransportClient(t *testing.T, port int, name string, multiplex bool) *core.Instance {
	t.Helper()
	document := map[string]interface{}{"log": map[string]interface{}{"loglevel": "none"}, "outbounds": []interface{}{map[string]interface{}{
		"tag": "server", "protocol": "vless", "settings": map[string]interface{}{"vnext": []interface{}{map[string]interface{}{"address": "127.0.0.1", "port": port, "users": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "encryption": "none"}}}}},
		"streamSettings": runtimeTransportSettings(name), "mux": map[string]interface{}{"enabled": multiplex, "concurrency": 8, "xudpConcurrency": 8},
	}}}
	client, err := core.New(runtimeBuildJSON(t, document))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func runtimeDialThrough(t *testing.T, client *core.Instance, destination int) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	connection, err := core.Dial(ctx, client, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(destination)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func TestRuntimeNativeTransportStreamsSurviveRoutingChanges(t *testing.T) {
	for _, transport := range []string{"tcp", "ws", "grpc", "xhttp", "xerahttp", "mux"} {
		t.Run(transport, func(t *testing.T) {
			first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
			apiPort, port := availableRuntimePort(t), availableRuntimePort(t)
			name := transport
			if name == "mux" {
				name = "tcp"
			}
			config := runtimeTransportServer(t, runtimeTestConfig(t, apiPort, port, first, 0), port, name)
			server, err := core.New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := server.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close() })
			control, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			client := runtimeTransportClient(t, port, name, transport == "mux")
			old := runtimeDialThrough(t, client, first)
			runtimePing(t, old, "A:", false)
			next := runtimeTransportServer(t, runtimeTestConfig(t, apiPort, port, second, 0), port, name)
			if err := runtimeApply(control, next, runtimeInspect(t, control)); err != nil {
				t.Fatal(err)
			}
			runtimePing(t, old, "A:", false)
			fresh := runtimeDialThrough(t, client, first)
			runtimePing(t, fresh, "B:", false)
			runtimePing(t, old, "A:", false)
		})
	}
}

func runtimeTLSSettings(t *testing.T) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))), "\n")
	server := map[string]interface{}{"serverName": "localhost", "alpn": []string{"h3"}, "certificates": []interface{}{map[string]interface{}{"certificate": encoded, "key": strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))), "\n")}}}
	client := map[string]interface{}{"serverName": "localhost", "alpn": []string{"h3"}, "disableSystemRoot": true, "certificates": []interface{}{map[string]interface{}{"certificate": encoded, "usage": "verify"}}}
	return server, client
}

func TestRuntimeNativeHysteriaTCPAndUDPKeepEstablishedSessions(t *testing.T) {
	first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	udpEcho := func(port int, prefix string) {
		listener, err := net.ListenPacket("udp", fmt.Sprint("127.0.0.1:", port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			buffer := make([]byte, 1024)
			for {
				n, peer, err := listener.ReadFrom(buffer)
				if err != nil {
					return
				}
				if _, err := listener.WriteTo(append([]byte(prefix), buffer[:n]...), peer); err != nil {
					return
				}
			}
		}()
	}
	udpEcho(first, "A:")
	udpEcho(second, "B:")
	apiPort, port := availableRuntimePort(t), availableRuntimePort(t)
	serverTLS, clientTLS := runtimeTLSSettings(t)
	serverStream := map[string]interface{}{"network": "hysteria", "security": "tls", "tlsSettings": serverTLS, "hysteriaSettings": map[string]interface{}{"version": 2}}
	fragment := runtimeBuildJSON(t, map[string]interface{}{"inbounds": []interface{}{map[string]interface{}{"tag": "PORT-A", "listen": "127.0.0.1", "port": port, "protocol": "hysteria", "settings": map[string]interface{}{"version": 2, "clients": []interface{}{map[string]interface{}{"auth": "native-test-credential", "email": "runtime-user"}}}, "streamSettings": serverStream}}})
	config := runtimeTestConfig(t, apiPort, port, first, 0)
	config.Inbound = fragment.Inbound
	server, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	control, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	clientConfig := runtimeBuildJSON(t, map[string]interface{}{"log": map[string]interface{}{"loglevel": "none"}, "outbounds": []interface{}{map[string]interface{}{"tag": "server", "protocol": "hysteria", "settings": map[string]interface{}{"version": 2, "address": "127.0.0.1", "port": port}, "streamSettings": map[string]interface{}{"network": "hysteria", "security": "tls", "tlsSettings": clientTLS, "hysteriaSettings": map[string]interface{}{"version": 2, "auth": "native-test-credential"}}}}})
	client, err := core.New(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	oldTCP := runtimeDialThrough(t, client, first)
	runtimePing(t, oldTCP, "A:", false)
	dialUDP := func() net.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		connection, err := core.Dial(ctx, client, xnet.UDPDestination(xnet.LocalHostIP, xnet.Port(first)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connection.Close() })
		return connection
	}
	oldUDP := dialUDP()
	runtimePing(t, oldUDP, "A:", false)
	next := runtimeTestConfig(t, apiPort, port, second, 0)
	next.Inbound = fragment.Inbound
	if err := runtimeApply(control, next, runtimeInspect(t, control)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, oldTCP, "A:", false)
	runtimePing(t, oldUDP, "A:", false)
	runtimePing(t, runtimeDialThrough(t, client, first), "B:", false)
	runtimePing(t, dialUDP(), "B:", false)
}

func TestRuntimeNativeXUDPGlobalIDIsIsolatedByPortAndGeneration(t *testing.T) {
	first, second := runtimeUDPEcho(t, "A:"), runtimeUDPEcho(t, "B:")
	apiPort, port, extra := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	build := func(a, b int) *core.Config {
		config := runtimeTestConfig(t, apiPort, port, a, extra)
		separate := runtimeTestConfig(t, apiPort, port, b, 0).Outbound[0]
		separate.Tag = "separate-port"
		config.Outbound = append(config.Outbound, separate)
		config.App = append(config.App, serial.ToTypedMessage(&router.Config{Rule: []*router.RoutingRule{{InboundTag: []string{"PORT-B"}, TargetTag: &router.RoutingRule_Tag{Tag: separate.Tag}}}}))
		return config
	}
	server, err := core.New(build(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	control, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	clientA, clientB := runtimeTransportClient(t, port, "tcp", true), runtimeTransportClient(t, extra, "tcp", true)
	dial := func(client *core.Instance) net.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		ctx = context.WithValue(ctx, "cone", true)
		ctx = session.ContextWithInbound(ctx, &session.Inbound{Name: "dokodemo-door", Source: xnet.UDPDestination(xnet.LocalHostIP, 40000)})
		connection, err := core.Dial(ctx, client, xnet.UDPDestination(xnet.LocalHostIP, xnet.Port(first)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connection.Close() })
		return connection
	}
	oldA, oldB := dial(clientA), dial(clientB)
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	if err := runtimeApply(control, build(second, first), runtimeInspect(t, control)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, dial(clientA), "B:", false)
	runtimePing(t, dial(clientB), "A:", false)
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
}
