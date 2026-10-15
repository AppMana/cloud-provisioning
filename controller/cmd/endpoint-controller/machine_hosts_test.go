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
}
