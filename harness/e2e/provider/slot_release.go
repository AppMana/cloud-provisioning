package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s SlotStore) readPool(ctx context.Context) (*corev1.ConfigMap, *SlotPool, error) {
	if s.API == nil || s.Namespace == "" || s.Name == "" || s.Lab == "" || len(s.Slots) == 0 {
		return nil, nil, fmt.Errorf("slot pool scope required")
	}
	raw, err := s.API.Run(ctx, "-n", s.Namespace, "get", "configmap", s.Name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil, nil
	}
	cm := &corev1.ConfigMap{}
	if err := json.Unmarshal(raw, cm); err != nil {
		return nil, nil, err
	}
	slots := append([]string(nil), s.Slots...)
	slices.Sort(slots)
	pool := &SlotPool{}
	if err := json.Unmarshal([]byte(cm.Data["pool.json"]), pool); err != nil {
		return nil, nil, err
	}
	if cm.UID == "" || cm.ResourceVersion == "" || cm.Name != s.Name || cm.Namespace != s.Namespace || !cm.DeletionTimestamp.IsZero() || pool.Lab != s.Lab || !reflect.DeepEqual(pool.Slots, slots) || pool.Owners == nil {
		return nil, nil, fmt.Errorf("pool identity or inventory changed")
	}
	seen := map[string]bool{}
	for slot, owner := range pool.Owners {
		if !slices.Contains(slots, slot) || owner.Namespace == "" || owner.Name == "" || owner.UID == "" || seen[owner.UID] {
			return nil, nil, fmt.Errorf("invalid pool owner")
		}
		seen[owner.UID] = true
	}
	for slot, uid := range pool.Stopped {
		if uid == "" || pool.Owners[slot].UID != uid {
			return nil, nil, fmt.Errorf("stop receipt does not match slot owner")
		}
	}
	return cm, pool, nil
}
func (s SlotStore) updatePool(ctx context.Context, cm *corev1.ConfigMap, pool *SlotPool) error {
	data, err := json.Marshal(pool)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(cm.UID)}, {"op": "test", "path": "/metadata/resourceVersion", "value": cm.ResourceVersion}, {"op": "replace", "path": "/data/pool.json", "value": string(data)}})
	_, err = s.API.Run(ctx, "-n", s.Namespace, "patch", "configmap", s.Name, "--type=json", "-p", string(patch), "-o", "json")
	return err
}

// Stop records completion before the provider removes its infrastructure
// finalizer. Native VM operations remain serialized by the harness lab lock.
func (s SlotStore) Stop(ctx context.Context, slot string, owner SlotOwner, stop func() error) error {
	cm, pool, err := s.readPool(ctx)
	if err != nil {
		return err
	}
	if pool == nil || owner.UID == "" || pool.Owners[slot] != owner || stop == nil {
		return fmt.Errorf("exact slot owner and native stop operation required")
	}
	if pool.Stopped[slot] == owner.UID {
		return nil
	}
	raw, err := s.API.Run(ctx, "-n", owner.Namespace, "get", machineKind, owner.Name, "-o", "json")
	if err != nil {
		return err
	}
	var machine struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &machine); err != nil {
		return err
	}
	if string(machine.Metadata.UID) != owner.UID || machine.Metadata.DeletionTimestamp.IsZero() {
		return fmt.Errorf("slot owner is not the terminating infrastructure Machine")
	}
	if err := stop(); err != nil {
		return err
	}
	if pool.Stopped == nil {
		pool.Stopped = map[string]string{}
	}
	pool.Stopped[slot] = owner.UID
	return s.updatePool(ctx, cm, pool)
}

// ReleaseStopped releases only stopped slots whose previous infrastructure
// Machine UID is absent. A terminating object still retains its reservation.
func (s SlotStore) ReleaseStopped(ctx context.Context) (int, error) {
	cm, pool, err := s.readPool(ctx)
	if err != nil || pool == nil {
		return 0, err
	}
	released := 0
	for slot := range pool.Stopped {
		owner := pool.Owners[slot]
		raw, err := s.API.Run(ctx, "-n", owner.Namespace, "get", machineKind, owner.Name, "--ignore-not-found", "-o", "json")
		if err != nil {
			return 0, err
		}
		if strings.TrimSpace(string(raw)) != "" {
			var machine struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &machine); err != nil {
				return 0, err
			}
			if machine.Metadata.UID == "" {
				return 0, fmt.Errorf("infrastructure Machine identity missing")
			}
			if string(machine.Metadata.UID) == owner.UID {
				continue
			}
		}
		delete(pool.Owners, slot)
		delete(pool.Stopped, slot)
		released++
	}
	if released == 0 {
		return 0, nil
	}
	if err := s.updatePool(ctx, cm, pool); err != nil {
		return 0, err
	}
	return released, nil
}
