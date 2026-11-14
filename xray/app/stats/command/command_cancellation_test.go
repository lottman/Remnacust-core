package command

import (
	"context"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/stats"
	feature_stats "github.com/xtls/xray-core/features/stats"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestCanceledQueuedLegacyStatsReset(t *testing.T) {
	for _, method := range []string{"GetStats", "QueryStats", "GetUsersStats"} {
		for _, shared := range []bool{true, false} {
			for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
				name := method + "/fallback/" + code.String()
				if shared {
					name = method + "/shared/" + code.String()
				}
				t.Run(name, func(t *testing.T) {
					manager, err := stats.NewManager(context.Background(), &stats.Config{})
					if err != nil {
						t.Fatal(err)
					}
					const prefix = "user>>>queued@example.test>>>"
					var counters []feature_stats.Counter
					for _, direction := range []string{"uplink", "downlink"} {
						counter, err := manager.RegisterCounter(prefix + "traffic>>>" + direction)
						if err != nil {
							t.Fatal(err)
						}
						counter.Set(9007199254740993)
						counters = append(counters, counter)
					}
					online, err := manager.RegisterOnlineMap(prefix + "online")
					if err != nil {
						t.Fatal(err)
					}
					online.AddIP("192.0.2.1")
					server := NewStatsServer(manager).(*statsServer)
					server.usagePath = ""
					if !shared {
						server.stats = struct{ feature_stats.Manager }{manager}
					}
					client, entered, finished := cancellationStatsRPC(t, server)
					read := func(ctx context.Context) ([]int64, error) {
						switch method {
						case "GetStats":
							response, err := client.GetStats(ctx, &GetStatsRequest{Name: prefix + "traffic>>>uplink", Reset_: true})
							if err != nil {
								return nil, err
							}
							return []int64{response.Stat.Value}, nil
						case "QueryStats":
							response, err := client.QueryStats(ctx, &QueryStatsRequest{Pattern: prefix, Reset_: true})
							if err != nil {
								return nil, err
							}
							var values []int64
							for _, stat := range response.Stat {
								values = append(values, stat.Value)
							}
							return values, nil
						default:
							response, err := client.GetUsersStats(ctx, &GetUsersStatsRequest{IncludeTraffic: true, Reset_: true})
							if err != nil {
								return nil, err
							}
							if len(response.Users) != 1 || response.Users[0].Traffic == nil {
								return nil, fmt.Errorf("missing online user traffic: %v", response)
							}
							return []int64{response.Users[0].Traffic.Uplink, response.Users[0].Traffic.Downlink}, nil
						}
					}
					unlock := server.lockResets()
					defer func() { unlock() }()
					ctx, cancel := context.WithCancel(context.Background())
					if code == codes.DeadlineExceeded {
						cancel()
						ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					}
					defer cancel()
					result := make(chan error, 1)
					go func() { _, err := read(ctx); result <- err }()
					serverContext := awaitStatsCancellation(t, entered)
					if code == codes.Canceled {
						cancel()
					}
					awaitStatsCancellation(t, serverContext.Done())
					if err := awaitStatsCancellation(t, result); status.Code(err) != code {
						t.Fatalf("client error: %v", err)
					}
					for _, counter := range counters {
						counter.Add(7)
					}
					unlock()
					unlock = func() {}
					serverError := awaitStatsCancellation(t, finished)
					for _, counter := range counters {
						if value := counter.Value(); value != 9007199254741000 {
							t.Errorf("canceled queued reset consumed traffic: %d", value)
						}
					}
					if status.Code(serverError) != status.FromContextError(serverContext.Err()).Code() {
						t.Errorf("server error after acquiring reset lock: %v", serverError)
					}
					if t.Failed() {
						return
					}
					healthy, stop := context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					values, err := read(healthy)
					want := []int64{9007199254741000, 9007199254741000}
					if method == "GetStats" {
						want = want[:1]
					}
					if err != nil || !slices.Equal(values, want) {
						t.Fatalf("healthy reset lost traffic: %v, %v", values, err)
					}
					for i, counter := range counters {
						want := int64(0)
						if method == "GetStats" && i == 1 {
							want = 9007199254741000
						}
						if counter.Value() != want {
							t.Fatalf("healthy reset left counter %d at %d", i, counter.Value())
						}
					}
				})
			}
		}
	}
}

func cancellationStatsRPC(t *testing.T, handler StatsServiceServer) (StatsServiceClient, <-chan context.Context, <-chan error) {
	t.Helper()
	entered := make(chan context.Context, 2)
	finished := make(chan error, 2)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		entered <- ctx
		response, err := next(ctx, request)
		finished <- err
		return response, err
	}))
	RegisterStatsServiceServer(server, handler)
	listener := bufconn.Listen(1024 * 1024)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	connection, err := grpc.NewClient("passthrough:///cancellation-stats", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return NewStatsServiceClient(connection), entered, finished
}

func awaitStatsCancellation[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queued statistics RPC")
		var zero T
		return zero
	}
}
