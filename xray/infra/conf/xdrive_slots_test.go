package conf_test

import (
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/transport/internet/xdrive"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestXDriveSlotOptInSurvivesSerializedConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		input := &conf.XDriveConfig{UseSlots: enabled, Service: "google-drive", RemoteFolder: "folder", Secrets: []string{"client", "secret", "refresh"}}
		built, err := input.Build()
		if err != nil {
			t.Fatal(err)
		}
		data, err := proto.Marshal(built)
		if err != nil {
			t.Fatal(err)
		}
		restored := new(xdrive.Config)
		if err := proto.Unmarshal(data, restored); err != nil {
			t.Fatal(err)
		}
		if restored.GetUseSlots() != enabled {
			t.Fatal("slot mode changed across config serialization")
		}
	}
}
