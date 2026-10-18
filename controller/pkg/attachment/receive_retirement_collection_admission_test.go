package attachment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type retirementCollectionReader struct {
	typedcore.ConfigMapInterface
	before func(string) error
}

func (r retirementCollectionReader) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	if err := r.before(name); err != nil {
		return nil, err
	}
	return r.ConfigMapInterface.Get(ctx, name, opts)
}

// Real API source reads only. This read budget does not prove transport latency,
// receipt authentication, native cleanup or reservation release.
func verifyRetirementCollectionAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, machineUID string, policies map[string]handover.PodReceiptAuthenticator) {
	t.Helper()
	ctx := context.Background()
	source, err := ReadReceiveRetirementSet(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	reader := store
	reader.ConfigMaps = retirementCollectionReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
		if name == store.Name {
			reads++
		}
		return nil
	}}
	if _, err := source.ReceiptCollection(ctx, api, reader, policies, time.Minute, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("collection reconstructed admitted history: %d journal reads, want one live recheck", reads)
	}
	if _, err := NewReceiveRetirementReceiptCollection(ctx, api, store, string(source.Metadata.UID), mesh, source.Journals[machineUID], policies, time.Minute, func() error { return nil }); err != nil {
		t.Fatal("standalone constructor compatibility", err)
	}
	for _, fault := range []string{"inventory", "mesh", "journal", "withdrawal", "missing-source", "missing-policy", "read", "policy", "cancelled", "inventory-race", "journal-race"} {
		t.Run(fault, func(t *testing.T) {
			candidate, err := ReadReceiveRetirementSet(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID)
			if err != nil {
				t.Fatal(err)
			}
			reader, trusted := store, policies
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			recheck := func() error { return nil }
			switch fault {
			case "inventory":
				candidate.Metadata.ResourceVersion = "stale"
			case "mesh":
				candidate.MeshMetadata.ResourceVersion = "stale"
			case "journal":
				reader.ExpectedUID = "replacement"
			case "withdrawal":
				candidate.Withdrawal.DrainIntent = "changed"
			case "missing-source":
				candidate = ReceiveRetirementSet{}
			case "missing-policy":
				trusted = nil
			case "read":
				reader.ConfigMaps = retirementCollectionReader{ConfigMapInterface: store.ConfigMaps, before: func(string) error { return errors.New("API unavailable") }}
			case "policy":
				recheck = func() error { return errors.New("policy changed") }
			case "cancelled":
				cancel()
			case "inventory-race", "journal-race":
				name := candidate.Metadata.Name
				if fault == "journal-race" {
					name = store.Name
				}
				original, err := store.ConfigMaps.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					current, err := store.ConfigMaps.Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					current.Annotations = original.Annotations
					if _, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				})
				recheck = func() error {
					current, err := store.ConfigMaps.Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if current.Annotations == nil {
						current.Annotations = map[string]string{}
					}
					current.Annotations["test-collection-race"] = "changed"
					_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
					return err
				}
			}
			if _, err := candidate.ReceiptCollection(runCtx, api, reader, trusted, time.Minute, recheck); err == nil {
				t.Fatal("changed authority produced receipt collection")
			}
		})
	}
}
