package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// reserveEligibleBindings excludes capacity-pending Machines from this pass while
// retaining progress for every successfully reserved and persisted binding.
// Explicit bindings reserve first. A failed binding patch retains its UID lease.
func (c *Controller) reserveEligibleBindings(ctx context.Context, namespace string, names []string) ([]string, error) {
	if c.Slots == nil {
		return names, nil
	}
	if c.Slots.Lab != c.LabName {
		return nil, fmt.Errorf("slot pool belongs to another lab")
	}
	for _, slot := range c.Slots.Slots {
		if _, err := c.node(slot); err != nil {
			return nil, err
		}
	}
	type candidate struct {
		name, uid, version, requested string
		annotations                   map[string]string
		finalizers                    []string
		deleting                      bool
	}
	var fixed, dynamic []candidate
	occupied := map[string]string{}
	ordered := append([]string(nil), names...)
	slices.Sort(ordered)
	for _, name := range ordered {
		raw, err := c.Kube.Run(ctx, "-n", namespace, "get", machineKind, name, "-o", "json")
		if err != nil {
			return nil, err
		}
		var obj struct {
			Metadata struct {
				UID               string            `json:"uid"`
				Version           string            `json:"resourceVersion"`
				Annotations       map[string]string `json:"annotations"`
				Finalizers        []string          `json:"finalizers"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				ContainerName string `json:"containerName"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if obj.Metadata.UID == "" || obj.Metadata.Version == "" {
			return nil, fmt.Errorf("persisted infrastructure Machine identity required")
		}
		item := candidate{name: name, uid: obj.Metadata.UID, version: obj.Metadata.Version, annotations: obj.Metadata.Annotations, finalizers: obj.Metadata.Finalizers, deleting: obj.Metadata.DeletionTimestamp != nil}
		binding := machineBinding(name, obj.Metadata.Annotations, obj.Spec.ContainerName)
		node, err := c.node(binding)
		if err == nil {
			item.requested = node.Name
			if prior := occupied[node.Name]; prior != "" {
				return nil, fmt.Errorf("VM slot %s is bound to both %s and %s", node.Name, prior, name)
			}
			occupied[node.Name] = name
			fixed = append(fixed, item)
		} else {
			if obj.Metadata.Annotations[containerNameAnnotation] != "" || obj.Spec.ContainerName != "" {
				return nil, err
			}
			dynamic = append(dynamic, item)
		}
	}
	var eligible []string
	var pending []error
	for _, item := range append(fixed, dynamic...) {
		if item.deleting {
			unallocated, err := c.Slots.CancelUnallocated(ctx, SlotOwner{Namespace: namespace, Name: item.name, UID: item.uid})
			if err != nil {
				return nil, err
			}
			if unallocated {
				if item.annotations[instanceAnnotation] != "" {
					return nil, fmt.Errorf("bootstrapped deleting Machine lacks its reservation")
				}
				if slices.Contains(item.finalizers, instanceFinalizer) {
					kept := slices.DeleteFunc(append([]string{}, item.finalizers...), func(value string) bool { return value == instanceFinalizer })
					patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": item.uid}, {"op": "test", "path": "/metadata/resourceVersion", "value": item.version}, {"op": "add", "path": "/metadata/finalizers", "value": kept}})
					if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, item.name, "--type=json", "-p", string(patch)); err != nil {
						return nil, err
					}
				}
				continue
			}
			if !slices.Contains(item.finalizers, instanceFinalizer) {
				return nil, fmt.Errorf("reserved deleting Machine has lost its provider finalizer")
			}
		} else if !slices.Contains(item.finalizers, instanceFinalizer) {
			finalizers := append(append([]string{}, item.finalizers...), instanceFinalizer)
			patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": item.uid}, {"op": "test", "path": "/metadata/resourceVersion", "value": item.version}, {"op": "add", "path": "/metadata/finalizers", "value": finalizers}})
			raw, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, item.name, "--type=json", "-p", string(patch), "-o", "json")
			if err != nil {
				return nil, err
			}
			var updated struct {
				Metadata struct {
					Version string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &updated); err != nil {
				return nil, err
			}
			if updated.Metadata.Version == "" {
				return nil, fmt.Errorf("finalizer patch returned no resourceVersion")
			}
			item.version = updated.Metadata.Version
		}
		slot, err := c.Slots.Reserve(ctx, SlotOwner{Namespace: namespace, Name: item.name, UID: item.uid}, item.requested)
		if err != nil {
			if errors.Is(err, ErrSlotCapacity) {
				pending = append(pending, fmt.Errorf("%s: %w", item.name, err))
				continue
			}
			return nil, err
		}
		eligible = append(eligible, item.name)
		if item.annotations[containerNameAnnotation] == slot {
			continue
		}
		annotations := item.annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[containerNameAnnotation] = slot
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": item.uid}, {"op": "test", "path": "/metadata/resourceVersion", "value": item.version}, {"op": "add", "path": "/metadata/annotations", "value": annotations}})
		if _, err := c.Kube.Run(ctx, "-n", namespace, "patch", machineKind, item.name, "--type=json", "-p", string(patch)); err != nil {
			return nil, err
		}
	}
	return eligible, errors.Join(pending...)
}
