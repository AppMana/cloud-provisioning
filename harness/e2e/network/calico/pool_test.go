package calico

import (
	"strings"
	"testing"
)

// The stock manifest leaves CALICO_IPV4POOL_CIDR commented out, and
// calico-node then creates its default pool from its own built-in
// 192.168.0.0/16 whatever the cluster was configured with. On kubeadm
// the two happened to agree; on k0s they did not, and the row passed
// with the mesh carrying 192.168.159.0/26 for a cluster told
// 10.244.0.0/16.
//
// The pool's CIDR is immutable once created, so this has to be set
// before calico-node first runs.
func TestThePoolIsPinnedToTheClustersPodCIDR(t *testing.T) {
	manifest := `            - name: CLUSTER_TYPE
              value: "k8s,bgp"
            # - name: CALICO_IPV4POOL_CIDR
            #   value: "192.168.0.0/16"
            - name: CALICO_DISABLE_FILE_LOGGING
`
	got := string(pinPool([]byte(manifest), "10.244.0.0/16"))

	if strings.Contains(got, "# - name: CALICO_IPV4POOL_CIDR") {
		t.Error("the variable is still commented out, so Calico keeps its own default")
	}
	if !strings.Contains(got, `- name: CALICO_IPV4POOL_CIDR`) {
		t.Fatalf("the variable was not set:\n%s", got)
	}
	if !strings.Contains(got, `value: "10.244.0.0/16"`) {
		t.Errorf("the pool was not pinned to the cluster's CIDR:\n%s", got)
	}
	if strings.Contains(got, "192.168.0.0/16") {
		t.Error("Calico's own default survived")
	}
	// Everything else is left alone.
	if !strings.Contains(got, "CALICO_DISABLE_FILE_LOGGING") {
		t.Error("the edit disturbed the rest of the manifest")
	}
}

// A manifest that does not carry the variable in the form expected is
// left untouched rather than silently half-edited: the assertion
// after the install is what then catches it.
func TestAnUnrecognisedManifestIsLeftAlone(t *testing.T) {
	manifest := []byte("no such variable here\n")
	if got := string(pinPool(manifest, "10.244.0.0/16")); got != string(manifest) {
		t.Errorf("an unrecognised manifest was edited: %q", got)
	}
}
