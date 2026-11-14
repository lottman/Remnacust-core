package xerahttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/testing/servers/tcp"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/browser_dialer"
	"github.com/xtls/xray-core/transport/internet/stat"
)

const adaptiveBrowserDriver = `
const {chromium}=require(process.env.XRAY_BROWSER_PLAYWRIGHT_PATH);
const config=JSON.parse(process.argv[1]);
(async()=>{
 const context=await chromium.launchPersistentContext(config.profile,{
  executablePath:process.env.XRAY_BROWSER_CHROME,headless:true,chromiumSandbox:true,serviceWorkers:'block',
  args:['--disable-background-networking','--disable-component-update','--disable-default-apps','--disable-sync','--disable-extensions','--no-first-run','--no-default-browser-check','--proxy-server='+config.proxy,'--proxy-bypass-list=localhost;127.0.0.1;[::1]','--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1, EXCLUDE ::1, EXCLUDE [::1]']
 });
 try {
  const page=await context.newPage();
  const response=await page.goto(config.url,{waitUntil:'load'});
  if(response.status()!==200)throw new Error('Browser helper did not load');
  await page.waitForFunction(()=>clientIdleCount===1);
  console.log('READY');
  process.stdin.resume();
  await new Promise(resolve=>process.stdin.on('end',resolve));
  await page.waitForFunction(()=>clientIdleCount===1 && upstreamGetCount===0 && upstreamPostCount===0,null,{timeout:5000});
 } finally {await context.close();}
})().catch(error=>{console.error(error.stack);process.exitCode=1});
`

func startAdaptiveBrowser(t *testing.T, url string) {
	t.Helper()
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "non-loopback requests are disabled", http.StatusForbidden)
	}))
	t.Cleanup(guard.Close)
	configuration, _ := json.Marshal(map[string]any{"url": url, "proxy": guard.URL, "profile": t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	command := exec.CommandContext(ctx, os.Getenv("XRAY_BROWSER_NODE"), "-e", adaptiveBrowserDriver, string(configuration))
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	completed := make(chan error, 1)
	go func() { completed <- command.Wait() }()
	t.Cleanup(func() {
		input.Close()
		select {
		case err := <-completed:
			if err != nil {
				t.Errorf("Chrome adaptive integration failed: %v\n%s", err, stderr.String())
			}
		case <-time.After(12 * time.Second):
			cancel()
			<-completed
			t.Error("Chrome adaptive integration did not stop")
		}
		cancel()
	})
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("Chrome helper failed before readiness")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("Chrome helper readiness timed out")
	}
}

func TestAdaptiveBrowserDialerChrome(t *testing.T) {
	for _, name := range []string{"XRAY_BROWSER_NODE", "XRAY_BROWSER_CHROME", "XRAY_BROWSER_PLAYWRIGHT_PATH"} {
		if os.Getenv(name) == "" {
			if os.Getenv("REMNACUST_REQUIRE_BROWSER_TESTS") == "1" {
				t.Fatalf("required Chrome dependency %s is missing", name)
			}
			t.Skipf("set %s to enable isolated real-Chrome Xera adaptive integration", name)
		}
	}
	helperAddress := "127.0.0.1:" + tcp.PickPort().String()
	t.Cleanup(browser_dialer.Reload)
	t.Setenv(platform.BrowserDialerAddress, helperAddress)
	browser_dialer.Reload()
	helperURL := "http://" + helperAddress
	leaseEventually(t, func() bool {
		response, err := http.Get(helperURL)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 200
	})
	startAdaptiveBrowser(t, helperURL)
	for _, address := range []net.Address{net.LocalHostIP, net.LocalHostIPv6} {
		t.Run(address.String(), func(t *testing.T) {
			if address.Family().IsIPv6() {
				requireXeraIPv6Loopback(t)
			}
			server := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/adaptive-browser"}}
			settings := &internet.MemoryStreamConfig{ProtocolName: protocolName, ProtocolSettings: &Config{Path: "/adaptive-browser", Mode: ModeStreamAuto, Adaptive: &AdaptiveConfig{Mode: AdaptiveBalanced, MinStreams: 1, MaxStreams: 1}, UplinkDataPlacement: PlacementBody, ScMinPostsIntervalMs: &RangeConfig{From: -1, To: -1}}}
			t.Cleanup(func() { settings.Close(); server.Close() })
			port := tcp.PickPort()
			listener, err := ListenXH(context.Background(), address, port, server, func(connection stat.Connection) {
				go func() { defer connection.Close(); io.Copy(connection, connection) }()
			})
			if err != nil {
				adaptiveLoopbackError(t, err)
			}
			defer listener.Close()
			dest := net.TCPDestination(address, port)
			for range 3 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				connection, err := Dial(ctx, dest, settings)
				cancel()
				if err != nil {
					adaptiveLoopbackError(t, err)
				}
				connection.SetDeadline(time.Now().Add(5 * time.Second))
				payload := bytes.Repeat([]byte("browser-adaptive"), 4096)
				written := make(chan error, 1)
				go func() { _, err := connection.Write(payload); written <- err }()
				got := make([]byte, len(payload))
				_, readErr := io.ReadFull(connection, got)
				connection.Close()
				writeErr := <-written
				if readErr != nil || writeErr != nil || !bytes.Equal(got, payload) {
					t.Fatalf("browser payload: read=%v write=%v", readErr, writeErr)
				}
			}
			globalDialerAccess.Lock()
			manager := globalDialerMap[dialerConf{dest, settings}]
			nativePool := manager != nil && len(manager.xmuxClients) != 0
			globalDialerAccess.Unlock()
			if manager == nil || manager.adaptive == nil || nativePool {
				t.Fatal("Browser Dialer bypassed admission or created a native TCP pool")
			}
			leaseEventually(t, func() bool {
				_, active, queued := adaptiveSnapshot(manager.adaptive)
				return active == [2]int{} && queued == 0
			})
		})
	}
}
