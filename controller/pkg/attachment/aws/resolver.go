package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolver binds an attachment to live CAPI, Node and single-NIC EC2 identities.
// Namespace, CAPI cluster and shared security group are explicit deployment scope.
type Resolver struct {
	Client                          client.Client
	API                             API
	Scope                           Scope
	Namespace, ClusterName, GroupID string
	// APIPort permits native worker TCP traffic through the gateway ENI.
	APIPort uint16
}

func (r Resolver) machine(ctx context.Context, m attachment.Machine) (InterfaceTarget, error) {
	if r.Client == nil || r.API == nil || r.Namespace == "" || r.ClusterName == "" || r.GroupID == "" {
		return InterfaceTarget{}, fmt.Errorf("resolver clients and deployment scope required")
	}
	network := "aws:" + r.Scope.Account + ":" + r.Scope.Region + ":" + r.Scope.VPCID
	if m.UID == "" || m.NodeUID == "" || m.InterfaceID == "" || m.NetworkID != network || !m.Address.Is4() || !m.Subnet.Contains(m.Address) {
		return InterfaceTarget{}, fmt.Errorf("incomplete or foreign planned machine")
	}
	parts := strings.Split(m.ProviderID, "/")
	if len(parts) != 5 || parts[0] != "aws:" || parts[1] != "" || parts[2] != "" || !strings.HasPrefix(parts[3], r.Scope.Region) || !strings.HasPrefix(parts[4], "i-") {
		return InterfaceTarget{}, fmt.Errorf("invalid EC2 provider identity")
	}
	target := InterfaceTarget{InterfaceID: m.InterfaceID, InstanceID: parts[4]}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta1", Kind: "MachineList"})
	if err := r.Client.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return target, err
	}
	found := false
	for _, machine := range list.Items {
		if string(machine.GetUID()) != m.UID {
			continue
		}
		provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		cluster, _, _ := unstructured.NestedString(machine.Object, "spec", "clusterName")
		nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		if found || (machine.GetDeletionTimestamp() != nil && !attachment.HasDeletionHold(&machine)) || provider != m.ProviderID || cluster != r.ClusterName || nodeName == "" {
			return target, fmt.Errorf("CAPI machine binding changed")
		}
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			return target, err
		}
		if string(node.UID) != m.NodeUID || node.Spec.ProviderID != m.ProviderID || node.DeletionTimestamp != nil {
			return target, fmt.Errorf("Node binding changed")
		}
		found = true
	}
	if !found {
		return target, fmt.Errorf("planned CAPI machine absent")
	}
	raw, err := r.API.Call(ctx, "ec2", "describe-network-interfaces", map[string]any{"Filters": []map[string]any{{"Name": "attachment.instance-id", "Values": []string{target.InstanceID}}}})
	if err != nil {
		return target, err
	}
	var response struct {
		NetworkInterfaces []struct {
			NetworkInterfaceId, OwnerId, VpcId, SubnetId, PrivateIpAddress, Status string
			Attachment                                                             struct {
				InstanceId, Status string
				DeviceIndex        int
			}
			Groups []struct{ GroupId string }
			TagSet []struct{ Key, Value string }
		}
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return target, err
	}
	if len(response.NetworkInterfaces) != 1 {
		return target, fmt.Errorf("single physical NIC required")
	}
	i := response.NetworkInterfaces[0]
	group, owned := false, false
	for _, g := range i.Groups {
		if g.GroupId == r.GroupID {
			group = true
		}
	}
	for _, tag := range i.TagSet {
		if tag.Key == r.Scope.OwnerTag && tag.Value == r.Scope.OwnerValue {
			owned = true
		}
	}
	if !owned || !group || i.NetworkInterfaceId != m.InterfaceID || i.OwnerId != r.Scope.Account || i.VpcId != r.Scope.VPCID || i.SubnetId != r.Scope.SubnetID || i.PrivateIpAddress != m.Address.String() || i.Status != "in-use" || i.Attachment.InstanceId != target.InstanceID || i.Attachment.Status != "attached" || i.Attachment.DeviceIndex != 0 {
		return target, fmt.Errorf("EC2 interface binding changed")
	}
	return target, nil
}
func (r Resolver) Resolve(ctx context.Context, record attachment.Record) (ForwardingBinding, error) {
	b := ForwardingBinding{Lease: record.LeaseID(), Scope: r.Scope}
	if record.ID == "" || record.Lease == "" || record.Digest == "" || record.Plan.UDPPort == 0 || len(record.Plan.ReturnHosts) == 0 {
		return b, fmt.Errorf("complete attachment intent required")
	}
	if _, err := r.machine(ctx, record.Plan.Worker); err != nil {
		return b, fmt.Errorf("worker: %w", err)
	}
	gateway, err := r.machine(ctx, record.Plan.Gateway)
	if err != nil {
		return b, fmt.Errorf("gateway: %w", err)
	}
	b.Gateway = gateway
	for _, host := range record.Plan.ReturnHosts {
		if !host.IsValid() || !host.Addr().Is4() || host.Bits() != 32 || record.Plan.Worker.Subnet.Contains(host.Addr()) {
			return b, fmt.Errorf("invalid return host")
		}
		b.Routes = append(b.Routes, RouteTarget{Destination: host, InterfaceID: gateway.InterfaceID, InstanceID: gateway.InstanceID})
		b.Ingress = append(b.Ingress, IngressTarget{GroupID: r.GroupID, Source: host, Port: record.Plan.UDPPort})
	}
	b.Ingress = append(b.Ingress, IngressTarget{GroupID: r.GroupID, Source: record.Plan.WorkerHost, Port: record.Plan.UDPPort})
	ports := map[uint16]bool{}
	if r.APIPort != 0 {
		ports[r.APIPort] = true
	}
	for _, port := range record.Plan.TCPPorts {
		if port == 0 {
			return b, fmt.Errorf("invalid control-plane TCP port")
		}
		ports[port] = true
	}
	ordered := make([]int, 0, len(ports))
	for port := range ports {
		ordered = append(ordered, int(port))
	}
	sort.Ints(ordered)
	for _, port := range ordered {
		b.Ingress = append(b.Ingress, IngressTarget{GroupID: r.GroupID, Source: record.Plan.WorkerHost, Port: uint16(port), Protocol: "tcp"})
	}
	return b, nil
}
