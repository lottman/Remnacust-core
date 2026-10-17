//go:build !wasm

package command_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appdns "github.com/xtls/xray-core/app/dns"
	apppolicy "github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/stats"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func runtimeDomainClient(t *testing.T, inbound, destination int, domain string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprint("127.0.0.1:", inbound), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	id, _ := hex.DecodeString(strings.ReplaceAll(runtimeTestUUID, "-", ""))
	request := append([]byte{0}, id...)
	request = append(request, 0, 1, byte(destination>>8), byte(destination), 2, byte(len(domain)))
	request = append(request, domain...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestRuntimeDNSGenerationKeepsOldStreamsAccountingAndProcessLogger(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	destination := runtimeServeEcho(t, first, "A:")
	second, err := net.Listen("tcp", fmt.Sprint("127.0.0.2:", destination))
	if err != nil {
		t.Fatal(err)
	}
	runtimeServeEcho(t, second, "B:")
	api, port := availableRuntimePort(t), availableRuntimePort(t)
	files := t.TempDir()
	build := func(ip, name string) *core.Config {
		handshake := 11
		if name == "updated" {
			handshake = 23
		}
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "debug", "error": filepath.Join(files, name+".error.log"), "access": filepath.Join(files, name+".access.log")},
			"dns":       map[string]interface{}{"hosts": map[string]interface{}{"generation.invalid": ip}},
			"stats":     map[string]interface{}{},
			"policy":    map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"handshake": handshake, "statsUserUplink": true, "statsUserDownlink": true}}},
			"api":       map[string]interface{}{"tag": "API", "listen": fmt.Sprint("127.0.0.1:", api), "services": []string{"HandlerService"}},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "SAME", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "same-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"domainStrategy": "ForceIPv4", "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}}},
		})
	}
	initial, target := build("127.0.0.1", "initial"), build("127.0.0.2", "updated")
	server, err := core.New(initial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	server.SetRuntimeJournal(filepath.Join(files, "state", "profile.pb"))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	client, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", api), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	old := runtimeDomainClient(t, port, destination, "generation.invalid")
	runtimePing(t, old, "A:", true)
	accounting := server.GetFeature(stats.ManagerType()).(stats.Manager)
	counter := accounting.GetCounter("user>>>same-user>>>traffic>>>uplink")
	if counter == nil || counter.Value() == 0 {
		t.Fatal("initial traffic was not counted")
	}
	before := counter.Value()
	if err := runtimeApply(client, target, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, old, "A:", false)
	fresh := runtimeDomainClient(t, port, destination, "generation.invalid")
	runtimePing(t, fresh, "B:", true)
	if accounting.GetCounter("user>>>same-user>>>traffic>>>uplink") != counter || counter.Value() <= before {
		t.Fatal("generation replaced or reset accounting")
	}
	if err := server.RuntimeController().View(context.Background(), stats.ManagerType(), func(feature features.Feature) error {
		if feature != accounting {
			t.Fatal("generation borrowed a different accounting owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var admittedDNS dns.Client
	if err := server.RuntimeController().View(context.Background(), dns.ClientType(), func(feature features.Feature) error {
		admittedDNS = feature.(dns.Client)
		ips, _, err := feature.(dns.Client).LookupIP("generation.invalid", dns.IPOption{IPv4Enable: true})
		if err != nil || len(ips) != 1 || ips[0].String() != "127.0.0.2" {
			t.Fatal("API view kept stale DNS", ips, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.RuntimeController().View(context.Background(), policy.ManagerType(), func(feature features.Feature) error {
		if feature.(policy.Manager).ForLevel(0).Timeouts.Handshake != 23*time.Second {
			t.Fatal("generation kept stale level policy")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if server.GetFeature(policy.ManagerType()).(policy.Manager).ForLevel(0).Timeouts.Handshake != 11*time.Second {
		t.Fatal("new level policy mutated the old graph")
	}
	// A malformed DNS feature must not disturb either generation or its journal.
	invalid := proto.Clone(target).(*core.Config)
	for _, app := range invalid.App {
		if app.Type == serial.GetMessageType(&appdns.Config{}) {
			app.Value = []byte{255}
		}
	}
	revision := runtimeInspect(t, client)
	if err := runtimeApply(client, invalid, revision); err == nil {
		t.Fatal("invalid DNS was acknowledged")
	}
	if runtimeInspect(t, client) != revision {
		t.Fatal("failed DNS replaced live configuration")
	}
	for _, corrupt := range []func(*core.Config){
		func(config *core.Config) {
			config.App = append(config.App, proto.Clone(config.App[0]).(*serial.TypedMessage))
		},
		func(config *core.Config) {
			for index, app := range config.App {
				if app.Type == serial.GetMessageType(&apppolicy.Config{}) {
					settings, err := app.GetInstance()
					if err != nil {
						t.Fatal(err)
					}
					settings.(*apppolicy.Config).System = &apppolicy.SystemPolicy{Stats: &apppolicy.SystemPolicy_Stats{InboundUplink: true}}
					config.App[index] = serial.ToTypedMessage(settings)
				}
			}
		},
	} {
		invalid := proto.Clone(target).(*core.Config)
		corrupt(invalid)
		if err := runtimeApply(client, invalid, revision); err == nil {
			t.Fatal("duplicate feature or native listener counter change was acknowledged")
		}
		if runtimeInspect(t, client) != revision {
			t.Fatal("rejected policy changed live configuration")
		}
	}
	runtimePing(t, old, "A:", false)
	runtimePing(t, fresh, "B:", false)
	cached, err := core.LoadRuntimeJournal(filepath.Join(files, "state", "profile.pb"))
	if err != nil || !proto.Equal(cached, target) {
		t.Fatal("DNS generation checkpoint changed after rejection", err)
	}
	log.Record(&log.GeneralMessage{Severity: log.Severity_Error, Content: "ROOT_DNS_GENERATION_SENTINEL"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		root, _ := os.ReadFile(filepath.Join(files, "initial.error.log"))
		updated, _ := os.ReadFile(filepath.Join(files, "updated.error.log"))
		access, _ := os.ReadFile(filepath.Join(files, "updated.access.log"))
		if strings.Contains(string(root), "ROOT_DNS_GENERATION_SENTINEL") && strings.Contains(string(access), "generation.invalid") {
			if strings.Contains(string(updated), "ROOT_DNS_GENERATION_SENTINEL") {
				t.Fatal("generation replaced process logger")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("generation logs did not reach their owners")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := runtimeApply(client, initial, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, fresh, "B:", false)
	ips, _, err := admittedDNS.LookupIP("generation.invalid", dns.IPOption{IPv4Enable: true})
	if err != nil || len(ips) != 1 || ips[0].String() != "127.0.0.2" {
		t.Fatal("still-pinned DNS generation was retired early", ips, err)
	}
	_ = fresh.Close()
	deadline = time.Now().Add(3 * time.Second)
	for {
		_, _, err = admittedDNS.LookupIP("generation.invalid", dns.IPOption{IPv4Enable: true})
		if err == context.Canceled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DNS generation did not retire after its final stream closed", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	runtimePing(t, old, "A:", false)
	runtimePing(t, runtimeDomainClient(t, port, destination, "generation.invalid"), "A:", true)
}
