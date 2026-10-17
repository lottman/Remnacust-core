//go:build !unix

package core

import "os"

func runtimeJournalOwned(os.FileInfo) bool { return true }
