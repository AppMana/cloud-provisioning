package k3s

import (
	"reflect"
	"testing"

	"github.com/coreos/go-systemd/v22/unit"
)

func TestNativeServiceOptionsRoundTrip(t *testing.T) {
	for _, args := range []string{"server --cluster-init", "agent --server https://192.0.2.1:6443 --token supplied-token"} {
		options := serviceOptions("explicit-node", args)
		got, err := unit.Deserialize(unit.Serialize(options))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, options) {
			t.Fatal("native unit serialization changed options")
		}
		values := map[string]string{}
		for _, option := range got {
			values[option.Section+"/"+option.Name] = option.Value
		}
		if values["Service/ExecStart"] != "/usr/local/bin/k3s "+args || values["Service/Type"] != "notify" || values["Service/Delegate"] != "yes" || values["Service/KillMode"] != "process" {
			t.Fatal("native k3s service behavior changed")
		}
		if values["Service/Restart"] != "always" || values["Install/WantedBy"] != "multi-user.target" {
			t.Fatal("reboot supervision changed")
		}
	}
}
