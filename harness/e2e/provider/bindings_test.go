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

func TestPooledProviderIdentityIncludesInfrastructureIncarnation(t *testing.T) {
	for _, uid := range []string{"old-uid", "replacement-uid"} {
		k := &fakeKube{objects: map[string]string{"generated-0": `{"metadata":{"uid":"` + uid + `","annotations":{"containernet.appmana.com/container-name":"remote1"}},"spec":{}}`}}
		c := &Controller{Kube: k.client(), Topology: lab.Default(), LabName: "cldt", Slots: &SlotStore{}}
		observed, err := c.reconcileOne(context.Background(), "test", "generated-0")
		if err != nil || observed.ProviderID != "containernet://remote1/"+uid {
			t.Fatal("provider identity lost incarnation", observed, err)
		}
		found := false
		for _, call := range k.calls {
			if strings.Contains(strings.Join(call, " "), `"providerID":"containernet://remote1/`+uid+`"`) {
				found = true
			}
		}
		if !found {
			t.Fatal("UID-bound provider identity not published")
		}
	}
}
