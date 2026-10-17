package conf_test

import (
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
	"testing"
)

func TestUserQuotaMandatoryMetadataSurvivesMultiFileOverrides(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		first, second := &conf.Config{}, &conf.Config{RemnacustUserQuotaRequired: true}
		if reverse {
			first, second = second, first
		}
		first.Override(second, "fixture")
		built, err := first.Build()
		if err != nil {
			t.Fatal(err)
		}
		if required, err := core.ConfigRequiresUserQuota(built); err != nil || !required {
			t.Fatal("merging configuration files dropped mandatory server quotas", err)
		}
	}
}
