package tunnel

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceiveJournalRoutes describes selected intent, not native completion. Legacy
// entries must be compiled alongside Receivers; an empty receiver map does not
// authorize deleting the legacy routes. Keys in Legacy are immutable Machine UIDs.
type ReceiveJournalRoutes struct {
	Receivers map[string]map[string]ReceiveRouteSelection
	Legacy    map[string]ReceiveTransitionPlan
}

// PlanReceiveJournalRoutes validates the complete caller-admitted journal set
// before selecting any egress. Prepare and abort preserve legacy selection;
// switch/drain/retire use the committed destination generation in either
// direction. Completed journals retain their verified serving generation.
//
// Expiry does not undo persisted intent. Execution still needs fresh admission,
// an executable round, the host lock, native verification and durable recovery.
// The caller must detect missing journals and preserve unrelated legacy peers.
func PlanReceiveJournalRoutes(snapshots []handover.Snapshot, clusterUID string) (ReceiveJournalRoutes, error) {
	fail := func(err error) (ReceiveJournalRoutes, error) { return ReceiveJournalRoutes{}, err }
	if len(snapshots) == 0 {
		return fail(fmt.Errorf("journal route planning requires an explicit complete set"))
	}
	plans := make([]ReceiveTransitionPlan, 0, len(snapshots))
	var selected []ReceiveTransitionPlan
	legacy := map[string]ReceiveTransitionPlan{}
	for _, snapshot := range snapshots {
		plan, err := DecodeReceiveTransition(snapshot, clusterUID)
		if err != nil {
			return fail(err)
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return fail(err)
		}
		generation := view.ServingGeneration
		if !view.Complete {
			switch view.Round.Phase {
			case handover.Prepare, handover.Abort:
				generation = view.Round.FromGeneration
			case handover.Switch, handover.Drain, handover.Retire:
				generation = view.Round.ToGeneration
			default:
				return fail(fmt.Errorf("unknown receiver routing phase"))
			}
		}
		switch generation {
		case "legacy:" + HashPeerList(plan.Base):
			legacy[plan.MachineUID] = plan
		case "receivers:" + view.Round.PlanHash:
			selected = append(selected, plan)
		default:
			return fail(fmt.Errorf("journal selects an unknown transport generation"))
		}
		plans = append(plans, plan)
	}
	// Check all journals, including those keeping legacy egress. Filtering first
	// would hide an overlapping worker allocation or a replaced node identity.
	all, err := PlanReceiveRouteSet(plans)
	if err != nil {
		return fail(err)
	}
	result := ReceiveJournalRoutes{Receivers: map[string]map[string]ReceiveRouteSelection{}, Legacy: legacy}
	for node := range all {
		result.Receivers[node] = map[string]ReceiveRouteSelection{}
	}
	if len(selected) != 0 {
		active, err := PlanReceiveRouteSet(selected)
		if err != nil {
			return fail(err)
		}
		for node, routes := range active {
			result.Receivers[node] = routes
		}
	}
	return result, nil
}
