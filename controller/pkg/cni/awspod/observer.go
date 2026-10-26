// Package awspod observes EC2 allocations. It never assigns addresses or changes
// routing. Only a fresh Machine -> Node -> instance -> ENI binding is accepted.
package awspod

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"net/netip"
	"sort"
	"strings"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const NodeLabel = platform.CNILabel
const NodeValue = "aws-vpc"

type Config struct {
	Account    string   `json:"account"`
	Region     string   `json:"region"`
	VPC        string   `json:"vpc"`
	Subnets    []string `json:"subnets"`
	Namespace  string   `json:"namespace"`
	Cluster    string   `json:"cluster"`
	OwnerTag   string   `json:"ownerTag"`
	OwnerValue string   `json:"ownerValue"`
	Family     string   `json:"family"`
	PodRanges  []string `json:"podRanges"`
}

func (c Config) Validate() error {
	if c.Account == "" || c.Region == "" || c.VPC == "" || c.Namespace == "" || c.Cluster == "" || c.OwnerTag == "" || c.OwnerValue == "" || len(c.Subnets) == 0 || len(c.PodRanges) == 0 {
		return fmt.Errorf("complete AWS allocation scope required")
	}
	if c.Family != "ipv4" && c.Family != "ipv6" {
		return fmt.Errorf("family must be ipv4 or ipv6; dual-stack AWS pods are not supported")
	}
	for _, subnet := range c.Subnets {
		if subnet == "" {
			return fmt.Errorf("empty subnet")
		}
	}
	for _, raw := range c.PodRanges {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p != p.Masked() || p.Bits() == 0 || p.Addr().Is4In6() || p.Addr().Is4() != (c.Family == "ipv4") {
			return fmt.Errorf("invalid %s pod range %q", c.Family, raw)
		}
	}
	return nil
}

type Observer struct {
	Config Config
	Reader client.Reader
	API    attachmentaws.API
}

// Allocation keeps native VPC destinations separate from pod ownership.
// Primary ENI addresses are native destinations but are never pod allocations.
type Allocation struct {
	PodPrefixes    []netip.Prefix
	NativePrefixes []netip.Prefix
}

func (o *Observer) PrefixesFor(ctx context.Context, expected *unstructured.Unstructured, nodeName string) ([]netip.Prefix, error) {
	allocation, err := o.AllocationsFor(ctx, expected, nodeName)
	if err != nil {
		return nil, err
	}
	return allocation.PodPrefixes, nil
}

func (o *Observer) AllocationsFor(ctx context.Context, expected *unstructured.Unstructured, nodeName string) (*Allocation, error) {
	if err := o.Config.Validate(); err != nil {
		return nil, err
	}
	if o.Reader == nil || o.API == nil || expected == nil || expected.GetUID() == "" {
		return nil, fmt.Errorf("allocation observer requires clients and immutable Machine identity")
	}
	m, node, err := o.binding(ctx, expected, nodeName)
	if err != nil {
		return nil, err
	}
	provider, _, _ := unstructured.NestedString(m.Object, "spec", "providerID")
	parts := strings.Split(provider, "/")
	if len(parts) != 5 || parts[0] != "aws:" || parts[1] != "" || parts[2] != "" || len(parts[3]) != len(o.Config.Region)+1 || !strings.HasPrefix(parts[3], o.Config.Region) || !strings.HasPrefix(parts[4], "i-") || len(parts[4]) <= 2 {
		return nil, fmt.Errorf("invalid EC2 provider identity")
	}
	instance := parts[4]
	var prefixes, nativeHosts []netip.Prefix
	seenENI := map[string]bool{}
	tokens := map[string]bool{}
	token := ""
	for {
		input := map[string]any{"Filters": []map[string]any{{"Name": "attachment.instance-id", "Values": []string{instance}}}}
		if token != "" {
			input["NextToken"] = token
		}
		raw, err := o.API.Call(ctx, "ec2", "describe-network-interfaces", input)
		if err != nil {
			return nil, err
		}
		var response struct {
			NextToken         string
			NetworkInterfaces []struct {
				NetworkInterfaceId, OwnerId, VpcId, SubnetId, Status string
				Attachment                                           struct {
					InstanceId, Status string
					DeviceIndex        *int
				}
				TagSet             []struct{ Key, Value string }
				PrivateIpAddresses []struct {
					PrivateIpAddress string
					Primary          bool
				}
				Ipv6Addresses []struct{ Ipv6Address string }
				Ipv4Prefixes  []struct{ Ipv4Prefix string }
				Ipv6Prefixes  []struct{ Ipv6Prefix string }
			}
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, err
		}
		for _, eni := range response.NetworkInterfaces {
			owned, subnet := false, false
			for _, tag := range eni.TagSet {
				if tag.Key == o.Config.OwnerTag && tag.Value == o.Config.OwnerValue {
					owned = true
				}
			}
			for _, id := range o.Config.Subnets {
				if id == eni.SubnetId {
					subnet = true
				}
			}
			if eni.NetworkInterfaceId == "" || seenENI[eni.NetworkInterfaceId] || !owned || !subnet || eni.OwnerId != o.Config.Account || eni.VpcId != o.Config.VPC || eni.Status != "in-use" || eni.Attachment.InstanceId != instance || eni.Attachment.Status != "attached" {
				return nil, fmt.Errorf("foreign, duplicate or detached EC2 interface")
			}
			if node.Labels[corev1.LabelOSStable] == "windows" && (eni.Attachment.DeviceIndex == nil || *eni.Attachment.DeviceIndex != 0 || len(seenENI) != 0) {
				return nil, fmt.Errorf("Windows secondary IP allocation requires one primary ENI")
			}
			seenENI[eni.NetworkInterfaceId] = true
			for _, ip := range eni.PrivateIpAddresses {
				if !ip.Primary {
					continue
				}
				address, err := netip.ParseAddr(ip.PrivateIpAddress)
				if err != nil || !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
					return nil, fmt.Errorf("invalid ENI primary address")
				}
				nativeHosts = append(nativeHosts, netip.PrefixFrom(address, 32))
			}
			// ENI IPv6 host addresses are native return destinations, including
			// for Service traffic masqueraded by kube-proxy. They are not
			// delegated pod allocations, regardless of the configured pod family.
			for _, ip := range eni.Ipv6Addresses {
				address, err := netip.ParseAddr(ip.Ipv6Address)
				if err != nil || !address.Is6() || address.Is4In6() || !address.IsGlobalUnicast() {
					return nil, fmt.Errorf("invalid ENI IPv6 host address")
				}
				nativeHosts = append(nativeHosts, netip.PrefixFrom(address, 128))
			}

			if o.Config.Family == "ipv4" {
				if len(eni.Ipv4Prefixes) > 0 {
					return nil, fmt.Errorf("IPv4 qualification requires secondary IP mode, not prefix delegation")
				}
				for _, ip := range eni.PrivateIpAddresses {
					if ip.Primary {
						continue
					}
					addr, err := netip.ParseAddr(ip.PrivateIpAddress)
					if err != nil || !addr.Is4() {
						return nil, fmt.Errorf("invalid EC2 secondary IPv4 address")
					}
					prefixes = append(prefixes, netip.PrefixFrom(addr, 32))
				}
			} else {
				for _, delegated := range eni.Ipv6Prefixes {
					p, err := netip.ParsePrefix(delegated.Ipv6Prefix)
					if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() != 80 || p != p.Masked() {
						return nil, fmt.Errorf("invalid EC2 delegated IPv6 /80")
					}
					prefixes = append(prefixes, p)
				}
			}
		}
		token = response.NextToken
		if token == "" {
			break
		}
		if tokens[token] || len(tokens) >= 100 {
			return nil, fmt.Errorf("EC2 pagination did not converge")
		}
		tokens[token] = true
	}
	if len(seenENI) == 0 {
		return nil, fmt.Errorf("no bound EC2 interfaces")
	}
	for i, p := range prefixes {
		scoped := false
		for _, raw := range o.Config.PodRanges {
			allowed, _ := netip.ParsePrefix(raw)
			if allowed.Bits() <= p.Bits() && allowed.Contains(p.Addr()) {
				scoped = true
			}
		}
		if !scoped {
			return nil, fmt.Errorf("EC2 pod allocation outside configured ranges")
		}
		for _, other := range prefixes[:i] {
			if p.Overlaps(other) {
				return nil, fmt.Errorf("overlapping EC2 pod allocations")
			}
		}
	}
	// The EC2 read can span multiple requests. Never publish allocations for
	// a Node or Machine replaced while those requests were in flight.
	latestMachine, latestNode, err := o.binding(ctx, expected, nodeName)
	if err != nil {
		return nil, err
	}
	latestProvider, _, _ := unstructured.NestedString(latestMachine.Object, "spec", "providerID")
	if latestNode.UID != node.UID || latestProvider != provider {
		return nil, fmt.Errorf("allocation binding changed during EC2 observation")
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].String() < prefixes[j].String() })
	// A complete, successful empty observation is authoritative withdrawal.
	return &Allocation{PodPrefixes: prefixes, NativePrefixes: append(nativeHosts, prefixes...)}, nil
}

func (o *Observer) binding(ctx context.Context, expected *unstructured.Unstructured, nodeName string) (*unstructured.Unstructured, *corev1.Node, error) {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(expected.GroupVersionKind())
	if err := o.Reader.Get(ctx, client.ObjectKeyFromObject(expected), m); err != nil {
		return nil, nil, err
	}
	cluster, _, _ := unstructured.NestedString(m.Object, "spec", "clusterName")
	provider, _, _ := unstructured.NestedString(m.Object, "spec", "providerID")
	boundName, _, _ := unstructured.NestedString(m.Object, "status", "nodeRef", "name")
	boundUID, _, _ := unstructured.NestedString(m.Object, "status", "nodeRef", "uid")
	if m.GetUID() != expected.GetUID() || m.GetDeletionTimestamp() != nil || m.GetNamespace() != o.Config.Namespace || cluster != o.Config.Cluster || boundName != nodeName {
		return nil, nil, fmt.Errorf("Machine allocation binding absent or changed")
	}
	node := &corev1.Node{}
	if err := o.Reader.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, nil, err
	}
	if node.UID == "" || (boundUID != "" && string(node.UID) != boundUID) || node.DeletionTimestamp != nil || node.Spec.ProviderID != provider || node.Labels[NodeLabel] != NodeValue || node.Labels["cloud-provisioning.appmana.com/role"] != "cloud-worker" {
		return nil, nil, fmt.Errorf("Node allocation binding absent or changed")
	}
	if err := ValidateNodeCNI(node); err != nil {
		return nil, nil, err
	}
	switch node.Labels[corev1.LabelOSStable] {
	case "linux":
	case "windows":
		if o.Config.Family != "ipv4" {
			return nil, nil, fmt.Errorf("Windows allocation observation requires IPv4 secondary IP mode")
		}
	default:
		return nil, nil, fmt.Errorf("unsupported Node operating system for AWS allocations")
	}
	if boundUID == "" {
		// CAPI v1beta2's MachineNodeReference contains only a name. Bind the
		// live Node UID through CAPI's reverse association and provider ID.
		phase, _, _ := unstructured.NestedString(m.Object, "status", "phase")
		if m.GroupVersionKind().Version != "v1beta2" || phase != "Running" ||
			node.Annotations["cluster.x-k8s.io/machine"] != m.GetName() ||
			node.Annotations["cluster.x-k8s.io/cluster-name"] != cluster ||
			node.Annotations["cluster.x-k8s.io/cluster-namespace"] != m.GetNamespace() {
			return nil, nil, fmt.Errorf("CAPI Node reverse association absent or changed")
		}
	}
	return m, node, nil
}
