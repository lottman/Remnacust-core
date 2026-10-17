//go:build !wasm

package command_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/transport/internet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestIndependentProfileGraphsOwnDNSPolicyAPIAndLiveSessions(t *testing.T) {
	testProfileGraphs(t, false)
}

func TestManagedProfileGraphsKeepNodeAccountingAcrossUpdatesAndRemoval(t *testing.T) {
	testProfileGraphs(t, true)
}

func testProfileGraphs(t *testing.T, managed bool) {
	a, b, next := runtimeEcho(t, "A:"), runtimeEcho(t, "B:"), runtimeEcho(t, "C:")
	portA, portB, apiA, apiB := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
	files := t.TempDir()
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"root.error.log", "A.error.log", "B.error.log", "A.access.log", "B.access.log"} {
				data, _ := os.ReadFile(filepath.Join(files, name))
				t.Logf("%s\n%s", name, data)
			}
		}
	})
	build := func(port, api, destination, handshake int, ip, name string) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "debug", "error": filepath.Join(files, name+".error.log"), "access": filepath.Join(files, name+".access.log")},
			"routing":   map[string]interface{}{"rules": []interface{}{map[string]interface{}{"type": "field", "inboundTag": []string{"SAME"}, "outboundTag": "direct"}}},
			"dns":       map[string]interface{}{"hosts": map[string]interface{}{"same.example": ip}},
			"stats":     map[string]interface{}{},
			"policy":    map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"handshake": handshake, "statsUserUplink": true, "statsUserDownlink": true}}},
			"api":       map[string]interface{}{"tag": "API", "listen": fmt.Sprint("127.0.0.1:", api), "services": []string{"HandlerService", "RoutingService"}},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "SAME", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "same-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	root, err := core.New(runtimeBuildJSON(t, map[string]interface{}{"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "debug", "error": filepath.Join(files, "root.error.log")}, "dns": map[string]interface{}{"hosts": map[string]interface{}{"same.example": "192.0.2.10"}}, "outbounds": []interface{}{map[string]interface{}{"protocol": "freedom", "tag": "root"}}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	construct := func(config *core.Config) (*core.Instance, error) {
		if managed {
			return core.NewManagedProfile(root, config)
		}
		return core.NewProfile(context.Background(), config)
	}
	configA := build(portA, apiA, a, 11, "192.0.2.20", "A")
	configB := build(portB, apiB, b, 23, "192.0.2.30", "B")
	profileA, err := construct(configA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { profileA.Close() })
	profileB, err := construct(configB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { profileB.Close() })
	profileA.SetRuntimeJournal(filepath.Join(files, "state", "A.pb"))
	profileB.SetRuntimeJournal(filepath.Join(files, "state", "B.pb"))
	for _, entry := range []struct {
		s         *core.Instance
		ip        string
		handshake time.Duration
	}{{profileA, "192.0.2.20", 11 * time.Second}, {profileB, "192.0.2.30", 23 * time.Second}} {
		ips, _, err := entry.s.GetFeature(dns.ClientType()).(dns.Client).LookupIP("same.example", dns.IPOption{IPv4Enable: true})
		if err != nil || len(ips) != 1 || ips[0].String() != entry.ip {
			t.Fatalf("profile DNS escaped: %v %v", ips, err)
		}
		if entry.s.GetFeature(policy.ManagerType()).(policy.Manager).ForLevel(0).Timeouts.Handshake != entry.handshake {
			t.Fatal("profile policy escaped")
		}
		if err := entry.s.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if profileA.GetFeature(routing.RouterType()) == profileB.GetFeature(routing.RouterType()) {
		t.Fatal("profiles borrowed the same router")
	}
	global, err := internet.LookupForIP("same.example", internet.DomainStrategy_FORCE_IP4, nil)
	if err != nil || len(global) != 1 || global[0].String() != "192.0.2.10" {
		t.Fatalf("profile replaced process DNS: %v %v", global, err)
	}
	controlA, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiA), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer controlA.Close()
	controlB, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiB), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer controlB.Close()
	oldA, oldB := runtimeClient(t, portA, a), runtimeClient(t, portB, b)
	runtimePing(t, oldA, "A:", true)
	runtimePing(t, oldB, "B:", true)
	accounting := root.GetFeature(stats.ManagerType()).(stats.Manager)
	var sharedCounter stats.Counter
	if managed {
		if profileA.GetFeature(stats.ManagerType()) != accounting || profileB.GetFeature(stats.ManagerType()) != accounting {
			t.Fatal("managed profile replaced node statistics")
		}
		sharedCounter = accounting.GetCounter("user>>>same-user>>>traffic>>>uplink")
		if sharedCounter == nil || sharedCounter.Value() == 0 {
			t.Fatal("managed traffic was not counted")
		}
	}
	beforeB := runtimeInspect(t, controlB)
	updatedA := build(portA, apiA, next, 11, "192.0.2.20", "A")
	if err := runtimeApply(controlA, updatedA, runtimeInspect(t, controlA)); err != nil {
		t.Fatal(err)
	}
	runtimePing(t, oldA, "A:", false)
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portA, a), "C:", true)
	runtimePing(t, runtimeClient(t, portB, b), "B:", true)
	cachedA, err := core.LoadRuntimeJournal(filepath.Join(files, "state", "A.pb"))
	if err != nil || !proto.Equal(cachedA, updatedA) {
		t.Fatal("profile A checkpoint differs", err)
	}
	cachedB, err := core.LoadRuntimeJournal(filepath.Join(files, "state", "B.pb"))
	if err != nil || !proto.Equal(cachedB, configB) {
		t.Fatal("profile B checkpoint changed", err)
	}
	if runtimeInspect(t, controlB) != beforeB {
		t.Fatal("profile A changed profile B's API/configuration")
	}
	log.Record(&log.GeneralMessage{Severity: log.Severity_Error, Content: "PROCESS_DEFAULT_SENTINEL"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		rootLog, _ := os.ReadFile(filepath.Join(files, "root.error.log"))
		accessA, _ := os.ReadFile(filepath.Join(files, "A.access.log"))
		accessB, _ := os.ReadFile(filepath.Join(files, "B.access.log"))
		if strings.Contains(string(rootLog), "PROCESS_DEFAULT_SENTINEL") && strings.Contains(string(accessA), fmt.Sprint("127.0.0.1:", a)) && strings.Contains(string(accessB), fmt.Sprint("127.0.0.1:", b)) {
			if strings.Contains(string(accessA), fmt.Sprint("127.0.0.1:", b)) || strings.Contains(string(accessB), fmt.Sprint("127.0.0.1:", a)) {
				t.Fatal("profile access logs escaped")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("profile/process logs did not reach their owners")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := profileA.Close(); err != nil {
		t.Fatal(err)
	}
	if managed && accounting.GetCounter("user>>>same-user>>>traffic>>>uplink") != sharedCounter {
		t.Fatal("removing a profile reset node statistics")
	}
	if managed {
		value := sharedCounter.Value()
		runtimePing(t, oldB, "B:", false)
		if sharedCounter.Value() <= value {
			t.Fatal("remaining profile stopped accounting")
		}
	}
	runtimePing(t, oldB, "B:", false)
	runtimePing(t, runtimeClient(t, portB, b), "B:", true)
}
