package provider

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func verifyUnallocatedCancellation(t *testing.T, ctx context.Context, c *Controller, ns string) {
	t.Helper()
	name := "never-allocated"
	data, _ := json.Marshal(map[string]any{"apiVersion": "containernet.appmana.com/v1beta2", "kind": "ContainernetMachine", "metadata": map[string]any{"name": name, "namespace": ns, "finalizers": []string{instanceFinalizer, "test/hold"}}, "spec": map[string]any{}})
	if err := c.Kube.Apply(ctx, data); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Kube.Run(ctx, "-n", ns, "delete", machineKind, name, "--wait=false"); err != nil {
		t.Fatal(err)
	}
	_, before, err := c.Slots.readPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.reserveEligibleBindings(ctx, ns, []string{name}); err != nil {
		t.Fatal(err)
	}
	raw, err := c.Kube.Run(ctx, "-n", ns, "get", machineKind, name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Metadata struct {
			Finalizers  []string          `json:"finalizers"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(obj.Metadata.Finalizers, instanceFinalizer) || !slices.Contains(obj.Metadata.Finalizers, "test/hold") || obj.Metadata.Annotations[containerNameAnnotation] != "" {
		t.Fatal("unallocated cancellation changed other ownership")
	}
	_, after, err := c.Slots.readPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("unallocated cancellation changed reservations")
	}
	if err := c.checkDistinctBindings(ctx, ns, []string{name}); err != nil {
		t.Fatal(err)
	}
	observed, err := c.reconcileOne(ctx, ns, name)
	if err != nil || observed.Ready || observed.Node != "" {
		t.Fatal("unallocated deleting Machine entered compute lifecycle", observed, err)
	}
}
