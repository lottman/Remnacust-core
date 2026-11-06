package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProcessDisplayNamePreservesExecutableAndArguments(t *testing.T) {
	if os.Getenv("REMNACUST_TEST_PROCESS_NAME") == "1" {
		executable, err := os.Readlink("/proc/self/exe")
		if err != nil {
			t.Fatal(err)
		}
		command, err := os.ReadFile("/proc/self/cmdline")
		if err != nil {
			t.Fatal(err)
		}
		arguments := append([]string(nil), os.Args...)
		done := make(chan struct{})
		go func() {
			setProcessDisplayName()
			close(done)
		}()
		<-done
		name, err := os.ReadFile("/proc/self/comm")
		if err != nil || strings.TrimSpace(string(name)) != "rc-core" {
			t.Fatalf("display name = %q: %v", name, err)
		}
		currentExecutable, err := os.Readlink("/proc/self/exe")
		if err != nil || currentExecutable != executable {
			t.Fatalf("executable changed: %q -> %q: %v", executable, currentExecutable, err)
		}
		currentCommand, err := os.ReadFile("/proc/self/cmdline")
		if err != nil || !bytes.Equal(currentCommand, command) || !reflect.DeepEqual(os.Args, arguments) {
			t.Fatalf("process arguments changed: %v", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessDisplayNamePreservesExecutableAndArguments$", "--", "compatibility-rw-core")
	command.Env = append(os.Environ(), "REMNACUST_TEST_PROCESS_NAME=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("display name subprocess failed: %v\n%s", err, output)
	}
}
