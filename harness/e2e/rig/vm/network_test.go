package vm

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/labcontainers/pkg/cloudinit/networkconfig"
)

func TestMachineNetworkUsesGeneratedCloudInitObjects(t *testing.T) {
	for _, node := range lab.Default().Nodes {
		if !node.IsClusterNode() {
			continue
		}
		t.Run(node.Name, func(t *testing.T) {
			cfg, err := NetworkConfig(node)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Version != 2 || len(cfg.Ethernets) != 1 || len(cfg.Bridges) != 0 || len(cfg.Bonds) != 0 || len(cfg.Vlans) != 0 {
				t.Fatalf("unexpected network devices: %+v", cfg)
			}
			physical, ok := cfg.Ethernets[GuestInterface(0)]
			if !ok {
				t.Fatal("configuration must use the guest's native NIC name")
			}
			if !reflect.DeepEqual(physical.Addresses, []string{node.Interfaces[0].Address}) {
				t.Fatalf("guest address changed: %v", physical.Addresses)
			}
			via, _ := lab.Gateway(node.Interfaces[0].Segment)
			if len(physical.Routes) != 1 || physical.Routes[0].To != "default" || physical.Routes[0].Via == nil || *physical.Routes[0].Via != via {
				t.Fatalf("product route changed: %+v", physical.Routes)
			}
			if physical.Nameservers == nil || !reflect.DeepEqual(physical.Nameservers.Addresses, []string{Resolver}) {
				t.Fatalf("product DNS changed: %+v", physical.Nameservers)
			}
			if physical.Gateway4 != nil || physical.Gateway6 != nil || physical.Dhcp4 != nil || physical.Dhcp6 != nil {
				t.Fatal("unexpected alternate address/gateway configuration")
			}
			path := filepath.Join(t.TempDir(), "extra-network.yaml")
			if err := networkconfig.WriteFile(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var decoded networkconfig.NetworkConfigVersion2
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			// Generated open-field decoding may materialize an empty map;
			// compare wire objects, not nil versus empty implementation details.
			roundTrip, err := json.Marshal(&decoded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, roundTrip) {
				t.Fatalf("native network object changed at serialization boundary: %s", data)
			}
		})
	}
}

func TestMachineNetworkRejectsUnsupportedProductTopology(t *testing.T) {
	for _, node := range []lab.Node{
		{Name: "no-link"},
		{Name: "two-links", Interfaces: []lab.Interface{{}, {}}},
		{Name: "no-edge", Interfaces: []lab.Interface{{Segment: "unknown"}}},
	} {
		if _, err := NetworkConfig(node); err == nil {
			t.Fatalf("accepted unsupported product node: %+v", node)
		}
	}
}

func TestMachineNetworkOmitsAbsentAddress(t *testing.T) {
	node := lab.Default().MustNode("remote1")
	node.Interfaces = append([]lab.Interface(nil), node.Interfaces...)
	node.Interfaces[0].Address = ""
	cfg, err := NetworkConfig(node)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ethernets[GuestInterface(0)].Addresses) != 0 {
		t.Fatal("inserted an address not supplied by the topology")
	}
}
