//go:build !wasm && !windows

package command_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	statscommand "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestNodeGeneratedLuaProfileKeepsPrivateAPIAndLiveRoutes(t *testing.T) {
	fixturePath := os.Getenv("REMNACUST_NODE_LUA_FIXTURE")
	if fixturePath == "" {
		t.Skip("Generate the node generate-api-config fixture and set REMNACUST_NODE_LUA_FIXTURE")
	}
	info, err := os.Stat(fixturePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		t.Fatal("invalid node fixture", err)
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int                    `json:"version"`
		Initial map[string]interface{} `json:"initial"`
		Updated map[string]interface{} `json:"updated"`
		Scripts map[string]string      `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Version != 1 {
		t.Fatal("invalid node fixture version", err)
	}
	first, second := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
	port := availableRuntimePort(t)
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(private, "api.sock")
	scripts := t.TempDir()
	build := func(document map[string]interface{}) *core.Config {
		t.Helper()
		if document == nil {
			t.Fatal("missing node fixture document")
		}
		api := document["api"].(map[string]interface{})
		if api["listen"] != "/fixture/api.sock,0600" {
			t.Fatal("node did not generate a private API listener")
		}
		api["listen"] = socket + ",0600"
		inbounds := document["inbounds"].([]interface{})
		if len(inbounds) != 1 {
			t.Fatal("node injected an API inbound into a Lua profile")
		}
		inbounds[0].(map[string]interface{})["port"] = port
		outbounds := document["outbounds"].([]interface{})
		for index, destination := range []int{first, second} {
			outbounds[index].(map[string]interface{})["settings"].(map[string]interface{})["redirect"] = fmt.Sprint("127.0.0.1:", destination)
		}
		routing := document["routing"].(map[string]interface{})
		if _, injected := routing["rules"]; injected {
			t.Fatal("node injected rules into Lua routing")
		}
		name, _ := routing["script"].(string)
		if name != "first.lua" && name != "second.lua" {
			t.Fatal("fixture did not reference a published-release Lua file")
		}
		code, exists := fixture.Scripts[name]
		if !exists {
			t.Fatal("missing original Lua fixture source")
		}
		path := filepath.Join(scripts, name)
		if err := os.WriteFile(path, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
		routing["script"] = path
		return runtimeBuildJSON(t, document)
	}
	initial, updated := build(fixture.Initial), build(fixture.Updated)
	server, err := core.New(initial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("API socket is not private", err)
	}
	client, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := statscommand.NewStatsServiceClient(client).GetSysStats(ctx, &statscommand.SysStatsRequest{}); err != nil {
		t.Fatal("node's statistics API cannot reach the Lua core", err)
	}
	old := runtimeClient(t, port, first)
	runtimePing(t, old, "A:", true)
	if err := runtimeApply(client, updated, runtimeInspect(t, client)); err != nil {
		t.Fatal(err)
	}
	fresh := runtimeClient(t, port, first)
	runtimePing(t, fresh, "B:", true)
	runtimePing(t, old, "A:", false)
	if runtimeInspect(t, client) == "" {
		t.Fatal("private control disappeared after updating Lua")
	}
}
