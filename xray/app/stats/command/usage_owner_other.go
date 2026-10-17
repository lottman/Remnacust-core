//go:build !unix

package command

import "os"

func usageOwned(info os.FileInfo) bool { return true }
