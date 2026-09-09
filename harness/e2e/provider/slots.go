package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type SlotOwner struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}
type SlotPool struct {
	Stopped map[string]string    `json:"stopped,omitempty"`
	Lab     string               `json:"lab"`
	Slots   []string             `json:"slots"`
	Owners  map[string]SlotOwner `json:"owners"`
}
type SlotCommands interface {
	Run(context.Context, ...string) ([]byte, error)
}
type SlotStore struct {
	API                  SlotCommands
	Namespace, Name, Lab string
	Slots                []string
}

// Reserve atomically assigns one fixed VM slot to an infrastructure Machine UID.
// The ConfigMap persists independently of Machine garbage collection. This
// operation never frees reservations; teardown must prove completion separately.
func (s SlotStore) Reserve(ctx context.Context, owner SlotOwner, requested string) (string, error) {
	if s.API == nil || s.Namespace == "" || s.Name == "" || s.Lab == "" || owner.Namespace == "" || owner.Name == "" || owner.UID == "" || len(s.Slots) == 0 {
		return "", fmt.Errorf("slot store scope, pool and Machine identity required")
	}
	slots := append([]string(nil), s.Slots...)
	slices.Sort(slots)
	for i, slot := range slots {
		if slot == "" || strings.TrimSpace(slot) != slot || i > 0 && slots[i-1] == slot {
			return "", fmt.Errorf("invalid slot inventory")
		}
	}
	if requested != "" && !slices.Contains(slots, requested) {
		return "", fmt.Errorf("requested slot is outside the pool")
	}
	cm, loaded, err := s.readPool(ctx)
	if err != nil {
		return "", err
	}
	existing := loaded != nil
	pool := SlotPool{Lab: s.Lab, Slots: slots, Owners: map[string]SlotOwner{}}
	if existing {
		pool = *loaded
	}
	seen := map[string]bool{}
	assigned := ""
	for slot, held := range pool.Owners {
		if !slices.Contains(slots, slot) || held.UID == "" || held.Namespace == "" || held.Name == "" || seen[held.UID] {
			return "", fmt.Errorf("invalid persisted slot ownership")
		}
		seen[held.UID] = true
		if held.UID == owner.UID {
			if held != owner {
				return "", fmt.Errorf("Machine UID associated with another name")
			}
			assigned = slot
		} else if held.Namespace == owner.Namespace && held.Name == owner.Name {
			return "", fmt.Errorf("previous Machine incarnation still owns a slot")
		}
	}
	if assigned != "" {
		if requested != "" && requested != assigned {
			return "", fmt.Errorf("Machine already owns another slot")
		}
		return assigned, nil
	}
	for _, slot := range slots {
		if requested != "" && slot != requested {
			continue
		}
		if _, occupied := pool.Owners[slot]; !occupied {
			assigned = slot
			break
		}
	}
	if assigned == "" {
		return "", fmt.Errorf("VM pool has no available requested capacity")
	}
	pool.Owners[assigned] = owner
	data, err := json.Marshal(pool)
	if err != nil {
		return "", err
	}
	if !existing {
		_, err = s.API.Run(ctx, "-n", s.Namespace, "create", "configmap", s.Name, "--from-literal=pool.json="+string(data), "-o", "json")
	} else {
		err = s.updatePool(ctx, cm, &pool)
	}
	if err != nil {
		return "", err
	}
	return assigned, nil
}
