package site

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// A provider-native network gives pods addresses from its own ranges (the
// AWS VPC CNI from the VPC's subnets). Calico masquerades a pod's traffic
// to any address outside its IP pools, so site pods reaching those pods
// would arrive from their node's address. Calico documents the remedy: a
// disabled pool covering the range, which Calico allocates nothing from and
// advertises nothing for, but counts as cluster-internal.

var ipPoolGVK = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPool"}

// providerRangeComponent labels the pools this product owns.
const providerRangeComponent = "provider-range"

func providerPoolName(p netip.Prefix) string {
	r := strings.NewReplacer(".", "-", ":", "-", "/", "-")
	return "cloud-provisioning-" + strings.Trim(r.Replace(p.String()), "-")
}

func (e *Excluder) pools(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ipPoolGVK.GroupVersion().WithKind("IPPoolList"))
	if err := e.Reader.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing Calico IP pools: %w", err)
	}
	return list.Items, nil
}

// providerRanges are the provider ranges, refused where one lies in a pool
// the site allocates from.
func (e *Excluder) providerRanges(ctx context.Context, pools []unstructured.Unstructured) ([]netip.Prefix, error) {
	if e.ProviderRanges == nil {
		return nil, nil
	}
	ranges, err := e.ProviderRanges(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading provider pod ranges: %w", err)
	}
	for _, pool := range pools {
		if pool.GetLabels()[platform.ComponentLabel] == providerRangeComponent {
			continue
		}
		if disabled, _, _ := unstructured.NestedBool(pool.Object, "spec", "disabled"); disabled {
			continue
		}
		raw, _, _ := unstructured.NestedString(pool.Object, "spec", "cidr")
		cidr, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		for _, r := range ranges {
			if r.Overlaps(cidr) {
				return nil, &UnsupportedError{Reason: fmt.Sprintf("provider pod range %s overlaps Calico pool %s (%s), which the site allocates from", r, pool.GetName(), cidr)}
			}
		}
	}
	return ranges, nil
}

// ensureProviderPools makes a disabled pool for every provider range and
// removes those for ranges no provider uses any more.
func (e *Excluder) ensureProviderPools(ctx context.Context) error {
	pools, err := e.pools(ctx)
	if err != nil {
		return err
	}
	ranges, err := e.providerRanges(ctx, pools)
	if err != nil {
		return err
	}
	want := map[string]netip.Prefix{}
	for _, r := range ranges {
		want[providerPoolName(r)] = r.Masked()
	}
	for _, pool := range pools {
		if pool.GetLabels()[platform.ComponentLabel] != providerRangeComponent {
			continue
		}
		if _, keep := want[pool.GetName()]; keep {
			delete(want, pool.GetName())
			continue
		}
		if err := e.Client.Delete(ctx, &pool); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing Calico pool %s: %w", pool.GetName(), err)
		}
		logf.FromContext(ctx).Info("removed the Calico pool of a provider range no longer used", "pool", pool.GetName())
	}
	for name, r := range want {
		pool := &unstructured.Unstructured{}
		pool.SetGroupVersionKind(ipPoolGVK)
		pool.SetName(name)
		pool.SetLabels(map[string]string{platform.ComponentLabel: providerRangeComponent})
		pool.Object["spec"] = map[string]any{
			"cidr": r.String(), "disabled": true, "nodeSelector": "!all()",
			"natOutgoing": false, "disableBGPExport": true, "ipipMode": "Never", "vxlanMode": "Never",
		}
		if err := e.Client.Create(ctx, pool); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating Calico pool %s for provider range %s: %w", name, r, err)
		}
		logf.FromContext(ctx).Info("made a provider range cluster-internal to Calico", "pool", name, "range", r)
	}
	return nil
}

// verifyProviderPools reports whether every provider range has its pool.
func (e *Excluder) verifyProviderPools(ctx context.Context) error {
	pools, err := e.pools(ctx)
	if err != nil {
		return err
	}
	ranges, err := e.providerRanges(ctx, pools)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, pool := range pools {
		if pool.GetLabels()[platform.ComponentLabel] == providerRangeComponent {
			have[pool.GetName()] = true
		}
	}
	var missing []string
	for _, r := range ranges {
		if !have[providerPoolName(r)] {
			missing = append(missing, "IPPool "+providerPoolName(r)+" for provider range "+r.String())
		}
	}
	if len(missing) > 0 {
		return &PendingError{DaemonSets: missing}
	}
	return nil
}

