package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func TestDuplicateBindingsStopBeforeAnyProvisioningWrite(t *testing.T) {
	for _, second := range []string{
		`{"spec":{"containerName":"remote1"}}`,
		`{"metadata":{"annotations":{"containernet.appmana.com/container-name":"clab-cldt-remote1"}},"spec":{"containerName":"remote2"}}`,
		`{"metadata":{"deletionTimestamp":"2026-09-09T00:00:00Z"},"spec":{"containerName":"clab-cldt-remote1"}}`,
	} {
		k := &fakeKube{objects: map[string]string{"group-slot-0": `{"spec":{"containerName":"clab-cldt-remote1"}}`, "group-slot-1": second}}
		c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt"}
		if _, err := c.Reconcile(context.Background(), "cloud-provisioning"); err == nil || !strings.Contains(err.Error(), "bound to both") {
			t.Fatalf("duplicate slot accepted: %v", err)
		}
		for _, call := range k.calls {
			for _, verb := range []string{"patch", "create", "delete", "apply", "exec"} {
				if contains(call, verb) {
					t.Fatalf("mutation before duplicate rejection: %v", call)
				}
			}
		}
	}
}
