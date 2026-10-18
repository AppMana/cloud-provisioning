package main

import (
	"context"
	"fmt"
	"net/netip"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	bgpapi "github.com/osrg/gobgp/v3/api"
)

// Caller holds the host lock and has admitted the full reserved set. Withdraw
// only the target's committed destinations, including its predecessor, without
// reannouncing survivors from a cached projection. Read back the actual BGP RIB;
// accepting DeletePath alone is not evidence that a prefix is absent.
func withdrawReceiveAdvertisements(ctx context.Context, speaker *transitSpeaker, plans []tunnel.ReceiveTransitionPlan, target, nodeUID string) error {
	if _, err := tunnel.PlanReceiveRouteSet(plans); err != nil {
		return err
	}
	prefixes := map[string]bool{}
	found := false
	for _, plan := range plans {
		if plan.MachineUID != target {
			continue
		}
		if found {
			return fmt.Errorf("duplicate advertisement withdrawal owner")
		}
		found = true
		graphs := []tunnel.ReceiveTransitionPlan{plan}
		if plan.PreviousReceiver != nil {
			graphs = append(graphs, *plan.PreviousReceiver.Plan)
		}
		for _, graph := range graphs {
			routes, err := tunnel.PlanReceiveRoutes(graph)
			if err != nil {
				return err
			}
			for prefix := range routes[nodeUID] {
				prefixes[prefix] = true
			}
		}
	}
	if !found || nodeUID == "" || len(prefixes) == 0 {
		return fmt.Errorf("advertisement withdrawal requires committed local destinations")
	}
	if speaker == nil {
		return ctx.Err()
	}
	return speaker.withdrawReceivePrefixes(ctx, prefixes)
}

func (s *transitSpeaker) withdrawReceivePrefixes(ctx context.Context, prefixes map[string]bool) error {
	var ordered []string
	for raw := range prefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
			return fmt.Errorf("invalid advertisement withdrawal destination")
		}
		ordered = append(ordered, raw)
	}
	sort.Strings(ordered)
	for _, prefix := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if med, found := s.advertised[prefix]; found {
			if err := s.advertise(ctx, transitRoute{prefix: prefix, med: med}, true); err != nil {
				return err
			}
			delete(s.advertised, prefix)
		}
	}
	for _, afi := range []bgpapi.Family_Afi{bgpapi.Family_AFI_IP, bgpapi.Family_AFI_IP6} {
		present := false
		err := s.server.ListPath(ctx, &bgpapi.ListPathRequest{TableType: bgpapi.TableType_GLOBAL, Family: &bgpapi.Family{Afi: afi, Safi: bgpapi.Family_SAFI_UNICAST}}, func(destination *bgpapi.Destination) {
			if prefixes[destination.Prefix] && len(destination.Paths) > 0 {
				present = true
			}
		})
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("withdrawn destination remains in BGP table")
		}
	}
	return ctx.Err()
}
