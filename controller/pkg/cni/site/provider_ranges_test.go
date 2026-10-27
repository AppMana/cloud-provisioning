package site

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ipPool(name, cidr string, disabled bool) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipPoolGVK)
	pool.SetName(name)
	pool.Object["spec"] = map[string]any{"cidr": cidr, "disabled": disabled}
	return pool
}

func providerPools(t *testing.T, c client.Client) map[string]map[string]any {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ipPoolGVK.GroupVersion().WithKind("IPPoolList"))
	if err := c.List(context.Background(), list, client.MatchingLabels{platform.ComponentLabel: providerRangeComponent}); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, p := range list.Items {
		spec, _, _ := unstructured.NestedMap(p.Object, "spec")
		out[spec["cidr"].(string)] = spec
	}
	return out
}

// On a Calico site, each range a provider-native network gives pods is a
// disabled Calico pool: Calico allocates nothing from it and advertises
// nothing, but treats it as cluster-internal, so site pods reaching AWS pods
// keep their source instead of being masqueraded. A native node is held
// until those pools exist, and a range no provider uses any more loses its
// pool.
func TestProviderRangesBecomeDisabledCalicoPools(t *testing.T) {
	ctx := context.Background()
	ranges := []netip.Prefix{netip.MustParsePrefix("172.29.0.0/25")}
	e, c := excluder(t, k0sCalicoNode(t), ipPool("default-ipv4-ippool", "10.101.128.0/17", false))
	e.ProviderRanges = func(context.Context) ([]netip.Prefix, error) { return ranges, nil }
	var pending *PendingError
	if err := e.verifyProviderPools(ctx); !errors.As(err, &pending) {
		t.Fatalf("a native node was admitted before its range's pool existed: %v", err)
	}
	if err := e.ensureProviderPools(ctx); err != nil {
		t.Fatal(err)
	}
	pools := providerPools(t, c)
	spec := pools["172.29.0.0/25"]
	if len(pools) != 1 || spec["disabled"] != true || spec["nodeSelector"] != "!all()" || spec["natOutgoing"] != false ||
		spec["disableBGPExport"] != true || spec["ipipMode"] != "Never" || spec["vxlanMode"] != "Never" {
		t.Fatalf("pools = %v", pools)
	}
	if err := e.Verify(ctx, cni.Network{Name: cni.Calico}, nativeClass); err != nil && !errors.As(err, &pending) {
		t.Fatal(err)
	}
	if err := e.verifyProviderPools(ctx); err != nil {
		t.Fatal(err)
	}

	ranges = []netip.Prefix{netip.MustParsePrefix("172.29.1.0/25")}
	if err := e.ensureProviderPools(ctx); err != nil {
		t.Fatal(err)
	}
	if pools := providerPools(t, c); len(pools) != 1 || pools["172.29.1.0/25"] == nil {
		t.Fatalf("after the range changed: %v", pools)
	}
}

// A provider range inside one of the site's own enabled pools would be
// allocated twice; that is refused, not papered over.
func TestAProviderRangeInsideASitePoolIsRefused(t *testing.T) {
	e, _ := excluder(t, ipPool("default-ipv4-ippool", "172.29.0.0/16", false))
	e.ProviderRanges = func(context.Context) ([]netip.Prefix, error) {
		return []netip.Prefix{netip.MustParsePrefix("172.29.0.0/25")}, nil
	}
	var unsupported *UnsupportedError
	if err := e.verifyProviderPools(context.Background()); !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "default-ipv4-ippool") {
		t.Fatalf("verify = %v", err)
	}
	if err := e.ensureProviderPools(context.Background()); err == nil {
		t.Fatal("a pool was made inside the site's own")
	}
}
