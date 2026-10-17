package core

import "github.com/xtls/xray-core/common/uuid"

var processID = func() string {
	id := uuid.New()
	return id.String()
}()

// ProcessID binds control acknowledgements to one process, including all of
// its native runtime generations. PIDs alone can repeat after a restart.
func ProcessID() string { return processID }
