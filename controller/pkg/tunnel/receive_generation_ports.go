package tunnel

import (
	"fmt"
	"maps"
)

// Validate checks every retained generation before any new assignment. Version
// two prevents old controllers from ignoring reservations they cannot interpret.
func (r ReceivePortReservations) Validate() error {
	if r.Ports == nil || r.NodeUID == "" || !validReceivePublicKey(r.RootPublicKey) ||
		(r.Version != 1 && r.Version != 2) || (r.Version == 1 && len(r.Generations) != 0) || (r.Version == 2 && len(r.Generations) == 0) {
		return fmt.Errorf("invalid receiver port reservation identity or version")
	}
	if _, err := (ReceiveTransitionPlan{MeshUID: r.MeshUID}).ReceiveScope(); err != nil {
		return err
	}
	used := map[int]bool{}
	check := func(ports map[string]int) error {
		if ports == nil {
			return fmt.Errorf("missing generation port reservations")
		}
		for peer, port := range ports {
			if !validReceivePublicKey(peer) || port < 1 || port > 65535 || used[port] {
				return fmt.Errorf("conflicting or invalid retained receiver port")
			}
			used[port] = true
		}
		return nil
	}
	if err := check(r.Ports); err != nil {
		return err
	}
	for generation, ports := range r.Generations {
		if generation == "" {
			return fmt.Errorf("empty receiver generation reservation")
		}
		if _, err := (ReceiveTransitionPlan{MeshUID: r.MeshUID, ReceiverGeneration: generation}).ReceiveScope(); err != nil {
			return err
		}
		if err := check(ports); err != nil {
			return err
		}
	}
	return nil
}

// Assign retains all other generations as occupied listeners and returns an
// independent candidate. Persist it before publishing preparation. This is not
// retirement authority and never releases a departed peer's reservation.
func (r ReceivePortReservations) Assign(generation string, paths []ReceivePath, pool []int) (map[string]int, ReceivePortReservations, error) {
	fail := func(err error) (map[string]int, ReceivePortReservations, error) {
		return nil, ReceivePortReservations{}, err
	}
	if err := r.Validate(); err != nil {
		return fail(err)
	}
	if _, err := (ReceiveTransitionPlan{MeshUID: r.MeshUID, ReceiverGeneration: generation}).ReceiveScope(); err != nil {
		return fail(err)
	}
	active := r.Ports
	var occupied []int
	if generation != "" {
		active = r.Generations[generation]
		if active == nil {
			active = map[string]int{}
		}
		for _, port := range r.Ports {
			occupied = append(occupied, port)
		}
	}
	for other, ports := range r.Generations {
		if other == generation {
			continue
		}
		for _, port := range ports {
			occupied = append(occupied, port)
		}
	}
	assigned, retained, err := AssignReceivePorts(paths, active, pool, occupied...)
	if err != nil {
		return fail(err)
	}
	updated := r
	updated.Ports = maps.Clone(r.Ports)
	updated.Generations = make(map[string]map[string]int, len(r.Generations)+1)
	for key, ports := range r.Generations {
		updated.Generations[key] = maps.Clone(ports)
	}
	if generation == "" {
		updated.Ports = retained
	} else {
		updated.Version = 2
		updated.Generations[generation] = retained
	}
	if err := updated.Validate(); err != nil {
		return fail(err)
	}
	return assigned, updated, nil
}
