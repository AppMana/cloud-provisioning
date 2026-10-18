package main

import (
	"fmt"
	"reflect"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// A relay must finish its own direct switch before a follower uses it. Otherwise
// the previous owner and successor can forward to each other during an ordinary
// asynchronous switch. Read progress only from admitted controller journals;
// receiver handshakes and prepare receipts do not prove forwarding completion.
func verifyReceiveRelayDependencies(intent tunnel.ReceiveJournalRoutes, nodeUID, target string, targetView handover.View, now time.Time) error {
	return visitReceiveRelayDependencies(intent, nodeUID, target, targetView, now, nil)
}

func visitReceiveRelayDependencies(intent tunnel.ReceiveJournalRoutes, nodeUID, target string, targetView handover.View, now time.Time, check func(tunnel.ReceiveTransitionSite, handover.Receipt) error) error {
	fail := func(reason string) error { return fmt.Errorf("relay forwarding dependency: %s", reason) }
	for machine, plan := range intent.Plans {
		_, legacy := intent.Legacy[machine]
		if plan.PreviousReceiver != nil {
			view, found := intent.Views[machine]
			if !found || legacy || (machine == target && !reflect.DeepEqual(view, targetView)) {
				return fail("successor receiver selection missing or changed")
			}
			active, root, err := plan.SelectedReceiveTransport(view)
			if err != nil || root {
				return fail("successor receiver generation differs")
			}
			plan = active
		}
		for _, site := range plan.Sites {
			if site.NodeUID != nodeUID {
				continue
			}
			path := site.Forward
			if legacy {
				path = site.Legacy
			}
			if path == nil || path.Transit == nil {
				continue
			}
			view, found := intent.Views[machine]
			if !found || (machine == target && !reflect.DeepEqual(view, targetView)) {
				return fail("committed round evidence missing or changed")
			}
			// Completed Switch barriers in replay-validated histories prove that all
			// peers passed the ordering check. Prepare/Abort retain initial forwarding.
			if view.Complete || view.Round.Phase != handover.Switch {
				continue
			}
			if intent.Plans[machine].PreviousReceiver == nil && legacy != view.RollbackRequested {
				return fail("direction differs from committed round")
			}
			owner, found := plan.Sites[path.PeerPublicKey]
			if !found || owner.NodeUID == nodeUID {
				return fail("relay identity missing or self-referential")
			}
			ownerPath := owner.Forward
			if legacy {
				ownerPath = owner.Legacy
			}
			if ownerPath == nil || ownerPath.Transit != nil || ownerPath.PeerPublicKey != path.PeerPublicKey {
				return fail("selected relay has no committed direct path")
			}
			var proofs []handover.AuthenticatedReceipt
			required := 0
			for _, member := range view.Round.Members {
				if member.NodeUID != owner.NodeUID {
					continue
				}
				required++
				for _, receipt := range view.Observed {
					if receipt.NodeUID == member.NodeUID && receipt.DeviceID == member.DeviceID {
						proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: receipt.NodeUID, Receipt: receipt})
					}
				}
			}
			if required == 0 || len(proofs) != required {
				return fail("selected relay has not verified this switch")
			}
			if err := view.Round.ValidateReceipts(now, proofs); err != nil {
				return fail("selected relay observation is stale or does not bind this round")
			}
			if check != nil {
				for _, proof := range proofs {
					if err := check(owner, proof.Receipt); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
