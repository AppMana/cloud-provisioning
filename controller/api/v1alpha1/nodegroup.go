package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ProvisionedNodeGroupClaimSpec describes machine capacity. Replicas counts
// machines, while WorkloadSelector identifies the pods served by that capacity.
type ProvisionedNodeGroupClaimSpec struct {
	Replicas         *int32                       `json:"replicas,omitempty"`
	Template         ProvisionedNodeClaimTemplate `json:"template"`
	WorkloadSelector *metav1.LabelSelector        `json:"workloadSelector,omitempty"`
}
type ProvisionedNodeClaimTemplate struct {
	Spec ProvisionedNodeClaimSpec `json:"spec"`
}
type GroupGatewayRequest struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}
type GroupGatewayInventory struct {
	Mesh     string                `json:"mesh"`
	Requests []GroupGatewayRequest `json:"requests"`
}
type GroupPeerConsumer struct {
	NodeName      string `json:"nodeName"`
	NodeUID       string `json:"nodeUID"`
	Site          bool   `json:"site"`
	MachineName   string `json:"machineName,omitempty"`
	TunnelAddress string `json:"tunnelAddress,omitempty"`
	PublicKey     string `json:"publicKey"`
	SecretUID     string `json:"secretUID,omitempty"`
}
type GroupPeerWithdrawal struct {
	MeshName      string              `json:"meshName"`
	MeshUID       string              `json:"meshUID"`
	SourceVersion string              `json:"sourceVersion"`
	PublicKey     string              `json:"publicKey"`
	Consumers     []GroupPeerConsumer `json:"consumers"`
}
type NodeGroupAction struct {
	Withdrawal *GroupPeerWithdrawal          `json:"withdrawal,omitempty"`
	Gateways   *GroupGatewayInventory        `json:"gateways,omitempty"`
	NodeName   string                        `json:"nodeName,omitempty"`
	NodeUID    string                        `json:"nodeUID,omitempty"`
	MachineUID string                        `json:"machineUID,omitempty"`
	ProviderID string                        `json:"providerID,omitempty"`
	ID         string                        `json:"id"`
	Type       string                        `json:"type"`
	Generation int64                         `json:"generation"`
	Ordinal    int32                         `json:"ordinal"`
	ChildName  string                        `json:"childName"`
	ChildUID   string                        `json:"childUID,omitempty"`
	Template   *ProvisionedNodeClaimTemplate `json:"template,omitempty"`
}

type ProvisionedNodeGroupClaimStatus struct {
	PendingAction      *NodeGroupAction   `json:"pendingAction,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Replicas           int32              `json:"replicas"`
	ReadyReplicas      int32              `json:"readyReplicas"`
	Selector           string             `json:"selector,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// ProvisionedNodeGroupClaim is experimental until its lifecycle controller and
// VM acceptance suite are complete. Its CRD currently lives with API test data.
type ProvisionedNodeGroupClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProvisionedNodeGroupClaimSpec   `json:"spec"`
	Status            ProvisionedNodeGroupClaimStatus `json:"status,omitempty"`
}
type ProvisionedNodeGroupClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedNodeGroupClaim `json:"items"`
}

func (in *ProvisionedNodeGroupClaim) DeepCopyInto(out *ProvisionedNodeGroupClaim) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Status.PendingAction != nil {
		action := *in.Status.PendingAction
		out.Status.PendingAction = &action
		if action.Withdrawal != nil {
			withdrawal := *action.Withdrawal
			withdrawal.Consumers = append([]GroupPeerConsumer{}, action.Withdrawal.Consumers...)
			out.Status.PendingAction.Withdrawal = &withdrawal
		}
		if action.Gateways != nil {
			inventory := *action.Gateways
			inventory.Requests = append([]GroupGatewayRequest{}, action.Gateways.Requests...)
			out.Status.PendingAction.Gateways = &inventory
		}
		if action.Template != nil {
			out.Status.PendingAction.Template = new(ProvisionedNodeClaimTemplate)
			in.Status.PendingAction.Template.Spec.DeepCopyInto(&out.Status.PendingAction.Template.Spec)
		}
	}
	in.Spec.Template.Spec.DeepCopyInto(&out.Spec.Template.Spec)
	if in.Spec.Replicas != nil {
		v := *in.Spec.Replicas
		out.Spec.Replicas = &v
	}
	if in.Spec.WorkloadSelector != nil {
		out.Spec.WorkloadSelector = in.Spec.WorkloadSelector.DeepCopy()
	}
	if in.Status.Conditions != nil {
		out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	}
}
func (in *ProvisionedNodeGroupClaim) DeepCopy() *ProvisionedNodeGroupClaim {
	if in == nil {
		return nil
	}
	out := new(ProvisionedNodeGroupClaim)
	in.DeepCopyInto(out)
	return out
}
func (in *ProvisionedNodeGroupClaim) DeepCopyObject() runtime.Object {
	if out := in.DeepCopy(); out != nil {
		return out
	}
	return nil
}
func (in *ProvisionedNodeGroupClaimList) DeepCopyInto(out *ProvisionedNodeGroupClaimList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ProvisionedNodeGroupClaim, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *ProvisionedNodeGroupClaimList) DeepCopy() *ProvisionedNodeGroupClaimList {
	if in == nil {
		return nil
	}
	out := new(ProvisionedNodeGroupClaimList)
	in.DeepCopyInto(out)
	return out
}
func (in *ProvisionedNodeGroupClaimList) DeepCopyObject() runtime.Object {
	if out := in.DeepCopy(); out != nil {
		return out
	}
	return nil
}
