//go:build !unix && !wasm

package dispatcher

import "os"

func quotaOwned(os.FileInfo) bool { return true }
