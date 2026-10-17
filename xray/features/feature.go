package features

import (
	"github.com/xtls/xray-core/common"
)

// Feature is the interface for Xray features. All features must implement this interface.
// All existing features have an implementation in app directory. These features can be replaced by third-party ones.
type Feature interface {
	common.HasType
	common.Runnable
}

// StartPreparer is optional. A server or feature scope runs every preparation
// before starting any feature, so admission policy is ready before listeners.
type StartPreparer interface {
	PrepareStart() error
}
