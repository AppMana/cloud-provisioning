package nodegroup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AuthorizeReceiveWithdrawal is the read-only lifecycle check supplied to
// retirement publication. Both clients must bypass caches. It reuses bound
// claim/Machine/Node admission and the drain pod policy; it neither evicts pods
// nor repairs a missing drain marker. Publication and cleanup remain separate.
func AuthorizeReceiveWithdrawal(ctx context.Context, api client.Client, workload client.Reader, gvk schema.GroupVersionKind, group *v1alpha1.ProvisionedNodeGroupClaim, intent attachment.RemoteWithdrawalIntent) error {
	if _, err := drainWorker(group); err != nil {
		return err
	}
	a := group.Status.PendingAction
	if api == nil || workload == nil || a.Removing || a.Withdrawal == nil || a.Gateways == nil {
		return fmt.Errorf("active captured withdrawal required")
	}
	expected := groupReceiveWithdrawalIntent(group)
	if a.Withdrawal.MeshName != a.Gateways.Mesh || !reflect.DeepEqual(intent, expected) {
		return fmt.Errorf("receiver retirement differs from captured drain intent")
	}
	checkGroup := func() error {
		current := &v1alpha1.ProvisionedNodeGroupClaim{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
			return err
		}
		if current.UID != group.UID || current.ResourceVersion != group.ResourceVersion || !reflect.DeepEqual(current.Status.PendingAction, a) {
			return fmt.Errorf("drain action changed during retirement authorization")
		}
		return nil
	}
	if err := checkGroup(); err != nil {
		return err
	}
	// drainWorker requires an already bound target, so this existing admission
	// path cannot persist a new binding during authorization.
	if _, err := BindDrainNode(ctx, api, workload, gvk, group); err != nil {
		return err
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: a.ChildName}, machine); err != nil {
		return err
	}
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if string(machine.GetUID()) != a.MachineUID || provider != a.ProviderID || !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != intent.DrainIntent {
		return fmt.Errorf("Machine drain authority changed")
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, client.ObjectKey{Name: a.NodeName}, node); err != nil {
		return err
	}
	if string(node.UID) != a.NodeUID || node.Spec.ProviderID != a.ProviderID || !node.DeletionTimestamp.IsZero() || !node.Spec.Unschedulable {
		return fmt.Errorf("withdrawal target is not the drained Node")
	}
	pods := &corev1.PodList{}
	if err := workload.List(ctx, pods, client.MatchingFields{"spec.nodeName": a.NodeName}); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != a.NodeName || !drainPodExempt(pod) {
			return fmt.Errorf("workload remains on withdrawal target")
		}
	}
	latestMachine := &unstructured.Unstructured{}
	latestMachine.SetGroupVersionKind(gvk)
	if err := api.Get(ctx, client.ObjectKeyFromObject(machine), latestMachine); err != nil {
		return err
	}
	latestNode := &corev1.Node{}
	if err := workload.Get(ctx, client.ObjectKeyFromObject(node), latestNode); err != nil {
		return err
	}
	if machine.GetResourceVersion() == "" || node.ResourceVersion == "" || latestMachine.GetUID() != machine.GetUID() || latestMachine.GetResourceVersion() != machine.GetResourceVersion() || latestNode.UID != node.UID || latestNode.ResourceVersion != node.ResourceVersion || !latestMachine.GetDeletionTimestamp().IsZero() || !latestNode.DeletionTimestamp.IsZero() {
		return fmt.Errorf("drain target changed during workload observation")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return checkGroup()
}

func groupReceiveWithdrawalIntent(group *v1alpha1.ProvisionedNodeGroupClaim) attachment.RemoteWithdrawalIntent {
	a := group.Status.PendingAction
	w := a.Withdrawal
	expected := attachment.RemoteWithdrawalIntent{MeshNamespace: group.Namespace, MeshName: w.MeshName, MeshUID: w.MeshUID, SourceVersion: w.SourceVersion, SourceDataHash: w.SourceDataHash, MachineName: a.ChildName, MachineUID: a.MachineUID, PublicKey: w.PublicKey, DrainIntent: string(group.UID) + "/" + a.ID}
	for _, c := range w.Consumers {
		expected.Consumers = append(expected.Consumers, attachment.ConsumerTarget{NodeName: c.NodeName, NodeUID: c.NodeUID, Site: c.Site, MachineName: c.MachineName, TunnelAddress: c.TunnelAddress, PublicKey: c.PublicKey, SecretUID: c.SecretUID})
	}
	return expected
}
