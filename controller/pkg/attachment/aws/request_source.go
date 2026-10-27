package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// calicoDefaultVXLANPort is Felix's VXLAN port when its configuration names
// none.
const calicoDefaultVXLANPort = 4789

var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

// RequestSource derives a worker's gateway request from what the cluster,
// the mesh and EC2 already state, so an operator names only the gateway.
type RequestSource struct {
	Resolver
	// MeshName is the peer Secret in the resolver's namespace.
	MeshName string
}

// Request derives the gateway request for the worker and gateway Machines,
// named in the resolver's namespace. Anything it cannot determine is an
// error, never a default.
func (s RequestSource) Request(ctx context.Context, worker, gateway string) (attachment.GatewayRequest, error) {
	var r attachment.GatewayRequest
	var err error
	if r.Worker, err = s.observe(ctx, worker); err != nil {
		return r, fmt.Errorf("worker: %w", err)
	}
	if r.Gateway, err = s.observe(ctx, gateway); err != nil {
		return r, fmt.Errorf("gateway: %w", err)
	}
	if r.SiteTransport, err = s.siteTransport(ctx); err != nil {
		return r, err
	}
	mesh := &corev1.Secret{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.MeshName}, mesh); err != nil {
		return r, fmt.Errorf("reading the mesh: %w", err)
	}
	if r.SiteReturnSources, err = siteReturnSources(mesh.Data); err != nil {
		return r, err
	}
	if r.Underlay, err = s.underlay(ctx, mesh.Data); err != nil {
		return r, err
	}
	if r.UDPPort, err = s.vxlanPort(ctx); err != nil {
		return r, err
	}
	if r.TCPPorts, err = s.konnectivityPorts(ctx); err != nil {
		return r, err
	}
	if _, err := attachment.PlanGateway(r); err != nil {
		return r, err
	}
	return r, nil
}

// observe binds a Machine to its Node and its instance's one interface,
// then checks the binding the same way resolution does.
func (s RequestSource) observe(ctx context.Context, name string) (attachment.Machine, error) {
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, machine); err != nil {
		return attachment.Machine{}, fmt.Errorf("Machine %s: %w", name, err)
	}
	provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	cluster, _, _ := unstructured.NestedString(machine.Object, "spec", "clusterName")
	nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
	if machine.GetDeletionTimestamp() != nil || cluster != s.ClusterName || provider == "" || nodeName == "" {
		return attachment.Machine{}, fmt.Errorf("Machine %s is not a running node of cluster %s", name, s.ClusterName)
	}
	node := &corev1.Node{}
	if err := s.Client.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return attachment.Machine{}, fmt.Errorf("Node %s: %w", nodeName, err)
	}
	if node.Spec.ProviderID != provider {
		return attachment.Machine{}, fmt.Errorf("Node %s is not Machine %s's instance", nodeName, name)
	}
	instance := provider[strings.LastIndex(provider, "/")+1:]
	raw, err := s.API.Call(ctx, "ec2", "describe-network-interfaces", map[string]any{"Filters": []map[string]any{{"Name": "attachment.instance-id", "Values": []string{instance}}}})
	if err != nil {
		return attachment.Machine{}, err
	}
	var interfaces struct {
		NetworkInterfaces []struct{ NetworkInterfaceId, SubnetId, PrivateIpAddress string }
	}
	if err := json.Unmarshal(raw, &interfaces); err != nil {
		return attachment.Machine{}, err
	}
	if len(interfaces.NetworkInterfaces) != 1 {
		return attachment.Machine{}, fmt.Errorf("instance %s has %d network interfaces; the gateway capability needs exactly one", instance, len(interfaces.NetworkInterfaces))
	}
	nic := interfaces.NetworkInterfaces[0]
	raw, err = s.API.Call(ctx, "ec2", "describe-subnets", map[string]any{"SubnetIds": []string{nic.SubnetId}})
	if err != nil {
		return attachment.Machine{}, err
	}
	var subnets struct{ Subnets []struct{ CidrBlock string } }
	if err := json.Unmarshal(raw, &subnets); err != nil {
		return attachment.Machine{}, err
	}
	if len(subnets.Subnets) != 1 {
		return attachment.Machine{}, fmt.Errorf("subnet %s not observed", nic.SubnetId)
	}
	subnet, err := netip.ParsePrefix(subnets.Subnets[0].CidrBlock)
	if err != nil {
		return attachment.Machine{}, err
	}
	address, err := netip.ParseAddr(nic.PrivateIpAddress)
	if err != nil {
		return attachment.Machine{}, err
	}
	m := attachment.Machine{UID: string(machine.GetUID()), NodeUID: string(node.UID), ProviderID: provider, InterfaceID: nic.NetworkInterfaceId,
		NetworkID: "aws:" + s.Scope.Account + ":" + s.Scope.Region + ":" + s.Scope.VPCID, Subnet: subnet, Address: address}
	if _, err := s.machine(ctx, m); err != nil {
		return attachment.Machine{}, err
	}
	return m, nil
}

// siteTransport is where Calico sends a worker's VXLAN packets: the address
// Calico holds for each site node. A site node Calico holds none for runs no
// Calico, and so receives none.
func (s RequestSource) siteTransport(ctx context.Context) ([]netip.Addr, error) {
	nodes := &corev1.NodeList{}
	if err := s.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, node := range nodes.Items {
		raw := node.Annotations[attachment.CalicoIPv4Address]
		if _, cloud := node.Labels[platform.RoleLabel]; cloud || raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("Node %s: Calico address %q: %w", node.Name, raw, err)
		}
		out = append(out, prefix.Addr())
	}
	return sorted(out), nil
}

// siteReturnSources are the site tunnel endpoints' mesh addresses, which
// source the site's traffic into the mesh.
func siteReturnSources(data map[string][]byte) ([]netip.Addr, error) {
	var out []netip.Addr
	for key, value := range data {
		if !strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(string(value)))
		if err != nil {
			return nil, fmt.Errorf("mesh %s: %w", key, err)
		}
		out = append(out, prefix.Addr())
	}
	return sorted(out), nil
}

// underlay is every outer tunnel endpoint: the endpoints the mesh publishes
// and the external addresses of the Machines, which are the endpoints a
// joining Machine will publish.
func (s RequestSource) underlay(ctx context.Context, data map[string][]byte) ([]netip.Addr, error) {
	var out []netip.Addr
	for key, value := range data {
		endpoint := strings.TrimSpace(string(value))
		if !strings.HasPrefix(key, tunnel.PeerEndpointPrefix) || endpoint == tunnel.PeerEndpointPending || endpoint == "" {
			continue
		}
		address, err := netip.ParseAddrPort(endpoint)
		if err != nil {
			return nil, fmt.Errorf("mesh %s: %w", key, err)
		}
		out = append(out, address.Addr())
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := s.Client.List(ctx, machines, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	for _, machine := range machines.Items {
		addresses, _, _ := unstructured.NestedSlice(machine.Object, "status", "addresses")
		for _, a := range addresses {
			entry, _ := a.(map[string]any)
			if entry["type"] != "ExternalIP" {
				continue
			}
			address, err := netip.ParseAddr(fmt.Sprint(entry["address"]))
			if err != nil {
				return nil, fmt.Errorf("Machine %s external address: %w", machine.GetName(), err)
			}
			if address.Is4() {
				out = append(out, address)
			}
		}
	}
	return sorted(out), nil
}

// vxlanPort is Felix's VXLAN port. Calico creates its default
// configuration when it starts, so its absence means the site runs no
// Calico this capability can carry.
func (s RequestSource) vxlanPort(ctx context.Context) (uint16, error) {
	felix := &unstructured.Unstructured{}
	felix.SetGroupVersionKind(schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "FelixConfiguration"})
	if err := s.Client.Get(ctx, client.ObjectKey{Name: "default"}, felix); err != nil {
		return 0, fmt.Errorf("reading Calico's default FelixConfiguration: %w", err)
	}
	port, found, err := unstructured.NestedInt64(felix.Object, "spec", "vxlanPort")
	if err != nil {
		return 0, fmt.Errorf("Calico's VXLAN port: %w", err)
	}
	if !found {
		return calicoDefaultVXLANPort, nil
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("Calico's VXLAN port %d is out of range", port)
	}
	return uint16(port), nil
}

// konnectivityPorts is the Konnectivity server port its agents dial (k0s),
// which a worker behind the gateway must reach; none without Konnectivity.
func (s RequestSource) konnectivityPorts(ctx context.Context) ([]uint16, error) {
	agent := &appsv1.DaemonSet{}
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "konnectivity-agent"}, agent)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, container := range agent.Spec.Template.Spec.Containers {
		for _, arg := range slices.Concat(container.Command, container.Args) {
			if value, ok := strings.CutPrefix(arg, "--proxy-server-port="); ok {
				port, err := strconv.ParseUint(value, 10, 16)
				if err != nil || port == 0 {
					return nil, fmt.Errorf("konnectivity-agent proxy server port %q", value)
				}
				return []uint16{uint16(port)}, nil
			}
		}
	}
	return nil, fmt.Errorf("konnectivity-agent names no --proxy-server-port, so the port its workers dial cannot be determined")
}

func sorted(addresses []netip.Addr) []netip.Addr {
	slices.SortFunc(addresses, netip.Addr.Compare)
	return slices.Compact(addresses)
}
