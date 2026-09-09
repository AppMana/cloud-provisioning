package nodegroup

import (
	"context"
	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestActionFreezesTemplateAndDrainIdentity(t *testing.T) {
	group := groupFixture()
	group.ResourceVersion = "1"
	group.Generation = 1
	action, e := ProposeAction(group, nil)
	if e != nil || action == nil || action.Type != CreateAction || action.Ordinal != 0 {
		t.Fatal(action, e)
	}
	group.Spec.Template.Spec.InfrastructureRef.Name = "new-template"
	if action.Template.Spec.InfrastructureRef.Name != "gpu" {
		t.Fatal("pending creation follows mutable template")
	}
	child, e := BuildChild(group, 0)
	if e != nil {
		t.Fatal(e)
	}
	child.UID = "child-uid"
	zero := int32(0)
	group.Spec.Replicas = &zero
	action, e = ProposeAction(group, []v1alpha1.ProvisionedNodeClaim{*child})
	if e != nil || action.Type != DrainAction || action.ChildUID != "child-uid" || action.Template != nil {
		t.Fatal(action, e)
	}
	group.Status.PendingAction = action
	if _, e = ProposeAction(group, nil); e == nil {
		t.Fatal("discarded in-flight action")
	}
	group.Status.PendingAction = nil
	if action, e = ProposeAction(group, nil); e != nil || action != nil {
		t.Fatal("zero group creates capacity", action, e)
	}
	one := int32(1)
	group.Spec.Replicas = &one
	now := metav1.Now()
	group.DeletionTimestamp = &now
	action, e = ProposeAction(group, []v1alpha1.ProvisionedNodeClaim{*child})
	if e != nil || action.Type != DrainAction {
		t.Fatal("group deletion did not drain", action, e)
	}
	if _, e = ReserveAction(context.Background(), nil, group, action); e == nil {
		t.Fatal("nil store accepted")
	}
}
