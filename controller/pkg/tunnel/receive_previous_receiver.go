package tunnel

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceivePreviousReceiver carries one serving receiver graph, not a chain of
// historical plans. Its generation must be authenticated against the pinned
// predecessor archive before live admission. It is never an original-tunnel
// fallback. PreviousReceiver inside Plan is forbidden so journal size remains
// bounded across successive transitions.
type ReceivePreviousReceiver struct {
	Generation string                 `json:"generation"`
	Plan       *ReceiveTransitionPlan `json:"plan"`
}

func validateReceivePreviousReceiver(p ReceiveTransitionPlan) error {
	if p.PreviousReceiver == nil {
		if p.Version == 2 {
			return fmt.Errorf("receiver successor plan has no previous graph")
		}
		return nil
	}
	if p.Version != 2 || p.PreviousReceiver.Plan == nil {
		return fmt.Errorf("previous receiver graph requires version two")
	}
	old := p.PreviousReceiver.Plan
	if old.PreviousReceiver != nil || old.Version != 1 {
		return fmt.Errorf("previous receiver graph must be a single flattened transport")
	}
	hash := strings.TrimPrefix(p.PreviousReceiver.Generation, "receivers:")
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != hash || !strings.HasPrefix(p.PreviousReceiver.Generation, "receivers:") {
		return fmt.Errorf("invalid previous receiver generation")
	}
	if p.MeshUID != old.MeshUID || p.MeshName != old.MeshName || p.Namespace != old.Namespace || p.MachineUID != old.MachineUID || p.MachineName != old.MachineName || p.AdoptionUID != old.AdoptionUID || p.AdoptionName != old.AdoptionName || p.CloudNodeUID != old.CloudNodeUID || p.CloudNodeName != old.CloudNodeName || p.RootPublicKey != old.RootPublicKey || !reflect.DeepEqual(p.CloudAddresses, old.CloudAddresses) {
		return fmt.Errorf("previous receiver graph belongs to different identities")
	}
	if p.ReceiverGeneration == "" || p.ReceiverGeneration == old.ReceiverGeneration || !bytes.Equal(p.Base, old.Base) || p.SiteBase == nil || !reflect.DeepEqual(*p.SiteBase, old.SiteDesired) {
		return fmt.Errorf("successor does not preserve its previous receiver inputs")
	}
	if _, err := PlanReceiveRoutes(*old); err != nil {
		return fmt.Errorf("invalid previous receiver graph: %w", err)
	}
	if len(p.Sites) != len(old.Sites) {
		return fmt.Errorf("successor changes the previous receiver participant set")
	}
	for public, site := range p.Sites {
		previous, found := old.Sites[public]
		if !found || previous.NodeUID != site.NodeUID || previous.NodeName != site.NodeName {
			return fmt.Errorf("successor changes a previous site identity")
		}
	}
	for _, binding := range p.Bindings {
		for _, previous := range old.Bindings {
			if binding.Interface == previous.Interface || binding.PublicKey == previous.PublicKey || binding.ListenPort == previous.ListenPort {
				return fmt.Errorf("successor reuses a previous receiver identity or listener")
			}
		}
	}
	return nil
}

// ValidateReceiveTransportHistory binds the projected serving graph to actual
// archived configuration. Matching member keys alone cannot authenticate source
// permissions, routes or relay choices. The caller also verifies full ancestry.
func ValidateReceiveTransportHistory(ctx context.Context, store handover.Store, current handover.Snapshot) error {
	if _, found := current.Predecessor(); !found {
		return nil
	}
	plan, err := DecodeReceiveTransition(current, store.ClusterUID)
	if err != nil {
		return err
	}
	archive, err := store.PredecessorArchive(ctx, current)
	if err != nil {
		return err
	}
	previous, err := decodeReceiveTransitionContents(archive, store.ClusterUID)
	if err != nil {
		return err
	}
	view, err := archive.Transition().View()
	if err != nil {
		return err
	}
	if view.ServingGeneration == "legacy:"+HashPeerList(previous.Base) {
		if plan.PreviousReceiver != nil || !bytes.Equal(plan.Base, previous.Base) || !reflect.DeepEqual(plan.PreviousSitePeers(), previous.PreviousSitePeers()) {
			return fmt.Errorf("successor differs from archived original transport")
		}
		return nil
	}
	var serving ReceiveTransitionPlan
	switch {
	case view.ServingGeneration == "receivers:"+view.Round.PlanHash:
		serving = previous
		serving.Version, serving.PreviousReceiver = 1, nil
	case previous.PreviousReceiver != nil && view.ServingGeneration == previous.PreviousReceiver.Generation:
		serving = *previous.PreviousReceiver.Plan
	default:
		return fmt.Errorf("archive selects an unknown receiver transport")
	}
	if plan.PreviousReceiver == nil || plan.PreviousReceiver.Generation != view.ServingGeneration || !reflect.DeepEqual(*plan.PreviousReceiver.Plan, serving) {
		return fmt.Errorf("successor receiver graph differs from archived serving transport")
	}
	return nil
}
