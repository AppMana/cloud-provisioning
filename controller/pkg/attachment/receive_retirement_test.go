package attachment

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func retirementFixture(t *testing.T, outcome string, frozen bool, meshUID ...string) (RemoteWithdrawalIntent, handover.Snapshot, tunnel.ReceiveInventory, *corev1.Secret) {
	t.Helper()
	uid := "mesh-uid"
	if len(meshUID) > 0 {
		uid = meshUID[0]
	}
	return retirementFixtureForConsumer(t, outcome, frozen, uid, "test", "site", "site-uid")
}

func retirementFixtureForConsumer(t *testing.T, outcome string, frozen bool, meshUID, namespace, nodeName, nodeUID string) (RemoteWithdrawalIntent, handover.Snapshot, tunnel.ReceiveInventory, *corev1.Secret) {
	t.Helper()
	key := func(b byte) string { var k wgtypes.Key; k[0] = b; return k.String() }
	site, root := key(1), key(2)
	peers := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: site, WGAllowedIPs: []string{"10.90.0.0/24"}}}}
	base, _ := json.Marshal(peers)
	p := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: namespace, MeshName: "mesh", MeshUID: "mesh-uid", MachineName: "worker", MachineUID: "machine-uid", AdoptionName: tunnel.AdoptionSecretName("worker"), AdoptionUID: "adoption-uid", CloudNodeName: "cloud", CloudNodeUID: "cloud-uid", RootPublicKey: root, Base: base, Desired: peers, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}, Ports: map[string]int{}, Bindings: map[string]tunnel.ReceiveBinding{}, Sites: map[string]tunnel.ReceiveTransitionSite{}}
	p.MeshUID = meshUID
	p.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32"}}}}
	prep, err := tunnel.PlanReceiveHandover(p.MeshUID, peers, peers, p.Native)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range prep.Receivers {
		binding := tunnel.ReceiveBinding{Interface: path.Interface, PeerPublicKey: site, PublicKey: key(3), ListenPort: 51821}
		p.Bindings[site], p.Ports[site] = binding, binding.ListenPort
		client, err := tunnel.PlanSiteReceiveClient(p.MeshUID, site, root, binding, p.SiteDesired, p.SiteDesired)
		if err != nil {
			t.Fatal(err)
		}
		p.Sites[site] = tunnel.ReceiveTransitionSite{NodeName: nodeName, NodeUID: nodeUID, Client: client}
	}
	plan, _ := json.Marshal(p)
	spec, err := tunnel.ReceiveTransitionSpec(p, "cluster", tunnel.HashPeerList(plan))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	rolled := false
	for steps := 0; outcome != "unfinished"; steps++ {
		if steps > 10 {
			t.Fatal("transition did not finish")
		}
		view, err := tr.View()
		if err != nil {
			t.Fatal(err)
		}
		if view.Complete || (strings.HasPrefix(outcome, "interrupted-") && strings.TrimPrefix(outcome, "interrupted-") == string(view.Round.Phase)) {
			break
		}
		at = view.Round.IssuedAt.Add(time.Second)
		if !rolled && (((outcome == "abort" || outcome == "interrupted-abort") && view.Round.Phase == handover.Prepare) || (outcome == "rollback" && view.Round.Phase == handover.Switch)) {
			tr, err = tr.Rollback(at)
			rolled = true
		} else {
			if view.Round.Phase == handover.Drain {
				at = view.DrainUntil
			}
			hash, err := view.Round.Digest()
			if err != nil {
				t.Fatal(err)
			}
			var receipts []handover.AuthenticatedReceipt
			for _, m := range view.Round.Members {
				receipts = append(receipts, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
			}
			tr, err = tr.Advance(at, receipts)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	history, _ := tr.Bytes()
	object := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveTransitionName(p.MachineUID), Namespace: p.Namespace, UID: "journal-uid", ResourceVersion: "journal-rv"}, Immutable: &frozen, Data: map[string]string{"plan.json": string(plan), "transition.json": string(history)}}
	raw, _ := json.Marshal(map[string]any{"version": 1, "object": object})
	snapshot, err := (handover.Store{Name: object.Name, ClusterUID: "cluster"}).RestoreCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	w := RemoteWithdrawalIntent{MeshNamespace: p.Namespace, MeshName: p.MeshName, MeshUID: p.MeshUID, SourceVersion: "before", MachineName: p.MachineName, MachineUID: p.MachineUID, PublicKey: root, DrainIntent: "claim/action", Consumers: []ConsumerTarget{{Site: true, NodeName: nodeName, NodeUID: nodeUID, PublicKey: site}}}
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: p.Namespace, MeshName: p.MeshName, MeshUID: p.MeshUID, MachineUIDs: []string{p.MachineUID}, Publications: map[string]tunnel.ReceiveJournalPublication{p.MachineUID: {JournalUID: "journal-uid"}}}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.MeshName, UID: "mesh-uid", ResourceVersion: "after"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + nodeName: []byte(site)}}
	return w, snapshot, inventory, mesh
}

// These are admission tests with synthetic terminal receipts and checkpoint
// storage. They do not establish live API consistency, authenticated retirement
// receipts, native cleanup, interruption bounds, or safe reservation release.
func TestReceiveRetirementRequiresFrozenOwnershipAndRetainedParticipants(t *testing.T) {
	for _, outcome := range []string{"forward", "abort", "rollback"} {
		for _, fault := range []string{"none", "unfrozen", "unfinished", "mesh", "drain", "source", "unpublished-version", "pending-publication", "machine", "root", "cluster", "journal", "legacy-reservation", "missing-reservation", "missing-participant", "replaced-participant", "duplicate", "retiring-node", "republished", "partial-withdrawal", "new-site", "new-remote", "changed-site-key", "unpublished-survivor", "site-without-endpoint"} {
			t.Run(outcome+"/"+fault, func(t *testing.T) {
				state := outcome
				if fault == "unfinished" {
					state = fault
				}
				w, snapshot, inventory, mesh := retirementFixture(t, state, fault != "unfrozen")
				switch fault {
				case "mesh":
					mesh.UID = "replacement"
				case "drain":
					w.DrainIntent = ""
				case "source":
					w.SourceVersion = ""
				case "unpublished-version":
					w.SourceVersion = mesh.ResourceVersion
				case "pending-publication":
					inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: "journal-uid", Plan: json.RawMessage(`{}`)}
				case "machine":
					w.MachineName = "other"
				case "root":
					w.PublicKey = w.Consumers[0].PublicKey
				case "cluster":
					inventory.ClusterUID = "other"
				case "journal":
					inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: "replacement"}
				case "legacy-reservation":
					inventory.Publications = nil
				case "missing-reservation":
					inventory.MachineUIDs = nil
				case "missing-participant":
					w.Consumers = nil
				case "replaced-participant":
					w.Consumers[0].NodeUID = "replacement"
				case "duplicate":
					w.Consumers = append(w.Consumers, w.Consumers[0])
				case "retiring-node":
					w.Consumers[0].NodeUID = "cloud-uid"
				case "republished":
					mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte(w.PublicKey)
				case "partial-withdrawal":
					mesh.Data[tunnel.PeerEndpointPrefix+w.MachineName] = []byte("198.51.100.1:51820")
				case "new-site":
					mesh.Data[tunnel.SiteAddressesPrefix+"new"] = []byte("10.90.0.2")
				case "new-remote":
					mesh.Data[tunnel.PeerPublicKeyPrefix+"new"] = []byte("new")
				case "changed-site-key":
					mesh.Data[tunnel.NodePublicKeyPrefix+"site"] = []byte("replacement")
				case "site-without-endpoint":
					w.Consumers = append(w.Consumers, ConsumerTarget{Site: true, NodeName: "plain-site", NodeUID: "plain-site-uid"}, ConsumerTarget{Site: true, NodeName: "plain-site-two", NodeUID: "plain-site-two-uid"})
					mesh.Data[tunnel.SiteAddressesPrefix+"plain-site"] = []byte("10.90.0.9")
					mesh.Data[tunnel.SiteAddressesPrefix+"plain-site-two"] = []byte("10.90.0.10")
				case "unpublished-survivor":
					delete(mesh.Data, tunnel.NodePublicKeyPrefix+"site")
				}
				before, _ := json.Marshal([]any{w, inventory, mesh})
				plan, err := w.ReceiveRetirementPlan(snapshot, inventory, mesh)
				if fault == "none" || fault == "unpublished-survivor" || fault == "site-without-endpoint" {
					if err != nil || plan.MachineUID != w.MachineUID || len(plan.Sites) != 1 {
						t.Fatal("lost required participant", err)
					}
				} else if err == nil {
					t.Fatal("invalid retirement admitted")
				}
				after, _ := json.Marshal([]any{w, inventory, mesh})
				if !reflect.DeepEqual(before, after) {
					t.Fatal("admission mutated its inputs")
				}
			})
		}
	}
}

// Admission only: a captured data hash tolerates metadata revisions without
// relaxing the complete data snapshot. Legacy captures keep exact-RV behavior.
// This does not authenticate capture, qualify API races or execute native cleanup.
func TestPreparedRetirementAllowsOnlyCapturedDataAcrossMetadataChanges(t *testing.T) {
	for _, fault := range []string{"none", "metadata", "data", "same-version-data", "invalid-hash", "legacy-metadata"} {
		t.Run(fault, func(t *testing.T) {
			w, snapshot, inventory, mesh := retirementFixture(t, "forward", true)
			mesh.ResourceVersion = w.SourceVersion
			mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte(w.PublicKey)
			w.SourceDataHash = withdrawalSourceDataHash(mesh.Data)
			switch fault {
			case "metadata":
				mesh.ResourceVersion = "new-version"
				mesh.Labels = map[string]string{"changed": "true"}
			case "data":
				mesh.ResourceVersion = "new-version"
				mesh.Data["private-key"] = []byte("changed fixture value")
			case "same-version-data":
				mesh.Data[tunnel.PeerRouteHostsPrefix+w.MachineName] = []byte("172.29.0.99")
			case "invalid-hash":
				w.SourceDataHash = "invalid"
			case "legacy-metadata":
				w.SourceDataHash = ""
				mesh.ResourceVersion = "new-version"
			}
			_, err := w.receiveRetirementPlan(snapshot, inventory, mesh, false)
			want := fault == "none" || fault == "metadata"
			if (err == nil) != want {
				t.Fatal("incorrect prepared source admission", fault, err)
			}
		})
	}
}
