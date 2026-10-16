package tunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceiveTransitionName is the shared controller/agent journal lookup key.
func ReceiveTransitionName(machineUID string) string {
	sum := sha256.Sum256([]byte(machineUID))
	return fmt.Sprintf("receive-transition-%x", sum[:16])
}

// ReceiveTransitionSpec binds the complete public transport graph to a round.
func ReceiveTransitionSpec(plan ReceiveTransitionPlan, clusterUID, hash string) (handover.Spec, error) {
	if _, err := PlanReceiveRoutes(plan); err != nil {
		return handover.Spec{}, fmt.Errorf("receiver journal has an invalid route plan: %w", err)
	}
	members := make([]handover.Member, 0, 2*len(plan.Sites))
	for pub, site := range plan.Sites {
		binding := plan.Bindings[pub]
		members = append(members,
			handover.Member{NodeUID: plan.CloudNodeUID, DeviceID: "site:" + pub, FromKey: plan.RootPublicKey, ToKey: binding.PublicKey, FromPeerKey: pub, ToPeerKey: pub},
			handover.Member{NodeUID: site.NodeUID, DeviceID: "cloud:" + plan.RootPublicKey, FromKey: pub, ToKey: pub, FromPeerKey: plan.RootPublicKey, ToPeerKey: binding.PublicKey})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].NodeUID != members[j].NodeUID {
			return members[i].NodeUID < members[j].NodeUID
		}
		return members[i].DeviceID < members[j].DeviceID
	})
	return handover.Spec{ClusterUID: clusterUID, TransitionUID: plan.MachineUID + ":" + hash, PlanHash: hash, FromGeneration: "legacy:" + HashPeerList(plan.Base), ToGeneration: "receivers:" + hash, Phase: handover.Prepare, Members: members}, nil
}

// A valid journal and matching plan hash do not by themselves bind its
// participants to the plan. Reconstruct that binding without minting a round
// or changing persisted state, including the reversed direction of rollback.
func validateReceiveTransitionBinding(snapshot handover.Snapshot, plan ReceiveTransitionPlan, clusterUID string) error {
	view, err := snapshot.Transition().View()
	if err != nil {
		return err
	}
	expected, err := ReceiveTransitionSpec(plan, clusterUID, view.Round.PlanHash)
	if err != nil {
		return err
	}
	expected.Phase = view.Round.Phase
	if view.RollbackRequested && view.Round.Phase != handover.Abort {
		expected.FromGeneration, expected.ToGeneration = expected.ToGeneration, expected.FromGeneration
		for i := range expected.Members {
			m := &expected.Members[i]
			m.FromKey, m.ToKey = m.ToKey, m.FromKey
			m.FromPeerKey, m.ToPeerKey = m.ToPeerKey, m.FromPeerKey
		}
	}
	if !reflect.DeepEqual(expected, view.Round.Spec) {
		return fmt.Errorf("receiver journal round does not match its public transport plan")
	}
	return nil
}

// DecodeReceiveTransition validates persisted configuration without minting a
// round or authorizing native changes. Both publisher and agents use this
// decoder; a storage hash alone does not establish executable graph semantics.
func DecodeReceiveTransition(snapshot handover.Snapshot, clusterUID string) (ReceiveTransitionPlan, error) {
	var plan ReceiveTransitionPlan
	fail := func(err error) (ReceiveTransitionPlan, error) { return ReceiveTransitionPlan{}, err }
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Plan()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return fail(fmt.Errorf("decoding receiver journal plan: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("receiver journal contains trailing plan data"))
	}
	if plan.Version != 1 || plan.Namespace == "" || plan.MeshName == "" || plan.MeshUID == "" || plan.MachineName == "" || plan.MachineUID == "" || plan.AdoptionName != AdoptionSecretName(plan.MachineName) || plan.AdoptionUID == "" || plan.CloudNodeName == "" || plan.CloudNodeUID == "" {
		return fail(fmt.Errorf("receiver journal has incomplete public identities"))
	}
	metadata := snapshot.Metadata()
	if metadata.Name != ReceiveTransitionName(plan.MachineUID) || metadata.Namespace != plan.Namespace || metadata.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver journal object does not match its public scope or is being deleted"))
	}
	if err := validateReceiveTransitionBinding(snapshot, plan, clusterUID); err != nil {
		return fail(err)
	}
	return plan, nil
}
