package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Pure projection: neither live withdrawal authority nor native route absence
// is implied. All journal owners remain present in the input inventory.
func TestReceiveSourceWithdrawalProjection(t *testing.T) {
	for _, fault := range []string{"none", "missing-owner", "missing-node", "allocation", "missing-projection"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			other, _ := wgtypes.GeneratePrivateKey()
			first := receiveRestoreTestPlan(t, root, site)
			second := receiveRestoreTestPlan(t, other, site)
			raw, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.NewReplacer("172.29.0.1", "172.29.0.3", "fd29::1", "fd29::3", "worker-uid", "other-worker-uid", "cloud-uid", "other-cloud-uid").Replace(string(raw))
			if err := json.Unmarshal([]byte(changed), &second); err != nil {
				t.Fatal(err)
			}
			second.MachineName = "other-worker"
			second.CloudNodeName = "other-cloud"
			wanted, node := first.RootPublicKey, "site-uid"
			switch fault {
			case "missing-owner":
				wanted = site.PublicKey().String()
			case "missing-node":
				node = "replacement"
			case "allocation":
				member := second.Sites[site.PublicKey().String()]
				member.Legacy.Tunnel.Addresses = []string{"10.90.0.3/24", "fd90::3/64"}
				member.Forward.Tunnel.Addresses = member.Legacy.Tunnel.Addresses
				second.Sites[site.PublicKey().String()] = member
			case "missing-projection":
				member := second.Sites[site.PublicKey().String()]
				member.Legacy.Tunnel = nil
				second.Sites[site.PublicKey().String()] = member
			}
			plans := []tunnel.ReceiveTransitionPlan{first, second}
			before, _ := json.Marshal(plans)
			cfg, addresses, hosts, err := planReceiveSourceWithdrawal(config{iface: "legacy", routeTable: 518, fwmark: 518}, plans, wanted, node)
			if fault != "none" {
				if err == nil {
					t.Fatal("invalid projection admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(hosts) != 0 || len(cfg.receiveRouteScope) != 2 || len(cfg.receiveSourceCandidates) != 0 || len(addresses) != 2 || cfg.receiveSourceRoutes != nil {
				t.Fatal("lost removal scope or survivor candidates")
			}
			if !cfg.receiveRouteScope["172.29.0.1/32"] || cfg.receiveRouteScope["172.29.0.3/32"] {
				t.Fatal("survivor added to removal scope")
			}
			after, _ := json.Marshal(plans)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("mutated admitted plans")
			}
		})
	}
}
