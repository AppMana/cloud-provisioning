package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// These cases use a journal plan from the real API fixture. They establish
// import admission and durable replay behavior, not native deletion authority.
func verifyCloudLegacyImport(t *testing.T, original tunnel.ReceiveTransitionPlan) {
	t.Helper()
	t.Run("legacy-cloud-import", func(t *testing.T) {
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		key, err := wgtypes.GeneratePrivateKey()
		must(err)
		old := tunnel.PeerSpec{PublicKey: key.PublicKey().String(), Remote: true,
			Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{"172.29.99.1/32"}, RouteHosts: []string{"172.29.99.1"}}
		plan := original
		var base tunnel.PeerListDoc
		must(json.Unmarshal(plan.Base, &base))
		base.Peers = append(base.Peers, old)
		plan.Base, err = json.Marshal(base)
		must(err)
		// Ordinary cloud edges are unchanged by the receiver transition itself.
		plan.Desired.Peers = append(append([]tunnel.PeerSpec{}, plan.Desired.Peers...), old)
		current := receiveNeighborCheckpoint{InventoryUID: "inventory", MeshUID: plan.MeshUID,
			NodeUID: plan.CloudNodeUID, PublicKey: plan.RootPublicKey}
		path := filepath.Join(t.TempDir(), "neighbors")
		peers, markerPath, marker, err := pendingReceiveCloudLegacy(path, current, plan, []tunnel.ReceiveTransitionPlan{plan})
		must(err)
		if !reflect.DeepEqual(peers, []tunnel.PeerSpec{old}) || marker == nil {
			t.Fatalf("lost committed ordinary peer or included site transport: %+v", peers)
		}
		if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
			t.Fatal("admission consumed import before retirement intent was persisted")
		}
		// A crash before transfer completion must return the same exact intent.
		retry, _, _, err := pendingReceiveCloudLegacy(path, current, plan, []tunnel.ReceiveTransitionPlan{plan})
		must(err)
		if !reflect.DeepEqual(retry, peers) {
			t.Fatal("retry changed legacy permissions")
		}
		must(writeDurableJSON(markerPath, marker))
		// Once intent is transferred, replay must not re-authorize old deletion
		// after a replacement has acquired the retired source range.
		retry, _, next, err := pendingReceiveCloudLegacy(path, current, plan, []tunnel.ReceiveTransitionPlan{plan})
		must(err)
		if len(retry) != 0 || next != nil {
			t.Fatal("consumed legacy base was replayed")
		}
		for _, field := range []string{"inventory", "mesh", "node", "root"} {
			changed := current
			switch field {
			case "inventory":
				changed.InventoryUID += "-changed"
			case "mesh":
				changed.MeshUID += "-changed"
			case "node":
				changed.NodeUID += "-changed"
			case "root":
				changed.PublicKey = old.PublicKey
			}
			if _, _, _, err := pendingReceiveCloudLegacy(path, changed, plan, []tunnel.ReceiveTransitionPlan{plan}); err == nil {
				t.Fatal("accepted changed import identity", field)
			}
		}
		for _, raw := range []string{"{", "{}", string(mustJSON(t, marker)) + " {}"} {
			must(os.WriteFile(markerPath, []byte(raw), 0600))
			if _, _, _, err := pendingReceiveCloudLegacy(path, current, plan, []tunnel.ReceiveTransitionPlan{plan}); err == nil {
				t.Fatal("invalid import marker allowed replay")
			}
		}
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
