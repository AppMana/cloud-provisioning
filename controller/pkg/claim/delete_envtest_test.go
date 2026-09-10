package claim

import (
	"context"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The real API server enforces deletion preconditions. No provider controllers
// or garbage collector run here; these cases qualify the claim's delete requests.
func TestRealAPITeardownDeletionRaces(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	crd := func(gvk schema.GroupVersionKind, plural string) *apiextensionsv1.CustomResourceDefinition {
		preserve := true
		return &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: plural + "." + gvk.Group},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: gvk.Group, Scope: apiextensionsv1.NamespaceScoped,
				Names:    apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: gvk.Kind},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: gvk.Version, Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}}}},
			},
		}
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../../charts/cloud-provisioning/crds"}, ErrorIfCRDPathMissing: true, CRDs: []*apiextensionsv1.CustomResourceDefinition{crd(machineGVK, "machines"), crd(joinaws.Provider{}.GVK(), "awsmachines")}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, kind := range []string{"machine", "infra", "node"} {
		for _, race := range []string{"replacement", "update"} {
			t.Run(kind+"-"+race, func(t *testing.T) {
				claim := fakeClaim(kind + "-" + race)
				claim.UID = ""
				claim.ResourceVersion = ""
				claim.Finalizers = []string{Finalizer}
				if err := api.Create(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if err := api.Delete(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				var target client.Object
				if kind == "node" {
					target = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Annotations: map[string]string{ClaimAnnotation: claim.Namespace + "/" + claim.Name}}}
				} else {
					u := &unstructured.Unstructured{}
					u.SetGroupVersionKind(machineGVK)
					if kind == "infra" {
						u.SetGroupVersionKind(joinaws.Provider{}.GVK())
					}
					u.SetNamespace(claim.Namespace)
					u.SetName(claim.Name)
					target = u
				}
				if err := api.Create(ctx, target); err != nil {
					t.Fatal(err)
				}
				oldUID := target.GetUID()
				racing := &deleteRaceClient{Client: api, before: func() {
					if race == "replacement" {
						if err := api.Delete(ctx, target); err != nil {
							t.Fatal(err)
						}
						target.SetUID("")
						target.SetResourceVersion("")
						if err := api.Create(ctx, target); err != nil {
							t.Fatal(err)
						}
						if target.GetUID() == oldUID {
							t.Fatal("replacement retained UID")
						}
					} else {
						target.SetLabels(map[string]string{"test-concurrent-change": "preserve"})
						if err := api.Update(ctx, target); err != nil {
							t.Fatal(err)
						}
					}
				}}
				r := &Reconciler{Client: racing, Reader: api, Provisioners: []join.MachineProvisioner{joinaws.Provider{}}}
				if _, err := r.reconcileDelete(ctx, claim); !apierrors.IsConflict(err) {
					t.Fatalf("expected stale deletion conflict, got %v", err)
				}
				if !racing.called {
					t.Fatal("delete race not exercised")
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
					t.Fatal("concurrent object removed", err)
				}
				if target.GetDeletionTimestamp() != nil {
					t.Fatal("concurrent object entered deletion")
				}
				if err := api.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				if !containsString(claim.Finalizers, Finalizer) {
					t.Fatal("claim released after stale deletion")
				}
			})
		}
	}
}

type deleteRaceClient struct {
	client.Client
	before func()
	called bool
}

func (c *deleteRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if !c.called {
		c.called = true
		c.before()
	}
	return c.Client.Delete(ctx, obj, opts...)
}
