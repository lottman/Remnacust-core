//go:build unix && !wasm

package dispatcher

import (
	"os"
	"syscall"
)

func quotaOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
