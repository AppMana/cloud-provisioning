package nodegroup

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	attachmentruntime "github.com/appmana/cloud-provisioning/controller/pkg/attachment/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Controlled native acknowledgements; API objects, journals and both retirement
// state machines use their real implementations in this integration test.
type gatewayAcks struct{ withdraw bool }

func (*gatewayAcks) Ensure(context.Context, attachment.Record) (bool, error)  { return true, nil }
func (*gatewayAcks) Release(context.Context, attachment.Record) (bool, error) { return true, nil }
func (*gatewayAcks) Publish(context.Context, attachment.Record) (bool, error) { return true, nil }
func (a *gatewayAcks) Withdraw(context.Context, attachment.Record) (bool, error) {
	return a.withdraw, nil
}

func verifySerialGatewayRetirement(t *testing.T, api client.Client) {
	t.Helper()
	ctx := context.Background()
	group := groupFixture()
	group.Name = "serial-gateways"
	group.Namespace = "default"
	group.UID = ""
	zero := int32(0)
	group.Spec.Replicas = &zero
	if err := api.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	child, err := BuildChild(group, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	action, err := ProposeAction(group, []v1alpha1.ProvisionedNodeClaim{*child})
	if err != nil {
		t.Fatal(err)
	}
	group, err = ReserveAction(ctx, api, group, action)
	if err != nil {
		t.Fatal(err)
	}
	machine := func(uid, ip string) attachment.Machine {
		return attachment.Machine{UID: uid, NodeUID: "node-" + uid, ProviderID: "test:///" + uid, InterfaceID: "eni-" + uid, NetworkID: "network", Subnet: netip.MustParsePrefix("172.29.0.0/25"), Address: netip.MustParseAddr(ip)}
	}
	worker := machine("serial-worker", "172.29.0.21")
	a := group.Status.PendingAction
	a.NodeName = "serial-node"
	a.NodeUID = worker.NodeUID
	a.MachineUID = worker.UID
	a.ProviderID = worker.ProviderID
	if err := api.Status().Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	acks := &gatewayAcks{}
	lifecycle := attachment.Reconciler{Store: attachment.ConfigMapStore{Client: api, Namespace: group.Namespace}, Forwarder: acks, Publication: acks}
	controller := &attachmentruntime.RequestController{Client: api, Namespace: group.Namespace, MeshName: "serial-mesh", Lifecycle: lifecycle}
	requests := []*corev1.ConfigMap{}
	for _, name := range []string{"serial-a", "serial-b"} {
		desired := attachment.GatewayRequest{Worker: worker, Gateway: machine("gateway", "172.29.0.9"), SiteTransport: []netip.Addr{netip.MustParseAddr("10.10.0.11")}, Underlay: []netip.Addr{netip.MustParseAddr("198.51.100.1")}, UDPPort: 4789}
		raw, err := json.Marshal(desired)
		if err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Namespace, Labels: map[string]string{attachmentruntime.RequestLabel: "serial-mesh"}}, Data: map[string]string{"request.json": string(raw)}}
		if err := api.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if _, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}); err != nil {
				t.Fatal(err)
			}
		}
		requests = append(requests, cm)
	}
	captured, err := CaptureGatewayInventory(ctx, api, group, "serial-mesh")
	if err != nil {
		t.Fatal(err)
	}
	for i, cm := range requests {
		if done, err := RetireGatewayInventory(ctx, api, captured); err != nil || done {
			t.Fatal("premature inventory completion", done, err)
		}
		live := &corev1.ConfigMap{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(cm), live); err != nil || live.DeletionTimestamp.IsZero() {
			t.Fatal("request not retiring", err)
		}
		if i == 0 {
			other := &corev1.ConfigMap{}
			if err := api.Get(ctx, client.ObjectKeyFromObject(requests[1]), other); err != nil || !other.DeletionTimestamp.IsZero() {
				t.Fatal("parallel retirement", err)
			}
		}
		acks.withdraw = false
		for j := 0; j < 2; j++ {
			if _, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}); err != nil {
				t.Fatal(err)
			}
		}
		// Simulate controller restart while native consumers still withhold their ack.
		reloaded := &v1alpha1.ProvisionedNodeGroupClaim{}
		if err := api.Get(ctx, client.ObjectKeyFromObject(group), reloaded); err != nil {
			t.Fatal(err)
		}
		captured = reloaded
		if done, err := RetireGatewayInventory(ctx, api, captured); err != nil || done {
			t.Fatal("ignored missing acknowledgement", done, err)
		}
		acks.withdraw = true
		for j := 0; j < 3; j++ {
			if _, err := controller.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if done, err := RetireGatewayInventory(ctx, api, captured); err != nil || !done {
		t.Fatal("inventory did not complete", done, err)
	}
	if err := api.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil || !child.DeletionTimestamp.IsZero() {
		t.Fatal("gateway completion deleted worker claim", err)
	}
}
