package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ImportedControlPlane presents an existing cluster's own control plane to
// Cluster API, so CAPI can manage cloud workers joining a cluster it did not
// create. Name it after the CAPI Cluster that references it.
type ImportedControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImportedControlPlaneSpec   `json:"spec"`
	Status ImportedControlPlaneStatus `json:"status,omitempty"`
}

// ImportedControlPlaneSpec has no fields: the control plane is observed,
// never configured.
type ImportedControlPlaneSpec struct{}

// ImportedControlPlaneStatus is the CAPI v1beta2 control-plane contract.
type ImportedControlPlaneStatus struct {
	// ExternalManagedControlPlane tells CAPI the control plane has no
	// Machines of its own, so worker drain and hooks still run.
	ExternalManagedControlPlane bool `json:"externalManagedControlPlane,omitempty"`
	// Initialization reports the observed API serving.
	Initialization ImportedControlPlaneInitialization `json:"initialization,omitempty"`
	// Version is the Kubernetes version the API server reports.
	Version string `json:"version,omitempty"`
	// Conditions carries Available.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ImportedControlPlaneInitialization is the contract's initialization block.
type ImportedControlPlaneInitialization struct {
	ControlPlaneInitialized bool `json:"controlPlaneInitialized,omitempty"`
}

// ImportedControlPlaneList is the list type.
type ImportedControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImportedControlPlane `json:"items"`
}

func (in *ImportedControlPlaneStatus) DeepCopyInto(out *ImportedControlPlaneStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ImportedControlPlane) DeepCopyInto(out *ImportedControlPlane) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ImportedControlPlane) DeepCopy() *ImportedControlPlane {
	if in == nil {
		return nil
	}
	out := new(ImportedControlPlane)
	in.DeepCopyInto(out)
	return out
}

func (in *ImportedControlPlane) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *ImportedControlPlaneList) DeepCopyInto(out *ImportedControlPlaneList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ImportedControlPlane, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ImportedControlPlaneList) DeepCopy() *ImportedControlPlaneList {
	if in == nil {
		return nil
	}
	out := new(ImportedControlPlaneList)
	in.DeepCopyInto(out)
	return out
}

func (in *ImportedControlPlaneList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
