package main

import (
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// The caller admits the complete inventory, ancestry, mesh and local incarnation
// under the host lock. This binds one follower path to the selected generation
// and reuses the participant switch barrier. check must verify the live relay
// incarnation; a transport handshake cannot substitute for its switch receipt.
// Native ownership, withdrawal and durable recovery remain separate admission.
func admitReceiveFollowerJournalPath(data map[string][]byte, nodeName, nodeUID string, snapshot handover.Snapshot, clusterUID string, now time.Time, check func(tunnel.ReceiveTransitionSite, handover.Receipt) error) (tunnel.ReceiveSitePath, error) {
	fail := func(err error) (tunnel.ReceiveSitePath, error) { return tunnel.ReceiveSitePath{}, err }
	if snapshot.WithdrawalIntentHash() != "" {
		return fail(fmt.Errorf("interrupted follower journal is fenced for withdrawal"))
	}
	if nodeName == "" || nodeUID == "" {
		return fail(fmt.Errorf("follower requires exact local Node identity"))
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return fail(err)
	}
	if string(data[tunnel.PeerPublicKeyPrefix+plan.MachineName]) != plan.RootPublicKey {
		return fail(fmt.Errorf("follower worker publication is absent or replaced"))
	}
	if plan.CloudNodeName == nodeName || plan.CloudNodeUID == nodeUID {
		return fail(fmt.Errorf("receiver owner cannot use follower routing"))
	}
	for _, site := range plan.Sites {
		if site.NodeName == nodeName || site.NodeUID == nodeUID {
			return fail(fmt.Errorf("receiver participant cannot use follower routing"))
		}
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		return fail(err)
	}
	if !view.Complete && (now.Before(view.Round.IssuedAt) || !now.Before(view.Round.ExpiresAt)) {
		return fail(fmt.Errorf("follower journal round is not currently executable"))
	}
	path, err := tunnel.PlanReceiveJournalFollowerPath(data, nodeName, snapshot, clusterUID)
	if err != nil {
		return fail(err)
	}
	selected, root, err := plan.SelectedReceiveTransport(view)
	if err != nil {
		return fail(err)
	}
	owner, found := selected.Sites[path.PeerPublicKey]
	if !found || owner.NodeUID == "" || owner.NodeName == "" || owner.NodeUID == nodeUID || owner.NodeName == nodeName || path.Transit == nil || path.Tunnel != nil {
		return fail(fmt.Errorf("follower relay lacks a distinct committed Node identity"))
	}
	if !view.Complete && view.Round.Phase == handover.Switch {
		if check == nil {
			return fail(fmt.Errorf("follower switch requires live relay incarnation verification"))
		}
		if err := visitReceiveRelaySwitch(owner, path.PeerPublicKey, root, view, now, check); err != nil {
			return fail(err)
		}
	}
	return path, nil
}
