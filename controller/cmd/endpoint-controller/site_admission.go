package main

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Site admission is independent of endpoint placement: controllers and other
// non-endpoints still need transit agents before a cloud worker is admitted.
func (r *meshReconciler) validateSiteDialerAdmission(nodes []corev1.Node) error {
	if r.siteDialerSelector == nil || r.siteDialerSelector.Empty() {
		return nil
	}
	for i := range nodes {
		n := &nodes[i]
		if r.isTunnelEndpoint(n) && !r.siteDialerSelector.Matches(labels.Set(n.Labels)) {
			return fmt.Errorf("tunnel endpoint %s is outside site dialer admission", n.Name)
		}
	}
	return nil
}

func (r *meshReconciler) siteDialerNodeAffinity() *corev1.NodeSelector {
	affinity := dialerNodeAffinity()
	if r.siteDialerSelector != nil && !r.siteDialerSelector.Empty() {
		// The selector was parsed at startup; conversion must never turn an
		// invalid operator or expression into an unrestricted affinity.
		affinity.NodeSelectorTerms[0].MatchExpressions = append(
			affinity.NodeSelectorTerms[0].MatchExpressions,
			parseSelectorRequirements(r.siteDialerSelector.String())...,
		)
	}
	return affinity
}

// Pin admission to node names. DaemonSet controllers evict pods when labels stop
// matching, even with IgnoredDuringExecution affinity. An admission-label edit
// must not silently remove a transit agent. Removal needs a separate, verified
// withdrawal workflow; until then admission is additive.
func (r *meshReconciler) pinSiteDialerAdmission(ctx context.Context, desired, existing *appsv1.DaemonSet) error {
	if r.siteDialerSelector == nil || r.siteDialerSelector.Empty() {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.reader.List(ctx, nodes); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, n := range nodes.Items {
		if n.Labels["kubernetes.io/os"] == "linux" && n.Labels[cloudWorkerRoleLabel] != cloudWorkerRoleValue && r.siteDialerSelector.Matches(labels.Set(n.Labels)) {
			names[n.Name] = true
		}
	}
	if existing != nil {
		// Also retain actual scheduled agents when upgrading a legacy unpinned DS.
		pods := &corev1.PodList{}
		if err := r.reader.List(ctx, pods, client.InNamespace(existing.Namespace)); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			for _, owner := range pod.OwnerReferences {
				if owner.UID == existing.UID && owner.Kind == "DaemonSet" && pod.Spec.NodeName != "" {
					names[pod.Spec.NodeName] = true
				}
			}
		}
		if a := existing.Spec.Template.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				for _, field := range term.MatchFields {
					if field.Key == "metadata.name" && field.Operator == corev1.NodeSelectorOpIn {
						for _, name := range field.Values {
							names[name] = true
						}
					}
				}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	// No terms matches no nodes; never emit an empty In expression.
	affinity := &corev1.NodeSelector{}
	if len(ordered) > 0 {
		affinity.NodeSelectorTerms = []corev1.NodeSelectorTerm{{MatchExpressions: dialerNodeAffinity().NodeSelectorTerms[0].MatchExpressions, MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: ordered}}}}
	}
	desired.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = affinity
	return nil
}
