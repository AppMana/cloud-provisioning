// Package v1alpha1 defines ProvisionedNodeClaim: the ONE resource a
// user commits to get a cloud node joined to the cluster over a
// WireGuard tunnel. The spec boils down to resource requests aligned
// with cloud instance types -- it names no cloud and carries no
// cloud-specific field. Which cloud fulfills the claim is decided by
// the registered provider that owns the CAPI Cluster's infrastructure
// (join.MachineProvisioner); the claim reconciler expands the claim
// into the CAPI Machine + provider machine pair, the join reconciler
// renders the tunnel-bootstrapping userdata, and the
// endpoint-controller runs the mesh (DaemonSets, endpoint mirroring,
// adoption).
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion identifies this API.
var GroupVersion = schema.GroupVersion{Group: "cloud-provisioning.appmana.com", Version: "v1alpha1"}

// ProvisionedNodeClaimSpec is the provider-agnostic ask.
type ProvisionedNodeClaimSpec struct {
	// Requests are pod-style resource requests (cpu, memory) the
	// provisioned node must satisfy. The fulfilling provider resolves
	// them to its smallest satisfying instance type.
	Requests corev1.ResourceList `json:"requests"`

	// Arch is the node architecture: arm64 (default) or amd64.
	// +optional
	Arch string `json:"arch,omitempty"`

	// InternetFacing nodes register with the internet-facing taint and
	// a public address; the public ingress data plane tolerates that
	// taint. Defaults to true -- a publicly reachable node is this
	// project's reason to exist.
	// +optional
	InternetFacing *bool `json:"internetFacing,omitempty"`

	// TunnelEndpoints selects which topologically-local nodes
	// terminate tunnels to this node. Empty selects every Linux
	// worker; control-plane nodes are excluded unless explicitly
	// selected by these terms (the other side of the tunnel must not
	// land on a controller).
	// +optional
	TunnelEndpoints *metav1.LabelSelector `json:"tunnelEndpoints,omitempty"`

	// ClusterName names the CAPI Cluster this node joins. Optional
	// when exactly one Cluster exists in the claim's namespace.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
}

// ProvisionedNodeClaimStatus reports fulfillment progress.
type ProvisionedNodeClaimStatus struct {
	// Phase mirrors the underlying CAPI Machine's phase (Pending,
	// Provisioning, Provisioned, Running, Deleting, Failed), or
	// "Resolving" before the Machine exists.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Provider is the infrastructure kind that fulfilled the claim
	// (e.g. AWSMachine).
	// +optional
	Provider string `json:"provider,omitempty"`
	// InstanceType is the resolved instance type.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`
	// MachineName is the CAPI Machine created for this claim.
	// +optional
	MachineName string `json:"machineName,omitempty"`
	// ExternalIP is the node's public address, once known.
	// +optional
	ExternalIP string `json:"externalIP,omitempty"`
	// WireGuardAddress is the node's allocated tunnel address.
	// +optional
	WireGuardAddress string `json:"wireGuardAddress,omitempty"`
	// TunnelInterface is the mesh's interface name on every member.
	// +optional
	TunnelInterface string `json:"tunnelInterface,omitempty"`
	// Message carries the latest human-readable fulfillment error.
	// +optional
	Message string `json:"message,omitempty"`
}

// ProvisionedNodeClaim is the single user-facing resource.
type ProvisionedNodeClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisionedNodeClaimSpec   `json:"spec"`
	Status ProvisionedNodeClaimStatus `json:"status,omitempty"`
}

// ProvisionedNodeClaimList is the list type.
type ProvisionedNodeClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedNodeClaim `json:"items"`
}

// DeepCopyInto/DeepCopy/DeepCopyObject: hand-written (the module has
// no code-generation toolchain, and these types are small enough that
// generated boilerplate would be the only reason to add one).

func (in *ProvisionedNodeClaimSpec) DeepCopyInto(out *ProvisionedNodeClaimSpec) {
	*out = *in
	if in.Requests != nil {
		out.Requests = make(corev1.ResourceList, len(in.Requests))
		for k, v := range in.Requests {
			out.Requests[k] = v.DeepCopy()
		}
	}
	if in.InternetFacing != nil {
		v := *in.InternetFacing
		out.InternetFacing = &v
	}
	if in.TunnelEndpoints != nil {
		out.TunnelEndpoints = in.TunnelEndpoints.DeepCopy()
	}
}

func (in *ProvisionedNodeClaim) DeepCopyInto(out *ProvisionedNodeClaim) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *ProvisionedNodeClaim) DeepCopy() *ProvisionedNodeClaim {
	if in == nil {
		return nil
	}
	out := new(ProvisionedNodeClaim)
	in.DeepCopyInto(out)
	return out
}

func (in *ProvisionedNodeClaim) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *ProvisionedNodeClaimList) DeepCopyInto(out *ProvisionedNodeClaimList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ProvisionedNodeClaim, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ProvisionedNodeClaimList) DeepCopy() *ProvisionedNodeClaimList {
	if in == nil {
		return nil
	}
	out := new(ProvisionedNodeClaimList)
	in.DeepCopyInto(out)
	return out
}

func (in *ProvisionedNodeClaimList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

// AddToScheme registers the types.
func AddToScheme(s *runtime.Scheme) error {
	builder := runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ProvisionedNodeClaim{}, &ProvisionedNodeClaimList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
	return builder.AddToScheme(s)
}
