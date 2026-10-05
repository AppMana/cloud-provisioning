package cni

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A cluster without a network's API group answers the client's lookup
// with a discovery failure wrapping "no matches", not with the "no
// matches for kind" text older clients produced. Observed read-only
// against a production k0s cluster through controller-runtime v0.24:
// *apiutil.ErrResourceDiscoveryFailed, "unable to retrieve the complete
// list of server APIs: operator.openshift.io/v1: no matches for
// operator.openshift.io/v1, Resource=". That is an absent network, and
// treating it as a failure to read stopped the controller at startup on
// every cluster that is not OpenShift.
func TestAnAbsentAPIGroupIsAnAbsentNetwork(t *testing.T) {
	base := newClient(ipPool("default-ipv4-ippool", "10.244.0.0/16", "Never", "Never"))
	absent := func(gvk schema.GroupVersionKind) error {
		gv := gvk.GroupVersion()
		return &apiutil.ErrResourceDiscoveryFailed{gv: &apimeta.NoResourceMatchError{PartialResource: gv.WithResource("")}}
	}
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if gvk := obj.GetObjectKind().GroupVersionKind(); gvk.Group == "operator.openshift.io" {
				return absent(gvk)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	network, err := Detect(context.Background(), c)
	if err != nil {
		t.Fatalf("Detect on a cluster without OpenShift's network operator: %v", err)
	}
	if network.Name != Calico || network.Encapsulation != Native {
		t.Fatalf("network = %+v, want native Calico", network)
	}
}
