package main

import (
	"fmt"
	"maps"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Released roots remain exclusions when active journals are planned. These
// records remove restoration authority only; checkpoint admission must still
// authenticate retained history and local fences before any native execution.
// Do not carry released address ranges into active reservations: a new key can
// own those addresses after the separately admitted release protocol.
func appendReleasedReceiveExclusions(inventory tunnel.ReceiveInventory, routes *tunnel.ReceiveJournalRoutes) error {
	if len(inventory.Released) == 0 {
		return nil
	}
	if routes == nil {
		return fmt.Errorf("released exclusions require a route plan")
	}
	if err := inventory.Validate(); err != nil {
		return err
	}
	withdrawn := maps.Clone(routes.Withdrawn)
	if withdrawn == nil {
		withdrawn = map[string]string{}
	}
	for uid, p := range inventory.Released {
		w, err := attachment.RecordedReceiveWithdrawal(p.Retirement, uid)
		if err != nil {
			return err
		}
		key, err := wgtypes.ParseKey(w.PublicKey)
		if err != nil || key == (wgtypes.Key{}) || w.MeshNamespace != inventory.Namespace || w.MeshName != inventory.MeshName || w.MeshUID != inventory.MeshUID {
			return fmt.Errorf("released exclusion identity differs from inventory")
		}
		for active, plan := range routes.Plans {
			if active == uid || plan.RootPublicKey == w.PublicKey {
				return fmt.Errorf("released owner reappeared in active routes")
			}
		}
		if old, found := withdrawn[uid]; found && old != w.PublicKey {
			return fmt.Errorf("released exclusion conflicts with withdrawal")
		}
		withdrawn[uid] = w.PublicKey
	}
	routes.Withdrawn = withdrawn
	return nil
}
