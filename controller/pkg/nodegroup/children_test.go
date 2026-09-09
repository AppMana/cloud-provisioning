package nodegroup

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func groupFixture() *v1alpha1.ProvisionedNodeGroupClaim {
	apiGroup := "infrastructure.cluster.x-k8s.io"
	return &v1alpha1.ProvisionedNodeGroupClaim{ObjectMeta: metav1.ObjectMeta{Name: "render-workers", Namespace: "capacity", UID: "group-uid"}, Spec: v1alpha1.ProvisionedNodeGroupClaimSpec{Template: v1alpha1.ProvisionedNodeClaimTemplate{Spec: v1alpha1.ProvisionedNodeClaimSpec{InfrastructureRef: corev1.TypedLocalObjectReference{APIGroup: &apiGroup, Kind: "AWSMachineTemplate", Name: "gpu"}}}}}
}
func TestStableChildIdentityAndTemplateIsolation(t *testing.T) {
	group := groupFixture()
	child, e := BuildChild(group, 0)
	if e != nil {
		t.Fatal(e)
	}
	child.UID = "child-uid"
	observed, e := ObserveChild(group, child)
	if e != nil || observed.Ordinal != 0 || observed.Terminating {
		t.Fatal(observed, e)
	}
	again, e := BuildChild(group.DeepCopy(), 0)
	if e != nil || again.Name != child.Name {
		t.Fatal("restart changed identity", e)
	}
	*child.Spec.InfrastructureRef.APIGroup = "changed"
	if *group.Spec.Template.Spec.InfrastructureRef.APIGroup != "infrastructure.cluster.x-k8s.io" {
		t.Fatal("child aliases template")
	}
	group.Spec.Template.Spec.InfrastructureRef.Name = "next-image"
	if _, e := ObserveChild(group, child); e != nil {
		t.Fatal("template update orphaned existing child", e)
	}
	replacement := group.DeepCopy()
	replacement.UID = "replacement-uid"
	newChild, e := BuildChild(replacement, 0)
	if e != nil || newChild.Name == child.Name {
		t.Fatal("recreated group reused old name", e)
	}
	if _, e := ObserveChild(replacement, child); e == nil {
		t.Fatal("adopted old incarnation")
	}
	now := metav1.Now()
	child.DeletionTimestamp = &now
	observed, e = ObserveChild(group, child)
	if e != nil || !observed.Terminating {
		t.Fatal("terminating slot lost", e)
	}
	group.DeletionTimestamp = &now
	if _, e := BuildChild(group, 1); e == nil {
		t.Fatal("created during deletion")
	}
}
func TestChildRejectsIdentitySpoofing(t *testing.T) {
	group := groupFixture()
	original, e := BuildChild(group, 1)
	if e != nil {
		t.Fatal(e)
	}
	original.UID = "child-uid"
	for _, change := range []func(*v1alpha1.ProvisionedNodeClaim){
		func(c *v1alpha1.ProvisionedNodeClaim) { c.OwnerReferences = nil },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.OwnerReferences[0].UID = "other" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.OwnerReferences[0].Kind = "Other" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Namespace = "other" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Name = "other" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Labels[GroupUIDLabel] = "other" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Annotations[OrdinalAnnotation] = "01" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Annotations[OrdinalAnnotation] = "-1" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.Annotations[OrdinalAnnotation] = "oops" },
		func(c *v1alpha1.ProvisionedNodeClaim) { c.UID = "" },
	} {
		child := original.DeepCopy()
		change(child)
		if _, e := ObserveChild(group, child); e == nil {
			t.Fatal("accepted malformed ownership")
		}
	}
	if _, e := BuildChild(nil, 0); e == nil {
		t.Fatal("nil group")
	}
	if _, e := ObserveChild(nil, original); e == nil {
		t.Fatal("nil owner")
	}
	group.Name = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)
	child, e := BuildChild(group, 2147483647)
	if e != nil || len(validation.IsDNS1123Label(child.Name)) != 0 {
		t.Fatal("invalid bounded machine name", e)
	}
}
