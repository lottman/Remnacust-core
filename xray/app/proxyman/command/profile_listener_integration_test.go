//go:build !wasm

package command_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func TestManagedProfileSameTCPPortReplacementKeepsNativeVLESSFlow(t *testing.T) {
	a, b := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	port := availableRuntimePort(t)
	build := func(destination int) *core.Config {
		return runtimeBuildJSON(t, map[string]interface{}{
			"log":       map[string]interface{}{"loglevel": "none"},
			"policy":    map[string]interface{}{"levels": map[string]interface{}{"0": map[string]interface{}{"uplinkOnly": 0, "downlinkOnly": 0}}},
			"inbounds":  []interface{}{map[string]interface{}{"tag": "public", "listen": "127.0.0.1", "port": port, "protocol": "vless", "settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{map[string]interface{}{"id": runtimeTestUUID, "email": "profile-socket-user"}}}}},
			"outbounds": []interface{}{map[string]interface{}{"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{"redirect": fmt.Sprint("127.0.0.1:", destination), "finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}},
		})
	}
	root, err := core.New(runtimeBuildJSON(t, map[string]interface{}{"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Start(); err != nil {
		t.Fatal(err)
	}
	old, err := core.NewManagedProfile(root, build(a))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	stream := runtimeClient(t, port, a)
	runtimePing(t, stream, "A:", true)
	next, err := core.NewStagedManagedProfile(root, build(b))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Start(); err != nil {
		t.Fatal(err)
	}
	probe := runtimeClient(t, port, a)
	runtimePing(t, probe, "A:", true)
	_ = probe.Close()
	failure := errors.New("checkpoint unavailable")
	if err := root.CommitProfileTCPListeners([]*core.Instance{next}, []*core.Instance{old}, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal("failed checkpoint was accepted", err)
	}
	probe = runtimeClient(t, port, a)
	runtimePing(t, probe, "A:", true)
	_ = probe.Close()
	journal := filepath.Join(t.TempDir(), "private", "replacement.pb")
	if err := root.CommitProfileTCPListeners([]*core.Instance{next}, []*core.Instance{old}, func() error { return core.SaveRuntimeJournal(journal, build(b)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadRuntimeJournal(journal); err != nil {
		t.Fatal(err)
	}
	current := runtimeClient(t, port, b)
	runtimePing(t, current, "B:", true)
	runtimePing(t, stream, "A:", false)
	select {
	case <-old.TCPProfileDrained():
		t.Fatal("old graph drained with a native VLESS flow")
	default:
	}
	_ = stream.Close()
	select {
	case <-old.TCPProfileDrained():
	case <-time.After(8 * time.Second):
		t.Fatal("native VLESS flow did not release retired ownership")
	}
	_ = old.Close()
	runtimePing(t, current, "B:", false)
}
