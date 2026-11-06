package main

import (
	"os"
	"runtime"
)

const coreProcessDisplayName = "rc-core"

func setProcessDisplayName() {
	if runtime.GOOS != "linux" {
		return
	}
	file, err := os.OpenFile("/proc/self/comm", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(coreProcessDisplayName)
}
