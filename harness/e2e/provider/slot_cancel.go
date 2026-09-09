package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s SlotStore) currentOwner(ctx context.Context, owner SlotOwner, deleting bool) (*metav1.ObjectMeta, error) {
	if owner.Namespace == "" || owner.Name == "" || owner.UID == "" {
		return nil, fmt.Errorf("infrastructure Machine identity required")
	}
	raw, err := s.API.Run(ctx, "-n", owner.Namespace, "get", machineKind, owner.Name, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, ErrSlotOwnerInactive
	}
	var object struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	m := &object.Metadata
	if string(m.UID) != owner.UID || m.Name != owner.Name || m.Namespace != owner.Namespace || m.ResourceVersion == "" {
		return nil, ErrSlotOwnerInactive
	}
	if deleting {
		if m.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("cancellation requires a deleting infrastructure Machine")
		}
	} else if !m.DeletionTimestamp.IsZero() {
		return nil, ErrSlotOwnerInactive
	}
	return m, nil
}

// CancelUnallocated fences new reservations for an exact deleting Machine.
// A false result means a slot is already owned and requires normal VM teardown.
// A true result permits provider finalizer removal: a pre-deletion allocator's
// snapshot conflicts with this pool write, and later allocators reject the
// deleting or absent Machine. No persistent cancellation tombstones are needed.
func (s SlotStore) CancelUnallocated(ctx context.Context, owner SlotOwner) (bool, error) {
	cm, pool, err := s.readPool(ctx)
	if err != nil {
		return false, err
	}
	if _, err := s.currentOwner(ctx, owner, true); err != nil {
		return false, err
	}
	if pool == nil {
		slots := append([]string(nil), s.Slots...)
		slices.Sort(slots)
		pool = &SlotPool{Lab: s.Lab, Slots: slots, Owners: map[string]SlotOwner{}}
	} else {
		for _, held := range pool.Owners {
			if held.UID == owner.UID {
				if held != owner {
					return false, fmt.Errorf("Machine UID associated with another name")
				}
				return false, nil
			}
		}
	}
	if cm == nil {
		data, err := json.Marshal(pool)
		if err != nil {
			return false, err
		}
		_, err = s.API.Run(ctx, "-n", s.Namespace, "create", "configmap", s.Name, "--from-literal=pool.json="+string(data), "-o", "json")
		return err == nil, err
	}
	// A no-op patch need not advance resourceVersion. Record the cancelled UID
	// in an annotation so the first cancellation always fences stale allocators.
	// Repeated cancellation is safe: every allocator starting after the first
	// fence must observe the owner's deletion timestamp.
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(cm.UID)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": cm.ResourceVersion},
		{"op": "add", "path": "/metadata/annotations", "value": cancellationAnnotations(cm.Annotations, owner.UID)},
	})
	_, err = s.API.Run(ctx, "-n", s.Namespace, "patch", "configmap", s.Name, "--type=json", "-p", string(patch), "-o", "json")
	return err == nil, err
}

func cancellationAnnotations(existing map[string]string, uid string) map[string]string {
	annotations := make(map[string]string, len(existing)+1)
	for key, value := range existing {
		annotations[key] = value
	}
	annotations["containernet.appmana.com/cancelled-machine-uid"] = uid
	return annotations
}
