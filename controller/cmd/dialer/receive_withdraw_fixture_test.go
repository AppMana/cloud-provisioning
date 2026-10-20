package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Reuse the native serving fixture for a second, independently owned worker.
func secondNativeWithdrawal(t *testing.T, site wgtypes.Key) (tunnel.ReceiveTransitionPlan, wgtypes.Key) {
	t.Helper()
	root, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	plan := receiveRestoreTestPlan(t, root, site)
	plan.MachineName, plan.MachineUID = "worker-two", "worker-uid-z"
	plan.CloudNodeName, plan.CloudNodeUID = "cloud-two", "cloud-two-uid"
	plan.AdoptionName, plan.AdoptionUID = tunnel.AdoptionSecretName(plan.MachineName), "adoption-two"
	plan.CloudAddresses = []string{"172.29.0.2/32", "fd29::2/128"}
	plan.SiteDesired.Peers[0].Endpoint = "198.51.100.2:51820"
	plan.SiteDesired.Peers[0].WGAllowedIPs = plan.CloudAddresses
	plan.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.2", "fd29::2"}
	public := site.PublicKey().String()
	local := plan.Sites[public]
	local.Client, err = tunnel.PlanSiteReceiveClient(plan.MeshUID, public, plan.RootPublicKey, plan.Bindings[public], plan.SiteDesired, plan.SiteDesired)
	if err != nil {
		t.Fatal(err)
	}
	plan.Sites[public] = local
	return plan, root
}

func addNativeWithdrawalPeer(mesh *corev1.Secret, plan tunnel.ReceiveTransitionPlan) {
	peer := plan.SiteDesired.Peers[0]
	mesh.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(plan.RootPublicKey)
	mesh.Data[tunnel.PeerEndpointPrefix+plan.MachineName] = []byte(peer.Endpoint)
	mesh.Data[tunnel.PeerAllowedIPsPrefix+plan.MachineName] = []byte(strings.Join(peer.WGAllowedIPs, ","))
	mesh.Data[tunnel.PeerRouteHostsPrefix+plan.MachineName] = []byte(strings.Join(peer.RouteHosts, ","))
}

// Synthetic frozen authority for native-only tests. Capture both retiring
// identities before unpublishing either; real-token and API-CAS tests are separate.
func nativeWithdrawalJournals(t *testing.T, mesh *corev1.Secret, plans []tunnel.ReceiveTransitionPlan, consumers []attachment.ConsumerTarget, phase string) (tunnel.ReceiveInventory, []runtime.Object) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(mesh.Data)
	must(err)
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "native-cluster", Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), Publications: map[string]tunnel.ReceiveJournalPublication{}}
	objects := []runtime.Object{}
	for i, plan := range plans {
		journal := cloudWithdrawalTestJournal(t, plan, fmt.Sprintf("journal-%d", i), phase)
		intent := attachment.RemoteWithdrawalIntent{MeshNamespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: mesh.ResourceVersion, MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "native-test", Consumers: slices.Clone(consumers)}
		version := 1
		fields := map[string][]byte{}
		if len(plans) > 1 {
			version = 2
			intent.SourceDataHash = tunnel.HashPeerList(raw)
			for j, other := range plans {
				if i == j {
					continue
				}
				address, err := netip.ParsePrefix(other.CloudAddresses[0])
				must(err)
				intent.Consumers = append(intent.Consumers, attachment.ConsumerTarget{NodeName: other.CloudNodeName, NodeUID: other.CloudNodeUID, MachineName: other.MachineName, SecretUID: other.AdoptionUID, PublicKey: other.RootPublicKey, TunnelAddress: address.Addr().String()})
			}
			remaining, present, err := tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
			must(err)
			if !present {
				t.Fatal("native fixture did not publish retiring peer")
			}
			for name, value := range mesh.Data {
				if _, found := remaining[name]; !found {
					fields[name] = value
				}
			}
		}
		if phase != "" {
			encoded, err := json.Marshal(intent)
			must(err)
			journal.Data["withdrawal-intent-sha256"] = tunnel.HashPeerList(encoded)
		}
		record := map[string]any{"version": version, "journalSHA256": tunnel.HashPeerList([]byte(journal.Data["transition.json"])), "withdrawal": intent}
		if version == 2 {
			record["peerData"] = fields
		}
		retirement, err := json.Marshal(record)
		must(err)
		inventory.MachineUIDs = append(inventory.MachineUIDs, plan.MachineUID)
		inventory.Publications[plan.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID), Retirement: retirement}
		objects = append(objects, journal)
	}
	slices.Sort(inventory.MachineUIDs)
	return inventory, objects
}
