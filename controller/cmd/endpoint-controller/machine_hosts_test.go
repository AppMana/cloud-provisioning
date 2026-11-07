package main

import (
	"reflect"
	"testing"
)

// A cloud worker can have a public tunnel endpoint and a private node
// address used for kubelet access. Both provider-reported addresses must
// be accepted and routed through the worker's tunnel.
func TestAMachinesNodeAddressesJoinItsPeerEntry(t *testing.T) {
	addresses := []any{
		map[string]any{"type": "InternalDNS", "address": "ip-172-29-0-93.us-west-2.compute.internal"},
		map[string]any{"type": "InternalIP", "address": "172.29.0.93"},
		map[string]any{"type": "ExternalIP", "address": "34.222.55.27"},
	}
	endpoint, hosts := machineHosts(addresses)
	if endpoint != "34.222.55.27" {
		t.Errorf("endpoint = %q, want the ExternalIP", endpoint)
	}
	if want := []string{"34.222.55.27", "172.29.0.93"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	// A provider reporting only internal addresses is dialled there, and
	// it is named once.
	endpoint, hosts = machineHosts(addresses[:2])
	if endpoint != "172.29.0.93" || !reflect.DeepEqual(hosts, []string{"172.29.0.93"}) {
		t.Errorf("internal only: endpoint %q hosts %v", endpoint, hosts)
	}

	// When an AWS worker runs with publicIP: false and assignPrimaryIPv6: enabled
	// (avoiding both NAT Gateway and public IPv4 charges), CAPA reports the
	// private IPv4 and global IPv6 GUA as InternalIPs with no ExternalIP.
	// machineHosts must select the globally routable IPv6 GUA as the tunnel endpoint.
	ipv6OnlyPublicAddresses := []any{
		map[string]any{"type": "InternalDNS", "address": "ip-172-29-0-93.us-west-2.compute.internal"},
		map[string]any{"type": "InternalIP", "address": "172.29.0.93"},
		map[string]any{"type": "InternalIP", "address": "2600:1f14:abcd:ef00::93"},
	}
	endpoint, hosts = machineHosts(ipv6OnlyPublicAddresses)
	if endpoint != "2600:1f14:abcd:ef00::93" {
		t.Errorf("ipv6 endpoint = %q, want 2600:1f14:abcd:ef00::93", endpoint)
	}
	if want := []string{"2600:1f14:abcd:ef00::93", "172.29.0.93"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("ipv6 hosts = %v, want %v", hosts, want)
	}
}
