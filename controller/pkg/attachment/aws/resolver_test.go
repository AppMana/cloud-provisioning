package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type resolverEC2 struct {
	secondNIC, foreignGroup bool
	calls                   int
}

func (e *resolverEC2) Call(_ context.Context, _, op string, in map[string]any) (json.RawMessage, error) {
	if op != "describe-network-interfaces" {
		return nil, fmt.Errorf("unexpected mutation %s", op)
	}
	e.calls++
	instance := in["Filters"].([]map[string]any)[0]["Values"].([]string)[0]
	ip := "172.29.0.21"
	if instance == "i-gateway" {
		ip = "172.29.0.9"
	}
	group := "sg"
	if e.foreignGroup {
		group = "other"
	}
	nic := map[string]any{"NetworkInterfaceId": "eni-" + instance, "OwnerId": "account", "VpcId": "vpc", "SubnetId": "subnet", "PrivateIpAddress": ip, "Status": "in-use", "Attachment": map[string]any{"InstanceId": instance, "Status": "attached", "DeviceIndex": 0}, "Groups": []any{map[string]any{"GroupId": group}}, "TagSet": []any{map[string]any{"Key": "run", "Value": "test"}}}
	nics := []any{nic}
	if e.secondNIC {
		nics = append(nics, nic)
	}
	return json.Marshal(map[string]any{"NetworkInterfaces": nics})
}
func resolverFixture(t *testing.T) (Resolver, attachment.Record, *resolverEC2) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	machine := func(name, ip string) attachment.Machine {
		provider := "aws:///us-west-2a/i-" + name
		obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": name, "namespace": "test", "uid": name}, "spec": map[string]any{"providerID": provider, "clusterName": "cluster"}, "status": map[string]any{"nodeRef": map[string]any{"name": name}}}}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("node-" + name)}, Spec: corev1.NodeSpec{ProviderID: provider}}
		objects = append(objects, obj, node)
		return attachment.Machine{UID: name, NodeUID: "node-" + name, ProviderID: provider, InterfaceID: "eni-i-" + name, NetworkID: "aws:account:us-west-2:vpc", Subnet: netip.MustParsePrefix("172.29.0.0/25"), Address: netip.MustParseAddr(ip)}
	}
	worker := machine("worker", "172.29.0.21")
	gateway := machine("gateway", "172.29.0.9")
	plan, err := attachment.PlanGateway(attachment.GatewayRequest{Worker: worker, Gateway: gateway, SiteTransport: []netip.Addr{netip.MustParseAddr("10.10.0.11")}, Underlay: []netip.Addr{netip.MustParseAddr("198.51.100.1")}, UDPPort: 4789})
	if err != nil {
		t.Fatal(err)
	}
	api := &resolverEC2{}
	return Resolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), API: api, Scope: Scope{Account: "account", Region: "us-west-2", VPCID: "vpc", SubnetID: "subnet", OwnerTag: "run", OwnerValue: "test"}, Namespace: "test", ClusterName: "cluster", GroupID: "sg"}, attachment.Record{ID: "attachment", Lease: "epoch", Digest: "digest", Plan: plan}, api
}

func TestResolverObservesDeletingMachineOnlyWhileLifetimeHeld(t *testing.T) {
	ctx := context.Background()
	r, record, _ := resolverFixture(t)
	m := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine"}}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "test", Name: "worker"}, m); err != nil {
		t.Fatal(err)
	}
	drain, terminate := attachment.DeletionHookKeys(record.LeaseID())
	m.SetAnnotations(map[string]string{drain: record.LeaseID(), terminate: record.LeaseID()})
	m.SetFinalizers([]string{"test/keep"})
	if err := r.Client.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Delete(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.machine(ctx, record.Plan.Worker); err != nil {
		t.Fatal("held Machine unavailable to withdrawal observer", err)
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	m.SetAnnotations(map[string]string{drain: record.LeaseID()})
	if err := r.Client.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.machine(ctx, record.Plan.Worker); err == nil {
		t.Fatal("unprotected deleting Machine accepted")
	}
}
func TestResolverBindsBothNativeTransportDirections(t *testing.T) {
	r, record, api := resolverFixture(t)
	b, err := r.Resolve(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if api.calls != 2 || b.Lease != record.LeaseID() || len(b.Routes) != 1 || len(b.Ingress) != 2 || b.Routes[0].InstanceID != "i-gateway" || b.Ingress[1].Source != record.Plan.WorkerHost {
		t.Fatalf("incomplete forwarding binding %#v", b)
	}
}
func TestResolverRejectsReplacedNodeAndWrongTopology(t *testing.T) {
	for _, kind := range []string{"node", "nic", "group", "network"} {
		t.Run(kind, func(t *testing.T) {
			r, record, api := resolverFixture(t)
			switch kind {
			case "node":
				record.Plan.Worker.NodeUID = "replacement"
			case "nic":
				api.secondNIC = true
			case "group":
				api.foreignGroup = true
			case "network":
				record.Plan.Worker.NetworkID = "foreign"
			}
			if _, err := r.Resolve(context.Background(), record); err == nil {
				t.Fatal("accepted changed identity or topology")
			}
		})
	}
}

func TestResolverIncludesNativeWorkerAPIIngress(t *testing.T) {
	r, record, _ := resolverFixture(t)
	r.APIPort = 6443
	b, err := r.Resolve(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	target := b.Ingress[len(b.Ingress)-1]
	if target.Protocol != "tcp" || target.Port != 6443 || target.Source != record.Plan.WorkerHost || target.GroupID != r.GroupID {
		t.Fatalf("API forwarding not bound to worker: %+v", target)
	}
}

func TestResolverIncludesObservedKonnectivityPortWithWorkerScope(t *testing.T) {
	// Native Windows TCP 8132 and kubectl exec recovered when this exact
	// worker /32 ingress was temporarily permitted on the gateway's group.
	r, record, _ := resolverFixture(t)
	r.APIPort = 6443
	record.Plan.TCPPorts = []uint16{8132, 6443, 8132}
	b, err := r.Resolve(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	var ports []uint16
	for _, target := range b.Ingress {
		if target.Protocol != "tcp" {
			continue
		}
		if target.Source != record.Plan.WorkerHost || target.GroupID != r.GroupID {
			t.Fatal("control-plane ingress escaped the worker /32 and owned group")
		}
		ports = append(ports, target.Port)
	}
	if len(ports) != 2 || ports[0] != 6443 || ports[1] != 8132 {
		t.Fatalf("missing or duplicated control-plane ingress: %v", ports)
	}
}
