package tunnel

import (
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// PlanReceiveJournalFollowerPath uses the same generation selection as receiver
// routes. The snapshot must already be admitted against inventory, ancestry and
// withdrawal authority. This is route intent only: a switch still needs the
// selected relay's current forwarding observation before a follower may use it.
func PlanReceiveJournalFollowerPath(data map[string][]byte, node string, snapshot handover.Snapshot, clusterUID string) (ReceiveSitePath, error) {
	plan, err := DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return ReceiveSitePath{}, err
	}
	view, err := snapshot.Transition().View()
	if err != nil {
		return ReceiveSitePath{}, err
	}
	selected, root, err := plan.SelectedReceiveTransport(view)
	if err != nil {
		return ReceiveSitePath{}, err
	}
	sources, destinations := selected.Desired, selected.SiteDesired
	if root {
		if err := json.Unmarshal(selected.Base, &sources); err != nil {
			return ReceiveSitePath{}, err
		}
		destinations = selected.PreviousSitePeers()
	}
	var cloud *PeerSpec
	for _, peer := range destinations.Peers {
		if peer.PublicKey == plan.RootPublicKey {
			if cloud != nil {
				return ReceiveSitePath{}, fmt.Errorf("follower destination identity is ambiguous")
			}
			copied := peer
			cloud = &copied
		}
	}
	if cloud == nil {
		return ReceiveSitePath{}, fmt.Errorf("follower destination identity missing")
	}
	return PlanReceiveFollowerPath(data, node, sources, *cloud)
}
