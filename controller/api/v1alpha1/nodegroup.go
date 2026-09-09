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
type ProvisionedNodeGroupClaimStatus struct {
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
