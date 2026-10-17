//go:build wasm

package dispatcher

import (
	"context"
	"errors"
	"github.com/xtls/xray-core/core"
)

// WASM clients do not read node-side host policy files.
func startHostPolicy() error { return nil }

func prepareHostPolicy(ctx context.Context) error {
	if ctx != nil {
		if instance := core.FromContext(ctx); instance != nil && instance.RequiresUserQuota() {
			return errors.New("server user quotas are not supported by WASM clients")
		}
	}
	return nil
}
