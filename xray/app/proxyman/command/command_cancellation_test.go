package command

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/vless"
	vlessinbound "github.com/xtls/xray-core/proxy/vless/inbound"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func cancellationInboundConfig(tag string) *core.InboundHandlerConfig {
	return &core.InboundHandlerConfig{
		Tag:              tag,
		ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{PortList: &xnet.PortList{}}),
		ProxySettings: serial.ToTypedMessage(&vlessinbound.Config{
			Decryption: "none",
			Users:      []*protocol.User{cancellationUser("existing@example.test", "11111111-1111-4111-8111-111111111111")},
		}),
	}
}

func cancellationUser(email, id string) *protocol.User {
	return &protocol.User{Email: email, Account: serial.ToTypedMessage(&vless.Account{Id: id})}
}

func cancellationHandler(t *testing.T) (*handlerServer, proxy.UserManager) {
	t.Helper()
	instance, err := core.NewProfile(context.Background(), &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		},
		Inbound:  []*core.InboundHandlerConfig{cancellationInboundConfig("existing")},
		Outbound: []*core.OutboundHandlerConfig{{Tag: "existing", ProxySettings: serial.ToTypedMessage(&freedom.Config{})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	if err := instance.Start(); err != nil {
		t.Fatal(err)
	}
	server := &handlerServer{
		s: instance, operation: instance.ManagementAccess(),
		ihm: instance.GetFeature(inbound.ManagerType()).(inbound.Manager),
		ohm: instance.GetFeature(outbound.ManagerType()).(outbound.Manager),
	}
	handler, err := server.ihm.GetHandler(context.Background(), "existing")
	if err != nil {
		t.Fatal(err)
	}
	p, err := getInbound(handler)
	if err != nil {
		t.Fatal(err)
	}
	return server, p.(proxy.UserManager)
}

func TestCanceledQueuedLegacyInboundMutation(t *testing.T) {
	for _, method := range []string{"add-user", "remove-user", "AddInbound", "RemoveInbound"} {
		for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
			t.Run(method+"/"+code.String(), func(t *testing.T) {
				server, users := cancellationHandler(t)
				client, entered, finished := cancellationHandlerRPC(t, server)
				call := func(ctx context.Context) error {
					switch method {
					case "AddInbound":
						_, err := client.AddInbound(ctx, &AddInboundRequest{Inbound: cancellationInboundConfig("new")})
						return err
					case "RemoveInbound":
						_, err := client.RemoveInbound(ctx, &RemoveInboundRequest{Tag: "existing"})
						return err
					default:
						operation := serial.ToTypedMessage(&RemoveUserOperation{Email: "existing@example.test"})
						if method == "add-user" {
							operation = serial.ToTypedMessage(&AddUserOperation{User: cancellationUser("new@example.test", "22222222-2222-4222-8222-222222222222")})
						}
						_, err := client.AlterInbound(ctx, &AlterInboundRequest{Tag: "existing", Operation: operation})
						return err
					}
				}
				changed := func() bool {
					switch method {
					case "AddInbound":
						_, err := server.ihm.GetHandler(context.Background(), "new")
						return err == nil
					case "RemoveInbound":
						_, err := server.ihm.GetHandler(context.Background(), "existing")
						return err != nil
					case "add-user":
						return users.GetUser(context.Background(), "new@example.test") != nil
					default:
						return users.GetUser(context.Background(), "existing@example.test") == nil
					}
				}
				server.operation.Lock()
				locked := true
				defer func() {
					if locked {
						server.operation.Unlock()
					}
				}()
				ctx, cancel := context.WithCancel(context.Background())
				if code == codes.DeadlineExceeded {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- call(ctx) }()
				serverContext := awaitHandlerCancellation(t, entered)
				if code == codes.Canceled {
					cancel()
				}
				awaitHandlerCancellation(t, serverContext.Done())
				if err := awaitHandlerCancellation(t, result); status.Code(err) != code {
					t.Fatalf("client error: %v", err)
				}
				server.operation.Unlock()
				locked = false
				serverError := awaitHandlerCancellation(t, finished)
				if changed() {
					t.Error("canceled queued RPC changed actual inbound/user state")
				}
				if status.Code(serverError) != status.FromContextError(serverContext.Err()).Code() {
					t.Errorf("server error after acquiring management lock: %v", serverError)
				}
				if t.Failed() {
					return
				}
				healthy, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := call(healthy); err != nil || !changed() {
					t.Fatalf("healthy subsequent mutation failed: %v", err)
				}
			})
		}
	}
}

func TestCanceledDirectLegacyOutboundMutation(t *testing.T) {
	for _, method := range []string{"AddOutbound", "RemoveOutbound"} {
		t.Run(method, func(t *testing.T) {
			server, _ := cancellationHandler(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			switch method {
			case "AddOutbound":
				_, err = server.AddOutbound(ctx, &AddOutboundRequest{Outbound: &core.OutboundHandlerConfig{Tag: "new", ProxySettings: serial.ToTypedMessage(&freedom.Config{})}})
				if server.ohm.GetHandler("new") != nil {
					t.Error("canceled direct RPC added actual outbound")
				}
			default:
				_, err = server.RemoveOutbound(ctx, &RemoveOutboundRequest{Tag: "existing"})
				if server.ohm.GetHandler("existing") == nil {
					t.Error("canceled direct RPC removed actual outbound")
				}
			}
			if status.Code(err) != codes.Canceled {
				t.Errorf("direct outbound cancellation: %v", err)
			}
			if t.Failed() {
				return
			}
			healthy, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			if method == "AddOutbound" {
				_, err = server.AddOutbound(healthy, &AddOutboundRequest{Outbound: &core.OutboundHandlerConfig{Tag: "new", ProxySettings: serial.ToTypedMessage(&freedom.Config{})}})
				if server.ohm.GetHandler("new") == nil {
					t.Error("healthy subsequent RPC did not add actual outbound")
				}
			} else {
				_, err = server.RemoveOutbound(healthy, &RemoveOutboundRequest{Tag: "existing"})
				if server.ohm.GetHandler("existing") != nil {
					t.Error("healthy subsequent RPC did not remove actual outbound")
				}
			}
			if err != nil {
				t.Errorf("healthy subsequent outbound RPC: %v", err)
			}
		})
	}
}

func TestCanceledRuntimeAccessUserMutation(t *testing.T) {
	for _, method := range []string{"add-user", "remove-user"} {
		for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
			t.Run(method+"/"+code.String(), func(t *testing.T) {
				journal := filepath.Join(t.TempDir(), "state", "users.pb")
				server, users := cancellationHandler(t)
				server.s.SetRuntimeJournal(journal)
				original := server.s.Configuration()
				if err := server.s.SaveRuntimeConfiguration(original); err != nil {
					t.Fatal(err)
				}
				checkpoint, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				checkpointFile, err := os.Stat(journal)
				if err != nil {
					t.Fatal(err)
				}
				reader, writer := pipe.New()
				linkContext, stopLink := context.WithCancel(session.ContextWithInbound(server.s.RuntimeContext(), &session.Inbound{
					Tag: "existing", User: users.GetUser(context.Background(), "existing@example.test"),
				}))
				t.Cleanup(func() { stopLink(); reader.Interrupt(); writer.Close() })
				dispatcher.WrapLink(linkContext, policy.DefaultManager{}, stats.NoopManager{}, &transport.Link{Reader: reader, Writer: writer})
				cancellationLinkLive(t, reader, writer)
				client, entered, finished := cancellationHandlerRPC(t, server)
				call := func(ctx context.Context) error {
					operation := serial.ToTypedMessage(&RemoveUserOperation{Email: "existing@example.test"})
					if method == "add-user" {
						operation = serial.ToTypedMessage(&AddUserOperation{User: cancellationUser("new@example.test", "22222222-2222-4222-8222-222222222222")})
					}
					_, err := client.AlterInbound(ctx, &AlterInboundRequest{Tag: "existing", Operation: operation})
					return err
				}
				barrier := server.s.RuntimeAccess()
				barrier.RLock()
				locked := true
				defer func() {
					if locked {
						barrier.RUnlock()
					}
				}()
				ctx, cancel := context.WithCancel(context.Background())
				if code == codes.DeadlineExceeded {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- call(ctx) }()
				serverContext := awaitHandlerCancellation(t, entered)
				awaitCancellationRuntimeWriter(t, barrier)
				if code == codes.Canceled {
					cancel()
				}
				awaitHandlerCancellation(t, serverContext.Done())
				if err := awaitHandlerCancellation(t, result); status.Code(err) != code {
					t.Fatalf("client error while queued at runtime barrier: %v", err)
				}
				barrier.RUnlock()
				locked = false
				serverError := awaitHandlerCancellation(t, finished)
				if users.GetUser(context.Background(), "existing@example.test") == nil || users.GetUser(context.Background(), "new@example.test") != nil || users.GetUsersCount(context.Background()) != 1 {
					t.Error("canceled runtime-barrier waiter mutated actual users")
				}
				after, err := os.ReadFile(journal)
				if err != nil {
					t.Fatal(err)
				}
				afterFile, err := os.Stat(journal)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(checkpoint, after) || !os.SameFile(checkpointFile, afterFile) {
					t.Error("canceled runtime-barrier waiter rewrote checkpoint")
				}
				if !proto.Equal(original, server.s.Configuration()) {
					t.Error("canceled runtime-barrier waiter recorded configuration")
				}
				cancellationLinkLive(t, reader, writer)
				if status.Code(serverError) != status.FromContextError(serverContext.Err()).Code() {
					t.Errorf("server error after acquiring runtime barrier: %v", serverError)
				}
				if t.Failed() {
					return
				}
				healthy, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := call(healthy); err != nil {
					t.Fatalf("healthy subsequent user operation failed: %v", err)
				}
				stored, err := core.LoadRuntimeJournal(journal)
				if err != nil || !proto.Equal(stored, server.runtimeConfiguration(context.Background())) || !proto.Equal(stored, server.s.Configuration()) || proto.Equal(original, stored) {
					t.Fatalf("healthy operation did not checkpoint and record actual users: %v", err)
				}
				if method == "add-user" {
					if users.GetUsersCount(context.Background()) != 2 || users.GetUser(context.Background(), "new@example.test") == nil {
						t.Error("healthy operation did not add actual user")
					}
					cancellationLinkLive(t, reader, writer)
				} else {
					if users.GetUsersCount(context.Background()) != 0 {
						t.Error("healthy operation did not remove actual user")
					}
					data, err := reader.ReadMultiBufferTimeout(time.Second)
					buf.ReleaseMulti(data)
					if err == nil || err == buf.ErrReadTimeout {
						t.Errorf("healthy removal did not revoke authenticated link: %v", err)
					}
				}
			})
		}
	}
}

func awaitCancellationRuntimeWriter(t *testing.T, barrier *sync.RWMutex) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for barrier.TryRLock() {
		barrier.RUnlock()
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("RPC did not queue at runtime barrier")
		}
	}
}

func cancellationLinkLive(t *testing.T, reader *pipe.Reader, writer *pipe.Writer) {
	t.Helper()
	payload := buf.New()
	_, _ = payload.Write([]byte("x"))
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Errorf("authenticated link was revoked: %v", err)
		return
	}
	data, err := reader.ReadMultiBufferTimeout(time.Second)
	defer buf.ReleaseMulti(data)
	if err != nil || len(data) != 1 || string(data[0].Bytes()) != "x" {
		t.Errorf("authenticated link did not carry data: %v", err)
	}
}

func cancellationHandlerRPC(t *testing.T, handler HandlerServiceServer) (HandlerServiceClient, <-chan context.Context, <-chan error) {
	t.Helper()
	entered := make(chan context.Context, 2)
	finished := make(chan error, 2)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		entered <- ctx
		response, err := next(ctx, request)
		finished <- err
		return response, err
	}))
	RegisterHandlerServiceServer(server, handler)
	listener := bufconn.Listen(1024 * 1024)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	connection, err := grpc.NewClient("passthrough:///cancellation-handler", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return NewHandlerServiceClient(connection), entered, finished
}

func awaitHandlerCancellation[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queued handler RPC")
		var zero T
		return zero
	}
}
