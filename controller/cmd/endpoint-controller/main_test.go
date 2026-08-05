package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The selector that says which nodes terminate a tunnel becomes the
// dialer DaemonSet's node affinity. Naming two nodes takes a set based
// term, whose commas belong to the term rather than separating terms,
// so splitting on commas produces fragments that match nothing and an
// affinity with no requirements at all. That does not fail: it places
// a dialer on every node, which is the opposite of what was asked for.
func TestParseSelectorRequirements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  []corev1.NodeSelectorRequirement
		empty bool
	}{
		{
			name: "one node by name",
			raw:  "kubernetes.io/hostname=cldt-worker",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"cldt-worker"},
			}},
		},
		{
			name: "two nodes, the case that was silently dropped",
			raw:  "kubernetes.io/hostname in (cldt-worker,cldt-worker2)",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"cldt-worker", "cldt-worker2"},
			}},
		},
		{
			name: "a label that only has to exist",
			raw:  "node-role.kubernetes.io/control-plane",
			want: []corev1.NodeSelectorRequirement{{
				Key: "node-role.kubernetes.io/control-plane", Operator: corev1.NodeSelectorOpExists,
			}},
		},
		{
			name: "excluding a node",
			raw:  "kubernetes.io/hostname!=cldt-worker",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"cldt-worker"},
			}},
		},
		{
			// Nothing is better than a term that matches everything.
			name:  "nonsense selects nothing rather than everything",
			raw:   "this is not a selector",
			empty: true,
		},
		{
			// A label parser reads this as "the label all must exist",
			// which places the DaemonSet on no node at all.
			name:  "the every-node sentinel constrains nothing",
			raw:   "all",
			empty: true,
		},
		{
			name:  "the other spelling of it",
			raw:   "*",
			empty: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSelectorRequirements(tc.raw)
			if tc.empty {
				if len(got) != 0 {
					t.Fatalf("parseSelectorRequirements(%q) = %+v, want none", tc.raw, got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseSelectorRequirements(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i].Key != tc.want[i].Key || got[i].Operator != tc.want[i].Operator {
					t.Errorf("requirement %d = %+v, want %+v", i, got[i], tc.want[i])
				}
				if len(got[i].Values) != len(tc.want[i].Values) {
					t.Fatalf("requirement %d values = %v, want %v", i, got[i].Values, tc.want[i].Values)
				}
				for j := range got[i].Values {
					if got[i].Values[j] != tc.want[i].Values[j] {
						t.Errorf("requirement %d value %d = %q, want %q", i, j, got[i].Values[j], tc.want[i].Values[j])
					}
				}
			}
		})
	}
}

// "all" means every node at this site, not every node in the cluster.
//
// A node this operator provisioned is on the far side of a tunnel. It
// is addressed by its tunnel address, because that is the only address
// the site can reach it by, and it already reaches the mesh as a peer.
// Counting it as one of the site's own ends gives it a contradictory
// second identity: measured, its address was pinned to the one the
// site cannot reach it by, and the tunnel address the mesh had
// assigned was overwritten.
func TestIsTunnelEndpoint_NeverAProvisionedNode(t *testing.T) {
	provisioned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "public-worker",
		Labels: map[string]string{"kubernetes.io/os": "linux", cloudWorkerRoleLabel: cloudWorkerRoleValue},
	}}
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "worker",
		Labels: map[string]string{"kubernetes.io/os": "linux"},
	}}

	for _, raw := range []string{"all", "*", ""} {
		r := &meshReconciler{tunnelEndpointsRaw: raw}
		if r.isTunnelEndpoint(provisioned) {
			t.Errorf("selector %q counted a provisioned node as one of the site's tunnel endpoints", raw)
		}
		if !r.isTunnelEndpoint(site) {
			t.Errorf("selector %q did not count an ordinary site node", raw)
		}
	}
}
