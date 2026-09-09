// Package nodegroup plans replica changes without performing cloud operations.
// The future reconciler must verify ownership, persist intent, and complete
// cordon/eviction/attachment withdrawal before deleting a selected claim.
package nodegroup

import "fmt"

type Child struct {
	Name        string
	OwnerUID    string
	Ordinal     int
	Terminating bool
	Draining    bool
}
type Plan struct {
	// CreateOrdinal is the next free stable slot, or nil when no create is needed.
	CreateOrdinal *int
	// Drain identifies an excess claim to cordon and drain, never direct deletion.
	Drain string
	// Waiting means an existing removal must finish before another mutation.
	Waiting bool
}

// Next serializes capacity mutations and counts provisioning children as live.
// The caller supplies all children in the group's naming/label scope; any foreign
// owner or duplicate identity fails closed rather than being silently adopted.
func Next(ownerUID string, replicas int, children []Child) (Plan, error) {
	if ownerUID == "" || replicas < 0 {
		return Plan{}, fmt.Errorf("owner UID and nonnegative replicas required")
	}
	slots := map[int]bool{}
	names := map[string]bool{}
	waiting := false
	for _, c := range children {
		if c.Name == "" || c.OwnerUID != ownerUID || c.Ordinal < 0 || slots[c.Ordinal] || names[c.Name] {
			return Plan{}, fmt.Errorf("invalid or foreign child identity")
		}
		slots[c.Ordinal] = true
		names[c.Name] = true
		waiting = waiting || c.Terminating || c.Draining
	}
	if waiting {
		return Plan{Waiting: true}, nil
	}
	if len(children) > replicas {
		var selected Child
		for _, c := range children {
			if selected.Name == "" || c.Ordinal > selected.Ordinal {
				selected = c
			}
		}
		return Plan{Drain: selected.Name}, nil
	}
	if len(children) < replicas {
		ordinal := 0
		for slots[ordinal] {
			ordinal++
		}
		return Plan{CreateOrdinal: &ordinal}, nil
	}
	return Plan{}, nil
}
