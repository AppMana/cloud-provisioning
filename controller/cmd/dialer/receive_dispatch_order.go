package main

import (
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// Discovery views determine scheduling only. Each target is still reread and
// admitted before execution; this order grants no authority or extra lifetime.
// Keep withdrawal cohorts together, then service expiring handovers before
// completed-worker maintenance. Opaque machine IDs must not determine which
// active round loses its opportunity to report a fresh observation.
func orderReceiveDispatchTargets(targets []string, retiring map[string]bool, rounds map[string]handover.View) {
	slices.SortStableFunc(targets, func(a, b string) int {
		if retiring[a] != retiring[b] {
			if retiring[a] {
				return -1
			}
			return 1
		}
		if retiring[a] {
			return 0
		}
		x, y := rounds[a], rounds[b]
		if x.Complete != y.Complete {
			if x.Complete {
				return 1
			}
			return -1
		}
		if x.Complete {
			return 0
		}
		return x.Round.ExpiresAt.Compare(y.Round.ExpiresAt)
	})
}
