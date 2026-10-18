package tunnel

import (
	"fmt"
	"maps"
	"slices"
)

// AssignReceivePorts reserves stable listener ports from an explicitly permitted
// pool. Existing reservations, including departed peers, remain occupied until
// a separate retirement operation proves they can be released. Occupied ports
// belong to other generations or listeners and cannot be assigned or claimed by
// this reservation set, even when the remote peer identity is unchanged.
func AssignReceivePorts(paths []ReceivePath, reserved map[string]int, pool []int, occupied ...int) (map[string]int, map[string]int, error) {
	if reserved == nil || pool == nil {
		return nil, nil, fmt.Errorf("explicit receive port reservations and pool required")
	}
	allowed := map[int]bool{}
	for _, port := range pool {
		if port < 1 || port > 65535 || allowed[port] {
			return nil, nil, fmt.Errorf("invalid or duplicate receive port")
		}
		allowed[port] = true
	}
	used := map[int]bool{}
	for _, port := range occupied {
		if port < 1 || port > 65535 || used[port] {
			return nil, nil, fmt.Errorf("invalid or duplicate occupied receive port")
		}
		used[port] = true
	}
	for key, port := range reserved {
		if !validReceivePublicKey(key) || !allowed[port] || used[port] {
			return nil, nil, fmt.Errorf("existing receive port reservation is invalid or outside the permitted pool")
		}
		used[port] = true
	}
	keys := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if !validReceivePublicKey(path.PublicKey) || seen[path.PublicKey] {
			return nil, nil, fmt.Errorf("invalid receive port peer")
		}
		keys = append(keys, path.PublicKey)
		seen[path.PublicKey] = true
	}
	slices.Sort(keys)
	available := slices.Clone(pool)
	slices.Sort(available)
	updated := maps.Clone(reserved)
	assigned := make(map[string]int, len(paths))
	for _, key := range keys {
		if port, ok := updated[key]; ok {
			assigned[key] = port
			continue
		}
		port := 0
		for _, candidate := range available {
			if !used[candidate] {
				port = candidate
				break
			}
		}
		if port == 0 {
			return nil, nil, fmt.Errorf("receive port pool exhausted; retained reservations cannot be reused")
		}
		updated[key], assigned[key], used[port] = port, port, true
	}
	return assigned, updated, nil
}

func ValidateReceivePorts(paths []ReceivePath, ports map[string]int) error {
	if ports == nil || len(ports) != len(paths) {
		return fmt.Errorf("exact receive port assignments required")
	}
	seen := map[int]bool{}
	for _, path := range paths {
		port := ports[path.PublicKey]
		if port < 1 || port > 65535 || seen[port] {
			return fmt.Errorf("invalid or duplicate assigned receive port")
		}
		seen[port] = true
	}
	return nil
}
