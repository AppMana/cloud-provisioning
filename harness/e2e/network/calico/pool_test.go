package calico

import (
	"strings"
	"testing"
)

// The pool has to be the cluster's own pod CIDR, and it has to be set
// before calico-node first runs, because a pool's CIDR is immutable
// once created.
//
// Left alone, calico-node creates its default pool from its own
// built-in 192.168.0.0/16 whatever the cluster was configured with.
// On kubeadm the two happened to agree; on k0s they did not, and the
// row passed anyway with the mesh carrying 192.168.159.0/26 for a
// cluster told 10.244.0.0/16. It passes because the product reads the
// network's own records rather than the cluster's configuration,
// which is the right way round — but a lab whose pod network is not
// the one the row says it installed makes two distributions
// incomparable.
func TestThePoolIsPinnedToTheClustersOwnCIDR(t *testing.T) {
	manifest := `        env:
            # - name: CALICO_IPV4POOL_CIDR
            #   value: "192.168.0.0/16"
            - name: CALICO_DISABLE_FILE_LOGGING
`
	out := string(pinPool([]byte(manifest), "10.244.0.0/16"))

	if !strings.Contains(out, `value: "10.244.0.0/16"`) {
		t.Errorf("the pool was not pinned to the cluster's CIDR:\n%s", out)
	}
	if strings.Contains(out, "192.168.0.0/16") {
		t.Errorf("calico's built-in default survives, so it may allocate from it:\n%s", out)
	}
	if strings.Contains(out, "# - name: CALICO_IPV4POOL_CIDR") {
		t.Errorf("the variable is still commented out, so calico never reads it:\n%s", out)
	}
}

// A manifest whose commented block has changed shape must not pass
// silently: the substitution would do nothing and the pool would come
// from calico's built-in default again, which is the failure this
// exists to prevent.
func TestAManifestThatCannotBePinnedIsNoticed(t *testing.T) {
	manifest := "        env:\n            - name: SOMETHING_ELSE\n"
	out := string(pinPool([]byte(manifest), "10.244.0.0/16"))
	if strings.Contains(out, "10.244.0.0/16") {
		t.Fatal("something was pinned in a manifest that carries no pool variable")
	}
	if out != manifest {
		t.Error("the manifest was changed in some other way")
	}
}
