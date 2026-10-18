package tunnel

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// ReceiveJournalRoutes describes selected intent, not native completion. Legacy
// entries must be compiled alongside Receivers; an empty receiver map does not
// authorize deleting the legacy routes. Keys in Legacy are immutable Machine UIDs.
type ReceiveJournalRoutes struct {
	Views     map[string]handover.View
	Receivers map[string]map[string]ReceiveRouteSelection
	Legacy    map[string]ReceiveTransitionPlan
	Plans     map[string]ReceiveTransitionPlan
	Sources   map[string]ReceiveSiteTunnel
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
	views := map[string]handover.View{}
	for _, snapshot := range snapshots {
		plan, err := DecodeReceiveTransition(snapshot, clusterUID)
		if err != nil {
			return fail(err)
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return fail(err)
		}
		views[plan.MachineUID] = view
		active, root, err := plan.SelectedReceiveTransport(view)
		if err != nil {
			return fail(err)
		}
		if root {
			legacy[plan.MachineUID] = plan
		} else {
			selected = append(selected, active)
		}
		plans = append(plans, plan)
	}
	// Check all journals, including those keeping legacy egress. Filtering first
	// would hide an overlapping worker allocation or a replaced node identity.
	all, err := PlanReceiveRouteSet(plans)
	if err != nil {
		return fail(err)
	}
	result := ReceiveJournalRoutes{Views: views, Receivers: map[string]map[string]ReceiveRouteSelection{}, Legacy: legacy, Plans: map[string]ReceiveTransitionPlan{}}
	for _, plan := range plans {
		result.Plans[plan.MachineUID] = plan
	}
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
	// Old journals remain readable, but a partially upgraded source projection
	// cannot authorize a shared-table update. Once any plan carries allocations,
	// the complete set must have a consistent explicit projection.
	hasSources := false
	for _, plan := range plans {
		for _, site := range plan.Sites {
			hasSources = hasSources || (site.Legacy != nil && site.Legacy.Tunnel != nil)
		}
	}
	if hasSources {
		result.Sources, err = PlanReceiveSourceRoutes(result)
		if err != nil {
			return fail(err)
		}
	}
	return result, nil
}

// SelectedReceiveTransport selects forwarding intent from an already admitted
// journal view. It does not validate ancestry, authorize native mutation or prove
// forwarding completion. The boolean identifies the original root transport;
// a predecessor receiver remains a receiver even during abort or rollback.
func (p ReceiveTransitionPlan) SelectedReceiveTransport(view handover.View) (ReceiveTransitionPlan, bool, error) {
	generation := view.ServingGeneration
	if !view.Complete {
		switch view.Round.Phase {
		case handover.Prepare, handover.Abort:
			generation = view.Round.FromGeneration
		case handover.Switch, handover.Drain, handover.Retire:
			generation = view.Round.ToGeneration
		default:
			return ReceiveTransitionPlan{}, false, fmt.Errorf("unknown receiver routing phase")
		}
	}
	if p.PreviousReceiver != nil {
		if p.PreviousReceiver.Plan == nil {
			return ReceiveTransitionPlan{}, false, fmt.Errorf("missing predecessor receiver graph")
		}
		if generation == p.PreviousReceiver.Generation {
			return *p.PreviousReceiver.Plan, false, nil
		}
	} else if generation == "legacy:"+HashPeerList(p.Base) {
		return p, true, nil
	}
	if view.Round.PlanHash != "" && generation == "receivers:"+view.Round.PlanHash {
		return p, false, nil
	}
	return ReceiveTransitionPlan{}, false, fmt.Errorf("journal selects an unknown transport generation")
}
