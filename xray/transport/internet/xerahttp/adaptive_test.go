package xerahttp

import (
	"context"
	"errors"
	"github.com/apernet/quic-go"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/transport/internet"
)

type adaptiveTestClock struct{ value atomic.Int64 }

func adaptiveTestController(mode string, minimum, maximum int32) (*adaptiveController, *adaptiveTestClock) {
	clock := new(adaptiveTestClock)
	clock.value.Store(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	c := newAdaptiveController(&AdaptiveConfig{Mode: mode, MinStreams: minimum, MaxStreams: maximum})
	c.now = func() time.Time { return time.Unix(0, clock.value.Load()) }
	c.windowAt = c.now()
	c.startedAt = c.windowAt
	return c, clock
}

func (clock *adaptiveTestClock) advance(d time.Duration) { clock.value.Add(int64(d)) }

func adaptiveSnapshot(c *adaptiveController) (limits, active [2]int, queued int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.directions {
		limits[i], active[i] = c.directions[i].limit, c.directions[i].active
	}
	return limits, active, len(c.waiters)
}

func adaptiveTick(c *adaptiveController, clock *adaptiveTestClock) {
	clock.advance(time.Second)
	c.mu.Lock()
	c.stepLocked(c.now())
	c.mu.Unlock()
}

func adaptiveAcquire(t *testing.T, c *adaptiveController, mask int) *adaptiveLease {
	t.Helper()
	lease, err := c.acquire(context.Background(), mask)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.finish(nil) })
	return lease
}

func adaptivePressure(t *testing.T, c *adaptiveController, mask, count int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	leases := make(chan *adaptiveLease, count)
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, _ := c.acquire(ctx, mask)
			if lease != nil {
				leases <- lease
			}
		}()
	}
	t.Cleanup(func() {
		cancel()
		group.Wait()
		close(leases)
		for lease := range leases {
			lease.finish(nil)
		}
	})
	leaseEventually(t, func() bool { _, _, queued := adaptiveSnapshot(c); return queued == count })
}

func TestAdaptiveInitialBudgetAndValidation(t *testing.T) {
	for _, bounds := range []struct {
		min, max int32
		initial  int
	}{{1, 1, 1}, {4, 32, 8}, {1, 3, 3}, {12, 16, 12}, {32, 32, 32}} {
		for _, mode := range []string{AdaptiveBalanced, AdaptiveLowLatency, AdaptiveUnstableNetwork} {
			config := &AdaptiveConfig{Mode: mode, MinStreams: bounds.min, MaxStreams: bounds.max}
			if err := config.Validate(); err != nil {
				t.Fatal(err)
			}
			c := newAdaptiveController(config)
			limits, _, _ := adaptiveSnapshot(c)
			if limits != [2]int{bounds.initial, bounds.initial} {
				t.Fatalf("initial budget: %v", limits)
			}
		}
	}
	for _, config := range []*AdaptiveConfig{{Mode: "auto", MinStreams: 4, MaxStreams: 32}, {Mode: AdaptiveBalanced, MaxStreams: 32}, {Mode: AdaptiveBalanced, MinStreams: 4, MaxStreams: 3}, {Mode: AdaptiveBalanced, MinStreams: 1, MaxStreams: 33}} {
		if err := (&Config{Adaptive: config}).Validate(); err == nil {
			t.Fatalf("accepted %v", config)
		}
	}
	if newAdaptiveController(nil) != nil {
		t.Fatal("legacy config enabled adaptation")
	}
}

func TestAdaptiveIndependentDirectionsAndAtomicDuplex(t *testing.T) {
	c, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	down := adaptiveAcquire(t, c, adaptiveDown)
	up := adaptiveAcquire(t, c, adaptiveUp)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { lease, err := c.acquire(ctx, adaptiveDown|adaptiveUp); lease.finish(nil); result <- err }()
	leaseEventually(t, func() bool { _, _, n := adaptiveSnapshot(c); return n == 1 })
	down.finish(nil)
	_, active, _ := adaptiveSnapshot(c)
	if active != [2]int{0, 1} {
		t.Fatalf("duplex partially reserved a direction: %v", active)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	up.finish(nil)
	duplex := adaptiveAcquire(t, c, adaptiveDown|adaptiveUp)
	_, active, _ = adaptiveSnapshot(c)
	if active != [2]int{1, 1} {
		t.Fatal(active)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() { defer group.Done(); duplex.finish(nil) }()
	}
	group.Wait()
	_, active, _ = adaptiveSnapshot(c)
	if active != [2]int{} {
		t.Fatalf("duplicate release: %v", active)
	}
}

func TestAdaptiveLongLivedProgressGrowsWithoutEOF(t *testing.T) {
	for _, test := range []struct {
		mode    string
		windows int
	}{{AdaptiveBalanced, 3}, {AdaptiveLowLatency, 2}, {AdaptiveUnstableNetwork, 5}} {
		t.Run(test.mode, func(t *testing.T) {
			c, clock := adaptiveTestController(test.mode, 4, 32)
			var held []*adaptiveLease
			for range 8 {
				held = append(held, adaptiveAcquire(t, c, adaptiveDown))
			}
			clock.advance(time.Millisecond)
			for _, lease := range held {
				lease.confirm()
			}
			adaptivePressure(t, c, adaptiveDown, 16)
			for window := 1; window <= test.windows; window++ {
				for _, lease := range held {
					lease.progress(adaptiveDown, 1000)
				}
				adaptiveTick(c, clock)
				limits, active, _ := adaptiveSnapshot(c)
				want := 8
				if window == test.windows {
					want = 9
				}
				if limits[0] != want || limits[1] != 8 || active[0] < 8 {
					t.Fatalf("window %d limits=%v active=%v", window, limits, active)
				}
			}
		})
	}
}

func TestAdaptiveHeadersWithoutPayloadDoNotGrow(t *testing.T) {
	c, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
	var held []*adaptiveLease
	for range 8 {
		held = append(held, adaptiveAcquire(t, c, adaptiveDown))
	}
	clock.advance(time.Millisecond)
	for _, lease := range held {
		lease.confirm()
	}
	adaptivePressure(t, c, adaptiveDown, 16)
	for range 12 {
		adaptiveTick(c, clock)
	}
	limits, _, _ := adaptiveSnapshot(c)
	if limits != [2]int{8, 8} {
		t.Fatalf("headers or connection establishment grew budget: %v", limits)
	}
}

func TestAdaptiveQuotaPlateauStopsOscillation(t *testing.T) {
	c, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
	var held []*adaptiveLease
	for range 8 {
		held = append(held, adaptiveAcquire(t, c, adaptiveUp))
	}
	clock.advance(time.Millisecond)
	for _, lease := range held {
		lease.confirm()
	}
	adaptivePressure(t, c, adaptiveUp, 24)
	for window := 1; window <= 60; window++ {
		for _, lease := range held {
			lease.progress(adaptiveUp, 1000)
		}
		adaptiveTick(c, clock)
		limits, _, _ := adaptiveSnapshot(c)
		want := 8
		if window == 3 {
			want = 9
		}
		if limits[1] != want {
			t.Fatalf("quota plateau oscillated at window %d: %v", window, limits)
		}
	}
	for range 3 {
		for _, lease := range held {
			lease.progress(adaptiveUp, 1500)
		}
		adaptiveTick(c, clock)
	}
	limits, _, _ := adaptiveSnapshot(c)
	if limits[1] != 9 {
		t.Fatalf("new measured capacity did not reopen growth: %v", limits)
	}
}

func TestAdaptiveStatusBackoffCooldownAndShrinkDrain(t *testing.T) {
	for _, test := range []struct {
		mode              string
		reduced, cooldown int
	}{{AdaptiveBalanced, 6, 3}, {AdaptiveLowLatency, 6, 3}, {AdaptiveUnstableNetwork, 4, 10}} {
		t.Run(test.mode, func(t *testing.T) {
			c, clock := adaptiveTestController(test.mode, 4, 32)
			var held []*adaptiveLease
			for range 8 {
				held = append(held, adaptiveAcquire(t, c, adaptiveUp))
			}
			clock.advance(time.Millisecond)
			for _, lease := range held {
				lease.confirm()
			}
			adaptivePressure(t, c, adaptiveUp, 12)
			held[0].reject(503)
			adaptiveTick(c, clock)
			limits, active, _ := adaptiveSnapshot(c)
			if limits[1] != test.reduced || active[1] != 8 {
				t.Fatalf("shrink aborted requests: %v %v", limits, active)
			}
			for range test.cooldown - 1 {
				for _, lease := range held {
					lease.progress(adaptiveUp, 1000)
				}
				adaptiveTick(c, clock)
				limits, _, _ = adaptiveSnapshot(c)
				if limits[1] != test.reduced {
					t.Fatalf("grew during cooldown: %v", limits)
				}
			}
			for _, lease := range held {
				lease.finish(nil)
			}
		})
	}
}

func TestAdaptiveLatencyAndPermanentStatusAreDistinct(t *testing.T) {
	c, clock := adaptiveTestController(AdaptiveLowLatency, 4, 32)
	first := adaptiveAcquire(t, c, adaptiveDown)
	clock.advance(10 * time.Millisecond)
	first.confirm()
	first.finish(nil)
	second := adaptiveAcquire(t, c, adaptiveDown)
	clock.advance(200 * time.Millisecond)
	second.confirm()
	adaptiveTick(c, clock)
	limits, _, _ := adaptiveSnapshot(c)
	if limits[0] != 6 {
		t.Fatalf("measured HTTP response latency did not reduce budget: %v", limits)
	}
	for _, status := range []int{404, 409, 413} {
		c, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
		lease := adaptiveAcquire(t, c, adaptiveUp)
		lease.reject(status)
		lease.finish(errors.New("rejected"))
		adaptiveTick(c, clock)
		limits, _, _ := adaptiveSnapshot(c)
		if limits[1] != 8 {
			t.Fatalf("permanent HTTP %d was treated as retryable congestion", status)
		}
	}
}

func TestAdaptiveRetirementAndQueueCapacity(t *testing.T) {
	c, _ := adaptiveTestController(AdaptiveBalanced, 1, 1)
	held := adaptiveAcquire(t, c, adaptiveDown)
	adaptivePressure(t, c, adaptiveDown, maxAdaptiveWaiters)
	if lease, err := c.acquire(context.Background(), adaptiveDown); lease != nil || !errors.Is(err, errAdaptiveQueueFull) {
		t.Fatalf("unbounded queue: %v %v", lease, err)
	}
	up := adaptiveAcquire(t, c, adaptiveUp)
	up.finish(nil)
	c.retire()
	_, active, _ := adaptiveSnapshot(c)
	if active[0] != 1 {
		t.Fatal("retirement aborted an active body")
	}
	if lease, err := c.acquire(context.Background(), adaptiveUp); lease != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("retired controller accepted request: %v", err)
	}
	held.finish(io.EOF)
}

func TestAdaptiveDestinationGenerationAndRotationScope(t *testing.T) {
	config := &Config{Adaptive: &AdaptiveConfig{Mode: AdaptiveBalanced, MinStreams: 4, MaxStreams: 32}, Xmux: &XmuxConfig{CMaxReuseTimes: &RangeConfig{From: 1, To: 1}}}
	settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
	other := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
	t.Cleanup(func() { settings.Close(); other.Close() })
	dest := xnet.TCPDestination(xnet.LocalHostIP, 1)
	first, a := getHTTPClient(context.Background(), dest, settings)
	second, b := getHTTPClient(context.Background(), dest, settings)
	third, d := getHTTPClient(context.Background(), dest, other)
	fourth, e := getHTTPClient(context.Background(), xnet.TCPDestination(xnet.LocalHostIPv6, 1), settings)
	defer releaseHTTPClient(a, false)
	defer releaseHTTPClient(b, false)
	defer releaseHTTPClient(d, false)
	defer releaseHTTPClient(e, false)
	controller := first.(*DefaultDialerClient).adaptive
	if first == second || controller == nil || controller != second.(*DefaultDialerClient).adaptive || controller == third.(*DefaultDialerClient).adaptive || controller == fourth.(*DefaultDialerClient).adaptive {
		t.Fatal("adaptive scope was tied to a physical client or shared across destinations/generations")
	}
	settings.Close()
	if lease, err := controller.acquire(context.Background(), adaptiveDown); lease != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatal("generation retirement retained admission")
	}
}

func TestAdaptiveDownloadSettingsInheritWithoutMutation(t *testing.T) {
	for _, override := range []bool{false, true} {
		childConfig := &Config{Mode: ModePacketUp}
		if override {
			childConfig.Adaptive = &AdaptiveConfig{Mode: AdaptiveLowLatency, MinStreams: 1, MaxStreams: 3}
		}
		config := &Config{Mode: ModePacketUp, Adaptive: &AdaptiveConfig{Mode: AdaptiveBalanced, MinStreams: 4, MaxStreams: 32}, DownloadSettings: &internet.StreamConfig{ProtocolName: protocolName, Address: xnet.NewIPOrDomain(xnet.LocalHostIPv6), Port: 443, TransportSettings: []*internet.TransportConfig{{ProtocolName: protocolName, Settings: serial.ToTypedMessage(childConfig)}}}}
		settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: config}
		t.Cleanup(func() { settings.Close() })
		child, err := prepareDownloadSettings(settings, config)
		if err != nil {
			t.Fatal(err)
		}
		main, a := getHTTPClient(context.Background(), xnet.TCPDestination(xnet.LocalHostIP, 443), settings)
		down, b := acquireHTTPClient(context.Background(), *child.Destination, child, settings, false)
		defer releaseHTTPClient(a, false)
		defer releaseHTTPClient(b, false)
		mainController := main.(*DefaultDialerClient).adaptive
		controller := down.(*DefaultDialerClient).adaptive
		wantMax, wantMode := 32, AdaptiveBalanced
		if override {
			wantMax, wantMode = 3, AdaptiveLowLatency
		}
		if controller == nil || controller == mainController || controller.maximum != wantMax || controller.mode != wantMode {
			t.Fatalf("download lost independent inherited/overridden policy: %v", controller)
		}
		if !override && child.ProtocolSettings.(*Config).Adaptive != nil {
			t.Fatal("runtime inheritance mutated the stored child configuration")
		}
	}
}

func TestAdaptivePassiveRTTIsSampledAndNeverInvented(t *testing.T) {
	c, clock := adaptiveTestController(AdaptiveLowLatency, 4, 32)
	lease := adaptiveAcquire(t, c, adaptiveDown)
	rtt := 10 * time.Millisecond
	var samples atomic.Int32
	lease.passiveRTT = func() time.Duration { samples.Add(1); return rtt }
	clock.advance(time.Millisecond)
	lease.confirm()
	for range 100 {
		lease.progress(adaptiveDown, 1)
	}
	if samples.Load() != 1 {
		t.Fatalf("RTT sampling was unbounded: %d", samples.Load())
	}
	rtt = 200 * time.Millisecond
	adaptiveTick(c, clock)
	lease.progress(adaptiveDown, 1000)
	adaptiveTick(c, clock)
	limits, _, _ := adaptiveSnapshot(c)
	if limits[0] != 6 {
		t.Fatalf("measured passive RTT inflation was ignored: %v", limits)
	}
	unknown, otherClock := adaptiveTestController(AdaptiveLowLatency, 4, 32)
	other := adaptiveAcquire(t, unknown, adaptiveDown)
	other.passiveRTT = func() time.Duration { return 0 }
	otherClock.advance(time.Millisecond)
	other.confirm()
	other.progress(adaptiveDown, 100)
	unknown.mu.Lock()
	defer unknown.mu.Unlock()
	if unknown.directions[0].rtt != 0 || unknown.directions[0].rttBaseline != 0 {
		t.Fatal("missing RTT was replaced with an invented sample")
	}
}

func TestAdaptiveConcurrentHardCeiling(t *testing.T) {
	c, _ := adaptiveTestController(AdaptiveBalanced, 4, 32)
	var group sync.WaitGroup
	var failures atomic.Int32
	for index := range 128 {
		group.Add(1)
		go func() {
			defer group.Done()
			mask := adaptiveDown
			if index%3 == 1 {
				mask = adaptiveUp
			} else if index%3 == 2 {
				mask |= adaptiveUp
			}
			lease, err := c.acquire(context.Background(), mask)
			if err != nil {
				failures.Add(1)
				return
			}
			_, active, _ := adaptiveSnapshot(c)
			if active[0] > 8 || active[1] > 8 || active[0] < 0 || active[1] < 0 {
				failures.Add(1)
			}
			lease.confirm()
			lease.progress(mask&-mask, 100)
			lease.finish(nil)
			lease.finish(io.EOF)
		}()
	}
	group.Wait()
	_, active, queued := adaptiveSnapshot(c)
	if failures.Load() != 0 || active != [2]int{} || queued != 0 {
		t.Fatalf("concurrent permits escaped bounds: failures=%d active=%v queued=%d", failures.Load(), active, queued)
	}
}

func TestAdaptiveSustainedMeasuredGrowthRespectsMaximum(t *testing.T) {
	c, clock := adaptiveTestController(AdaptiveBalanced, 4, 32)
	var held []*adaptiveLease
	for range 8 {
		held = append(held, adaptiveAcquire(t, c, adaptiveDown))
	}
	clock.advance(time.Millisecond)
	for _, lease := range held {
		lease.confirm()
	}
	adaptivePressure(t, c, adaptiveDown, 64)
	for range 140 {
		limits, _, _ := adaptiveSnapshot(c)
		for _, lease := range held {
			lease.progress(adaptiveDown, limits[0]*1000)
		}
		adaptiveTick(c, clock)
		limits, active, _ := adaptiveSnapshot(c)
		if limits[0] > 32 || limits[0] < 4 || active[0] > 32 {
			t.Fatalf("escaped configured maximum: limits=%v active=%v", limits, active)
		}
	}
	limits, _, _ := adaptiveSnapshot(c)
	if limits[0] != 32 {
		t.Fatalf("measured growth did not reach the configured ceiling: %v", limits)
	}
}

func TestAdaptiveRejectsProvisionalQUICRTT(t *testing.T) {
	for _, stats := range []quic.ConnectionStats{{}, {LatestRTT: 100 * time.Millisecond, SmoothedRTT: 100 * time.Millisecond}, {LatestRTT: 20 * time.Millisecond, SmoothedRTT: 20 * time.Millisecond}} {
		if measuredQUICRTT(stats) != 0 {
			t.Fatal("QUIC startup or restored estimate was used as measured RTT")
		}
	}
	stats := quic.ConnectionStats{LatestRTT: 20 * time.Millisecond, SmoothedRTT: 25 * time.Millisecond, MeanDeviation: 2 * time.Millisecond}
	if measuredQUICRTT(stats) != 25*time.Millisecond {
		t.Fatal("measured QUIC RTT was discarded")
	}
}
