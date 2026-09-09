package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

func TestGroupSchemeAndDeepCopy(t *testing.T) {
	replicas := int32(3)
	public := true
	apiGroup := "infrastructure.cluster.x-k8s.io"
	original := &ProvisionedNodeGroupClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "workers", Labels: map[string]string{"team": "render"}},
		Spec:       ProvisionedNodeGroupClaimSpec{Replicas: &replicas, WorkloadSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "render"}}, Template: ProvisionedNodeClaimTemplate{Spec: ProvisionedNodeClaimSpec{InternetFacing: &public, InfrastructureRef: corev1.TypedLocalObjectReference{APIGroup: &apiGroup, Kind: "AWSMachineTemplate", Name: "gpu"}, TunnelEndpoints: &metav1.LabelSelector{MatchLabels: map[string]string{"endpoint": "yes"}}}}},
		Status:     ProvisionedNodeGroupClaimStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}}},
	}
	original.Status.PendingAction = &NodeGroupAction{
		ID: "reserved", Type: "Create", Generation: 1,
		Template: &ProvisionedNodeClaimTemplate{Spec: ProvisionedNodeClaimSpec{
			InfrastructureRef: corev1.TypedLocalObjectReference{APIGroup: &apiGroup, Kind: "AWSMachineTemplate", Name: "frozen"},
		}},
	}
	copy := original.DeepCopyObject().(*ProvisionedNodeGroupClaim)
	*copy.Spec.Replicas = 1
	*copy.Spec.Template.Spec.InternetFacing = false
	*copy.Spec.Template.Spec.InfrastructureRef.APIGroup = "changed"
	copy.Labels["team"] = "other"
	copy.Spec.WorkloadSelector.MatchLabels["app"] = "other"
	copy.Spec.Template.Spec.TunnelEndpoints.MatchLabels["endpoint"] = "no"
	copy.Status.Conditions[0].Status = metav1.ConditionTrue
	if replicas != 3 || !public || apiGroup != "infrastructure.cluster.x-k8s.io" || original.Labels["team"] != "render" || original.Spec.WorkloadSelector.MatchLabels["app"] != "render" || original.Spec.Template.Spec.TunnelEndpoints.MatchLabels["endpoint"] != "yes" || original.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatal("copy mutated cached group")
	}
	if copy.Status.PendingAction.Template.Spec.InfrastructureRef.Name != "frozen" {
		t.Fatal("copy lost reserved template")
	}
	copy.Status.PendingAction.ID = "changed"
	copy.Status.PendingAction.Template.Spec.InfrastructureRef.Name = "changed"
	*copy.Status.PendingAction.Template.Spec.InfrastructureRef.APIGroup = "changed"
	if original.Status.PendingAction.ID != "reserved" || original.Status.PendingAction.Template.Spec.InfrastructureRef.Name != "frozen" || apiGroup != "infrastructure.cluster.x-k8s.io" {
		t.Fatal("copy mutated reserved action")
	}
	list := &ProvisionedNodeGroupClaimList{Items: []ProvisionedNodeGroupClaim{*original}}
	listCopy := list.DeepCopyObject().(*ProvisionedNodeGroupClaimList)
	*listCopy.Items[0].Spec.Replicas = 0
	if replicas != 3 {
		t.Fatal("list copy aliases group")
	}
	if (*ProvisionedNodeGroupClaim)(nil).DeepCopyObject() != nil || (*ProvisionedNodeGroupClaimList)(nil).DeepCopyObject() != nil {
		t.Fatal("nil runtime copy")
	}
	scheme := runtime.NewScheme()
	if e := AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"ProvisionedNodeClaim", "ProvisionedNodeGroupClaim", "ProvisionedNodeGroupClaimList"} {
		if _, e := scheme.New(GroupVersion.WithKind(kind)); e != nil {
			t.Fatal(e)
		}
	}
}
