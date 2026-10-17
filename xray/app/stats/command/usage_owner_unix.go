//go:build unix

package command

import (
	"os"
	"syscall"
)

func usageOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
