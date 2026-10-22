//go:build !wasm

package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestFairSpeedOwnerCannotMultiplyShareWithConnections(t *testing.T) {
	limiter := rate.NewLimiter(512*1024, policyBurst)
	limiter.AllowN(time.Now(), policyBurst)
	fair := newFairSpeedLimiter(limiter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flows sync.WaitGroup
	for i := 0; i < 100; i++ {
		flows.Add(1)
		go func(i int) {
			defer flows.Done()
			_ = fair.wait(ctx, "hog", fmt.Sprint(i), policyBurst)
		}(i)
	}
	for deadline := time.Now().Add(time.Second); ; {
		fair.mu.Lock()
		queued := fair.pending
		fair.mu.Unlock()
		if queued >= 80 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("competing flows did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	peerCtx, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	if err := fair.wait(peerCtx, "peer", "one-device", 32*1024); err != nil {
		t.Fatalf("one owner's flow count starved another owner: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("peer waited behind the dominant owner's connection backlog")
	}
	cancel()
	flows.Wait()
}

func TestFairSpeedDevicesShareOwnerAndIdleDevicesReserveNothing(t *testing.T) {
	limiter := rate.NewLimiter(512*1024, policyBurst)
	limiter.AllowN(time.Now(), policyBurst)
	fair := newFairSpeedLimiter(limiter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flows sync.WaitGroup
	for i := 0; i < 50; i++ {
		flows.Add(1)
		go func() { defer flows.Done(); _ = fair.wait(ctx, "same-user", "device-a", policyBurst) }()
	}
	for deadline := time.Now().Add(time.Second); ; {
		fair.mu.Lock()
		queued := fair.pending
		fair.mu.Unlock()
		if queued >= 40 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device backlog did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	peerCtx, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	if err := fair.wait(peerCtx, "same-user", "device-b", 32*1024); err != nil {
		t.Fatalf("one device monopolized the user's limit: %v", err)
	}
	cancel()
	flows.Wait()
	// A single remaining device can use the full rate; there is no fixed share
	// reserved for an idle or disconnected device.
	single := newFairSpeedLimiter(rate.NewLimiter(512*1024, policyBurst))
	start := time.Now()
	if err := single.wait(context.Background(), "same-user", "device-a", 256*1024); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond || elapsed > time.Second {
		t.Fatalf("single active device did not use the available ceiling: %v", elapsed)
	}
}

func TestFairSpeedCancellationRemovesPendingRequest(t *testing.T) {
	fair := newFairSpeedLimiter(rate.NewLimiter(1, policyBurst))
	fair.limiter.AllowN(time.Now(), policyBurst)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fair.wait(ctx, "owner", "device", policyBurst) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation result: %v", err)
	}
	fair.mu.Lock()
	defer fair.mu.Unlock()
	if fair.pending != 0 || len(fair.owners) != 0 {
		t.Fatal("cancelled flow retained its queue slot")
	}
}

func TestHostPolicyDeviceCapSharesConnectionsButNotOtherDevices(t *testing.T) {
	doc := v2Policy()
	doc.Groups["a"] = hostLimitPolicy{DeviceBytesPerSecond: policyBurst}
	first := hostEmail("1", hostA)
	second := strings.Replace(first, "0123456789abcdef01234567", "1123456789abcdef01234567", 1)
	host := doc.Hosts[hostA]
	host.AllowedIdentities[second] = true
	doc.Hosts[hostA] = host
	testHostPolicy(t, doc)
	if err := waitHostPolicy(policyContext("shared"), first, "down", policyBurst); err != nil {
		t.Fatal(err)
	}
	if err := waitHostPolicy(policyContext("shared"), second, "up", policyBurst); err != nil {
		t.Fatal("one device consumed another device's speed budget:", err)
	}
	ctx, cancel := context.WithTimeout(policyContext("shared"), 40*time.Millisecond)
	defer cancel()
	if err := waitHostPolicy(ctx, first, "up", policyBurst); err == nil {
		t.Fatal("another connection/direction bypassed the authenticated device cap")
	}
}
