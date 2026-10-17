//go:build !wasm

package command_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	control "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	vlessinbound "github.com/xtls/xray-core/proxy/vless/inbound"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func runtimeExpectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("revoked stream timed out instead of closing")
		}
		return
	}
	var response [1]byte
	n, err := conn.Read(response[:])
	if n != 0 || err == nil {
		t.Fatal("revoked stream still carried data")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked stream remained open")
	}
}

func TestProfileUsersRouteByBindingAndRejectAmbiguousLegacyTags(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint("duplicate=", duplicate), func(t *testing.T) {
			a, b := runtimeEcho(t, "A:"), runtimeEcho(t, "B:")
			portA, portB, apiPort := availableRuntimePort(t), availableRuntimePort(t), availableRuntimePort(t)
			tagB := "OTHER"
			if duplicate {
				tagB = "SHARED"
			}
			build := func(tag string, port, destination int) *core.Config {
				return runtimeBuildJSON(t, map[string]interface{}{
					"log": map[string]interface{}{"loglevel": "none"},
					"inbounds": []interface{}{map[string]interface{}{
						"tag": tag, "listen": "127.0.0.1", "port": port, "protocol": "vless",
						"settings": map[string]interface{}{"decryption": "none", "clients": []interface{}{
							map[string]interface{}{"id": runtimeTestUUID, "email": "scoped-user"},
						}},
					}},
					"outbounds": []interface{}{map[string]interface{}{
						"tag": "direct", "protocol": "freedom", "settings": map[string]interface{}{
							"redirect":   fmt.Sprint("127.0.0.1:", destination),
							"finalRules": []interface{}{map[string]interface{}{"action": "allow", "ip": []string{"127.0.0.1/32"}}},
						},
					}},
				})
			}
			base := runtimeBuildJSON(t, map[string]interface{}{
				"api":   map[string]interface{}{"tag": "CONTROL", "listen": fmt.Sprint("127.0.0.1:", apiPort), "services": []string{"HandlerService"}},
				"stats": map[string]interface{}{}, "log": map[string]interface{}{"loglevel": "none"},
			})
			config, err := core.WithProfileBindings(base, []core.ProfileBinding{
				{BindingID: enrolledA, ProfileID: enrolledA, InboundID: enrolledA, Config: build("SHARED", portA, a)},
				{BindingID: enrolledB, ProfileID: enrolledB, InboundID: enrolledB, Config: build(tagB, portB, b)},
			})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := core.NewProfile(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			journal := filepath.Join(t.TempDir(), "private", "profile-users.pb")
			owner.SetRuntimeJournal(journal)
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			rpc, err := grpc.NewClient(fmt.Sprint("127.0.0.1:", apiPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer rpc.Close()
			client := control.NewHandlerServiceClient(rpc)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			scope := func(binding string) context.Context {
				return metadata.NewOutgoingContext(ctx, metadata.Pairs(control.ProfileBindingMetadata, binding))
			}
			count, err := client.GetInboundUsersCount(ctx, &control.GetInboundUserRequest{Tag: "SHARED"})
			if duplicate {
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("ambiguous tag: %v", err)
				}
			} else if err != nil || count.Count != 1 {
				t.Fatalf("legacy unique tag: %v, %v", count, err)
			}
			for _, id := range []string{enrolledA, enrolledB} {
				tag := "SHARED"
				if id == enrolledB {
					tag = tagB
				}
				users, err := client.GetInboundUsers(scope(id), &control.GetInboundUserRequest{Tag: tag})
				if err != nil || len(users.Users) != 1 {
					t.Fatalf("scoped users: %v, %v", users, err)
				}
			}
			for _, invalid := range []context.Context{
				scope(""), scope("33333333-3333-4333-8333-333333333333"),
				metadata.NewOutgoingContext(ctx, metadata.Pairs(control.ProfileBindingMetadata, enrolledA, control.ProfileBindingMetadata, enrolledB)),
			} {
				if _, err := client.GetInboundUsersCount(invalid, &control.GetInboundUserRequest{Tag: "SHARED"}); err == nil {
					t.Fatal("invalid binding was accepted")
				}
			}
			streamA, streamB := runtimeClient(t, portA, a), runtimeClient(t, portB, b)
			defer streamA.Close()
			defer streamB.Close()
			runtimePing(t, streamA, "A:", true)
			runtimePing(t, streamB, "B:", true)
			_, err = client.AlterInbound(scope(enrolledA), &control.AlterInboundRequest{
				Tag: "SHARED", Operation: serial.ToTypedMessage(&control.RemoveUserOperation{Email: "scoped-user"}),
			})
			if err != nil {
				t.Fatal(err)
			}
			runtimeExpectClosed(t, streamA)
			runtimePing(t, streamB, "B:", false)
			stored, err := core.LoadRuntimeJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			bindings, _, err := core.ConfigProfileBindings(stored)
			if err != nil {
				t.Fatal(err)
			}
			for _, binding := range bindings {
				settings, err := binding.Config.Inbound[0].ProxySettings.GetInstance()
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if binding.BindingID == enrolledA {
					want = 0
				}
				if len(settings.(*vlessinbound.Config).Users) != want {
					t.Fatal("credential checkpoint changed the wrong profile")
				}
			}
		})
	}
}
