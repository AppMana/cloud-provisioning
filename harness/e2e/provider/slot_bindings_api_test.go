package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type slotAPIBastion struct{ rig.Node }

func (slotAPIBastion) Exec(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", append([]string{"exec", "clab-cldt-bastion"}, args...)...).Output()
}
func (slotAPIBastion) Pipe(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", "clab-cldt-bastion"}, args...)...)
	cmd.Stdin = input
	return cmd.Output()
}

func verifyPooledBindings(t *testing.T, ctx context.Context, ns string) {
	t.Helper()
	k := &kube.Client{Bastion: slotAPIBastion{}, ControlPlanes: []string{"10.10.0.10"}}
	store := &SlotStore{API: k, Namespace: ns, Name: "binding-pool", Lab: "cldt", Slots: []string{"remote1", "remote2"}}
	controller := &Controller{Kube: k, Topology: lab.Default(), LabName: "cldt", Slots: store}
	for name, binding := range map[string]string{"aaa-generated-0": "", "zzz-fixed": "remote1"} {
		data, _ := json.Marshal(map[string]any{"apiVersion": "containernet.appmana.com/v1beta2", "kind": "ContainernetMachine", "metadata": map[string]string{"name": name, "namespace": ns}, "spec": map[string]string{"containerName": binding}})
		if err := k.Apply(ctx, data); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"aaa-generated-0", "zzz-fixed"}
	if _, err := controller.reserveEligibleBindings(ctx, ns, names); err != nil {
		t.Fatal(err)
	}
	if err := controller.checkDistinctBindings(ctx, ns, names); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"aaa-generated-0": "remote2", "zzz-fixed": "remote1"} {
		raw, err := k.Run(ctx, "-n", ns, "get", machineKind, name, "-o", "json")
		if err != nil {
			t.Fatal(err)
		}
		var obj struct {
			Metadata struct {
				UID         string            `json:"uid"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Metadata.Annotations[containerNameAnnotation] != want {
			t.Fatalf("%s took %s, expected %s", name, obj.Metadata.Annotations[containerNameAnnotation], want)
		}
		if slot, err := store.Reserve(ctx, SlotOwner{Namespace: ns, Name: name, UID: obj.Metadata.UID}, ""); err != nil || slot != want {
			t.Fatal("binding lacks matching reservation", slot, err)
		}
	}
	// Reconstructed controller retains bindings and allocates no additional slot.
	restarted := &Controller{Kube: k, Topology: lab.Default(), LabName: "cldt", Slots: &SlotStore{API: k, Namespace: ns, Name: "binding-pool", Lab: "cldt", Slots: []string{"remote1", "remote2"}}}
	if _, err := restarted.reserveEligibleBindings(ctx, ns, names); err != nil {
		t.Fatal(err)
	}
	verifySlotRelease(t, ctx, *store)
	verifyUnallocatedCancellation(t, ctx, controller, ns)
	verifyCapacityEligibility(t, ctx, k, ns)

}

// The native four-worker campaign exposed namespace-wide starvation when one
// allocation returned capacity pending. Exercise that planning boundary with
// real UID/version writes; this test does not boot or modify any VM.
func verifyCapacityEligibility(t *testing.T, ctx context.Context, k *kube.Client, ns string) {
	t.Helper()
	names := []string{"capacity-0", "capacity-1", "capacity-2"}
	for _, name := range names {
		raw, _ := json.Marshal(map[string]any{"apiVersion": "containernet.appmana.com/v1beta2", "kind": "ContainernetMachine", "metadata": map[string]string{"name": name, "namespace": ns}, "spec": map[string]any{}})
		if err := k.Apply(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	c := &Controller{Kube: k, Topology: lab.Default(), LabName: "cldt", Slots: &SlotStore{API: k, Namespace: ns, Name: "capacity-pool", Lab: "cldt", Slots: []string{"remote1", "remote2"}}}
	for attempt := 0; attempt < 2; attempt++ {
		eligible, err := c.reserveEligibleBindings(ctx, ns, names)
		if !errors.Is(err, ErrSlotCapacity) || !slices.Equal(eligible, names[:2]) {
			t.Fatalf("eligible=%v err=%v", eligible, err)
		}
		if err := c.checkDistinctBindings(ctx, ns, eligible); err != nil {
			t.Fatal(err)
		}
		// Reconstruct the controller and store; persisted reservations are retained.
		copyStore := *c.Slots
		c = &Controller{Kube: k, Topology: lab.Default(), LabName: "cldt", Slots: &copyStore}
	}
	raw, err := k.Run(ctx, "-n", ns, "get", machineKind, "capacity-2", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Metadata struct{ Annotations map[string]string }
		Spec     struct{ ProviderID string }
		Status   struct{ Ready bool }
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Metadata.Annotations[containerNameAnnotation] != "" || pending.Spec.ProviderID != "" || pending.Status.Ready {
		t.Fatal("unallocated Machine acquired a binding or readiness")
	}
}
