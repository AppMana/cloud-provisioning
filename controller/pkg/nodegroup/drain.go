package nodegroup

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DrainNode performs at most one mutation per call. The caller must persist the
// resolved Node UID in its removal intent before invoking it. Empty means that
// workload pods are gone; attachment withdrawal remains a separate required gate.
// The client must read directly from the workload cluster API.
func DrainNode(ctx context.Context, api client.Client, name string, uid types.UID) (empty bool, err error) {
	if api == nil || name == "" || uid == "" {
		return false, fmt.Errorf("workload API and node identity required")
	}
	node := &corev1.Node{}
	if err = api.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		return false, err
	}
	if node.UID != uid || !node.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("drain node identity changed or is deleting")
	}
	if !node.Spec.Unschedulable {
		node.Spec.Unschedulable = true
		return false, api.Update(ctx, node)
	}
	pods := &corev1.PodList{}
	if err = api.List(ctx, pods, client.MatchingFields{"spec.nodeName": name}); err != nil {
		return false, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		a, b := pods.Items[i], pods.Items[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != name {
			return false, fmt.Errorf("pod list returned a different node")
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Static pods and DaemonSets persist while the node runs. Network agents
		// must remain available until the later attachment withdrawal stage.
		if pod.Annotations[corev1.MirrorPodAnnotationKey] != "" {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" {
			continue
		}
		if !pod.DeletionTimestamp.IsZero() {
			return false, nil
		}
		if pod.UID == "" {
			return false, fmt.Errorf("pod UID required for eviction")
		}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}}
		err = api.SubResource("eviction").Create(ctx, pod, eviction)
		if apierrors.IsTooManyRequests(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
