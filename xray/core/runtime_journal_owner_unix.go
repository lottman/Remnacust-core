//go:build unix

package core

import (
	"os"
	"syscall"
)

func runtimeJournalOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
