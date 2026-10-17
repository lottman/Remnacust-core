package command

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xtls/xray-core/common/quotausage"
	"github.com/xtls/xray-core/core"
	feature_stats "github.com/xtls/xray-core/features/stats"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const UsageReceiptServiceName = "remnacust-usage-receipts-v1"
const UsageReceiptMethod = "/" + UsageReceiptServiceName + "/Exchange"

type usageReceiptControl interface {
	exchangeUsage(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

func registerUsageReceipts(server *grpc.Server, handler *statsServer) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: UsageReceiptServiceName,
		HandlerType: (*usageReceiptControl)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Exchange", Handler: func(s interface{}, ctx context.Context, decode func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
			request := new(structpb.Struct)
			if err := decode(request); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, request interface{}) (interface{}, error) {
				return s.(usageReceiptControl).exchangeUsage(ctx, request.(*structpb.Struct))
			}
			if interceptor == nil {
				return call(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: s, FullMethod: UsageReceiptMethod}, call)
		}}},
	}, handler)
}

func (s *statsServer) usageEnabled() bool {
	if s.usagePath != usageJournalPath {
		return s.usagePath != ""
	}
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("REMNACUST_NODE_RUNTIME") == "1" || os.Getenv("XERA_CORE_MANAGER_VERSION") == "1" {
		return true
	}
	if _, err := os.Lstat("/var/lib/remnanode/host-policy.json"); err == nil {
		return true
	}
	_, err := os.Lstat(filepath.Dir(s.usagePath))
	return !errors.Is(err, os.ErrNotExist)
}

func (s *statsServer) lockResets() func() {
	if manager, ok := s.stats.(interface{ LockCounterResets() func() }); ok {
		return manager.LockCounterResets()
	}
	s.resetAccess.Lock()
	return s.resetAccess.Unlock
}

// Once a durable stream starts, a legacy destructive reader cannot consume
// user counters behind its back. Non-destructive and inbound/outbound stats
// retain the original API behaviour.
func (s *statsServer) legacyUserReset() error {
	if !s.usageEnabled() {
		return nil
	}
	if s.usageErr != nil {
		return status.Error(codes.FailedPrecondition, "usage journal requires recovery")
	}
	_, err := readUsageJournal(s.usagePath)
	if errors.Is(err, os.ErrNotExist) && !s.usageSeen {
		return nil
	}
	return status.Error(codes.FailedPrecondition, "user counters require durable usage receipts")
}

func (s *statsServer) persistUsage(state *usageJournal) error {
	save := s.usageSave
	if save == nil {
		save = saveUsageJournal
	}
	if err := save(s.usagePath, state); err != nil {
		// A rename may have succeeded before fsync failed. Never reset or
		// recapture on an ambiguous write in this process.
		s.usageErr = err
		return status.Error(codes.Unavailable, "usage journal could not be committed; recovery required")
	}
	s.usageSeen = true
	return nil
}

func (s *statsServer) exchangeUsage(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	fields := request.AsMap()
	action, _ := fields["action"].(string)
	stream, _ := fields["stream"].(string)
	sequenceText, _ := fields["sequence"].(string)
	expectedCore, _ := fields["coreProcessId"].(string)
	digest, _ := fields["digest"].(string)
	sequence, valid := usageNumber(sequenceText, false)
	if !valid || !usageUUID(stream) || !usageUUID(expectedCore) || (action != "sample" && action != "ack") || (action == "sample" && len(fields) != 4) || (action == "ack" && (len(fields) != 5 || !usageHash(digest))) {
		return nil, status.Error(codes.InvalidArgument, "invalid usage receipt request")
	}
	if !s.usageEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "durable usage is available only in node runtime")
	}
	if _, ok := s.stats.(interface{ LockCounterResets() func() }); !ok {
		return nil, status.Error(codes.FailedPrecondition, "statistics manager cannot serialize durable counter resets")
	}
	unlock := s.lockResets()
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if s.usageErr != nil {
		return nil, status.Error(codes.FailedPrecondition, "usage journal requires recovery")
	}
	state, err := readUsageJournal(s.usagePath)
	if errors.Is(err, os.ErrNotExist) && !s.usageSeen && sequence == 1 && action == "sample" {
		state = &usageJournal{Version: 1, Stream: stream, Acked: "0"}
	} else if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "usage journal is missing or invalid")
	} else {
		s.usageSeen = true
	}
	if state.Stream != stream {
		return nil, status.Error(codes.FailedPrecondition, "usage stream binding differs")
	}
	acked, _ := usageNumber(state.Acked, true)
	if action == "ack" {
		if sequence == acked && state.AckDigest == digest {
			return structpb.NewStruct(map[string]any{"stream": stream, "sequence": sequenceText, "acknowledged": true})
		}
		frame := state.Pending
		if frame == nil || frame.Sequence != sequenceText || frame.CoreProcessID != expectedCore || usageDigest(frame) != digest {
			return nil, status.Error(codes.FailedPrecondition, "usage acknowledgement differs from pending frame")
		}
		state.Acked, state.AckDigest, state.Pending = sequenceText, digest, nil
		if err := s.persistUsage(state); err != nil {
			return nil, err
		}
		return structpb.NewStruct(map[string]any{"stream": stream, "sequence": sequenceText, "acknowledged": true})
	}
	if frame := state.Pending; frame != nil {
		if frame.Sequence != sequenceText || frame.CoreProcessID != expectedCore {
			return nil, status.Error(codes.FailedPrecondition, "a different usage frame awaits acknowledgement")
		}
		return usageFrameResponse(state)
	}
	if sequence <= acked || sequence-acked != 1 || expectedCore != core.ProcessID() {
		return nil, status.Error(codes.FailedPrecondition, "usage sequence or core incarnation differs")
	}
	frame := &usageFrame{Sequence: sequenceText, CoreProcessID: core.ProcessID(), CapturedAt: strconv.FormatInt(time.Now().UnixMilli(), 10), Stats: []usageValue{}}
	state.Pending = frame
	basePayload, _ := json.Marshal(state)
	payloadSize := len(basePayload)
	type capturedCounter struct {
		counter feature_stats.Counter
		value   int64
	}
	captured := make([]capturedCounter, 0)
	invalid := false
	s.stats.VisitCounters(func(name string, counter feature_stats.Counter) bool {
		if !quotausage.IsUsageCounter(name) {
			if strings.HasPrefix(name, quotausage.Prefix) {
				invalid = true
				return false
			}
			return true
		}
		value := counter.Value()
		if value < 0 || len(name) > 1024 || (value > 0 && len(captured) >= 100000) {
			invalid = true
			return false
		}
		if value > 0 {
			stat := usageValue{Name: name, Value: strconv.FormatInt(value, 10)}
			encoded, _ := json.Marshal(stat)
			payloadSize += len(encoded) + 1
			if payloadSize > usageJournalLimit {
				invalid = true
				return false
			}
			frame.Stats = append(frame.Stats, stat)
			captured = append(captured, capturedCounter{counter, value})
		}
		return true
	})
	if invalid {
		return nil, status.Error(codes.ResourceExhausted, "usage counters exceed the frame limit")
	}
	sort.Slice(frame.Stats, func(i, j int) bool { return frame.Stats[i].Name < frame.Stats[j].Name })
	state.Pending = frame
	if err := s.persistUsage(state); err != nil {
		return nil, err
	}
	// Additions from unrelated active hosts remain in RAM. Persisting precedes
	// subtraction: after a crash the frame survives even if this loop did not.
	for _, entry := range captured {
		entry.counter.Add(-entry.value)
	}
	return usageFrameResponse(state)
}

func usageFrameResponse(state *usageJournal) (*structpb.Struct, error) {
	stats := make([]any, 0, len(state.Pending.Stats))
	for _, value := range state.Pending.Stats {
		stats = append(stats, map[string]any{"name": value.Name, "value": value.Value})
	}
	return structpb.NewStruct(map[string]any{
		"capability": UsageReceiptServiceName, "stream": state.Stream,
		"sequence": state.Pending.Sequence, "coreProcessId": state.Pending.CoreProcessID,
		"capturedAt": state.Pending.CapturedAt, "digest": usageDigest(state.Pending), "stats": stats,
	})
}
