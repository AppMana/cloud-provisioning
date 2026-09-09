package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// reserveBindings reserves explicit topology bindings before generated names
// can take free slots. Allocation and binding are separate durable writes: a
// failed Machine patch leaves its UID's reservation available to the retry.
func (c *Controller) reserveBindings(ctx context.Context, namespace string, names []string) error {
	if c.Slots == nil {
		return nil
	}
	if c.Slots.Lab != c.LabName {
		return fmt.Errorf("slot pool belongs to another lab")
	}
	for _, slot := range c.Slots.Slots {
		if _, err := c.node(slot); err != nil {
			return err
		}
	}
	type candidate struct {
		name, uid, version, requested string
		annotations                   map[string]string
	}
	var fixed, dynamic []candidate
	occupied := map[string]string{}
	ordered := append([]string(nil), names...)
	slices.Sort(ordered)
	for _, name := range ordered {
		raw, err := c.Kube.Run(ctx, "-n", namespace, "get", machineKind, name, "-o", "json")
		if err != nil {
			return err
		}
		var obj struct {
			Metadata struct {
				UID         string            `json:"uid"`
				Version     string            `json:"resourceVersion"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				ContainerName string `json:"containerName"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		if obj.Metadata.UID == "" || obj.Metadata.Version == "" {
			return fmt.Errorf("persisted infrastructure Machine identity required")
		}
		item := candidate{name: name, uid: obj.Metadata.UID, version: obj.Metadata.Version, annotations: obj.Metadata.Annotations}
		binding := machineBinding(name, obj.Metadata.Annotations, obj.Spec.ContainerName)
		node, err := c.node(binding)
		if err == nil {
			item.requested = node.Name
			if prior := occupied[node.Name]; prior != "" {
				return fmt.Errorf("VM slot %s is bound to both %s and %s", node.Name, prior, name)
			}
			occupied[node.Name] = name
			fixed = append(fixed, item)
		} else {
			if obj.Metadata.Annotations[containerNameAnnotation] != "" || obj.Spec.ContainerName != "" {
				return err
			}
			dynamic = append(dynamic, item)
		}
	}
	for _, item := range append(fixed, dynamic...) {
		slot, err := c.Slots.Reserve(ctx, SlotOwner{Namespace: namespace, Name: item.name, UID: item.uid}, item.requested)
		if err != nil {
			return err
		}
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
			return err
		}
	}
	return nil
}
