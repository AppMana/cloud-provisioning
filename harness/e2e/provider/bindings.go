package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// checkDistinctBindings checks the whole namespace before a reconcile pass can
// publish addresses or touch a VM. Templates with a fixed slot cannot be used
// for multiple replicas. This is a preflight check, not a concurrent allocator;
// the harness still serializes provider passes under its lab process lock.
func (c *Controller) checkDistinctBindings(ctx context.Context, namespace string, names []string) error {
	owners := map[string]string{}
	for _, name := range names {
		raw, err := c.Kube.Run(ctx, "-n", namespace, "get", machineKind, name, "-o", "json")
		if err != nil {
			return err
		}
		var obj struct {
			Metadata struct {
				Annotations       map[string]string `json:"annotations"`
				Finalizers        []string          `json:"finalizers"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				ContainerName string `json:"containerName"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("reading binding for %s: %w", name, err)
		}
		if c.Slots != nil && obj.Metadata.DeletionTimestamp != nil && !slices.Contains(obj.Metadata.Finalizers, instanceFinalizer) {
			continue
		}
		binding := machineBinding(name, obj.Metadata.Annotations, obj.Spec.ContainerName)
		node, err := c.node(binding)
		if err != nil {
			return err
		}
		if owner, exists := owners[node.Name]; exists {
			return fmt.Errorf("VM slot %s is bound to both %s and %s; each infrastructure Machine requires its own slot", node.Name, owner, name)
		}
		owners[node.Name] = name
	}
	return nil
}

func machineBinding(name string, annotations map[string]string, spec string) string {
	if value := annotations[containerNameAnnotation]; value != "" {
		return value
	}
	if spec != "" {
		return spec
	}
	return name
}
