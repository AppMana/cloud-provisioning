package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API and durable retirement fences; native forwarding, finalization and
// release authority come from the synthetic execution fixture. This exercises
// all-retired admission and new-address intent, not native reuse or reboot.
func verifyReleasedNeighborRecovery(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg.secretNamespace, cfg.secretName, cfg.receiptMeshName = scope.Namespace, scope.MeshName, scope.MeshName
	cfg.receiptURL, cfg.receiptCAFile, cfg.receiptTokenFile = "https://unused.invalid", "unused", "unused"
	must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	must(err)
	original := mesh.DeepCopy()
	defer func() {
		current, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		must(e)
		current.Data = original.Data
		_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
		must(e)
	}()
	replacement, err := wgtypes.GeneratePrivateKey()
	must(err)
	snapshot, err := store.Load(ctx)
	must(err)
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	must(err)
	mesh.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName] = []byte(key.PublicKey().String())
	mesh.Data[tunnel.PeerPublicKeyPrefix+"replacement"] = []byte(replacement.PublicKey().String())
	mesh.Data[tunnel.PeerEndpointPrefix+"replacement"] = []byte("198.51.100.77:51820")
	mesh.Data[tunnel.PeerAllowedIPsPrefix+"replacement"] = []byte(strings.Join(plan.SiteDesired.Peers[0].WGAllowedIPs, ","))
	var hosts []string
	for _, raw := range plan.SiteDesired.Peers[0].WGAllowedIPs {
		prefix, e := netip.ParsePrefix(raw)
		must(e)
		hosts = append(hosts, prefix.Addr().String())
	}
	mesh.Data[tunnel.PeerRouteHostsPrefix+"replacement"] = []byte(strings.Join(hosts, ","))
	mesh, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	desired := mesh.DeepCopy()
	lockPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock")
	originalInventory, err := os.ReadFile(lockPath + ".inventory.json")
	must(err)
	originalJournal, err := os.ReadFile(lockPath + "." + store.Name + ".json")
	must(err)
	for _, fault := range []string{"none", "previous-boot", "missing-fence", "missing-journal", "old-key", "unpublished-site", "changed-site-key", "mesh-during-native", "agent-during-native", "native-error"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				must(os.WriteFile(lockPath+".inventory.json", originalInventory, 0600))
				must(os.WriteFile(lockPath+"."+store.Name+".json", originalJournal, 0600))
				current, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
				must(e)
				current.Data = desired.Data
				_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				must(e)
			}()
			switch fault {
			case "missing-fence":
				must(os.Remove(lockPath + ".inventory.json"))
			case "missing-journal":
				must(os.Remove(lockPath + "." + store.Name + ".json"))
			case "previous-boot":
				var saved receiveInventoryCheckpoint
				must(json.Unmarshal(originalInventory, &saved))
				saved.BootID = "previous-boot"
				must(writeDurableJSON(lockPath+".inventory.json", saved))
			case "unpublished-site", "changed-site-key":
				current, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
				must(e)
				if fault == "unpublished-site" {
					delete(current.Data, tunnel.NodeTunnelAddressPrefix+cfg.nodeName)
				} else {
					current.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName] = []byte(replacement.PublicKey().String())
				}
				_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				must(e)
			case "old-key":
				current, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
				must(e)
				current.Data[tunnel.PeerPublicKeyPrefix+"replacement"] = []byte(plan.RootPublicKey)
				_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				must(e)
			}
			handled, e := dispatchReceivePhases(ctx, api, cfg, func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error {
				t.Fatal("released history reached phase execution")
				return nil
			}, nil)
			must(e)
			if !handled {
				t.Fatal("all-retired inventory fell through to legacy")
			}
			calls := 0
			denied := errors.New("native readback failed")
			e = reconcileReceiveNeighbors(ctx, api, cfg, func(ctx context.Context, _ wgtypes.Key, peers []tunnel.PeerSpec, _ []string, retiring []tunnel.PeerSpec, allocated []string, commit func() error) error {
				calls++
				if len(allocated) != 1 || allocated[0] != string(desired.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName]) {
					t.Fatal("lost local allocation protection")
				}
				found := false
				for _, peer := range peers {
					if peer.PublicKey == plan.RootPublicKey {
						t.Fatal("restored retired owner")
					}
					found = found || peer.PublicKey == replacement.PublicKey().String()
				}
				if !found {
					t.Fatal("replacement intent missing")
				}
				if len(retiring) != 0 {
					t.Fatal("released receiver became ordinary deletion authority")
				}
				if _, err := lockLegacyReconciliation(cfg); err == nil {
					t.Fatal("legacy recovery escaped fence")
				}
				if fault == "native-error" {
					return denied
				}
				if fault == "mesh-during-native" {
					current, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
					must(e)
					current.Annotations = map[string]string{"released-race": "changed"}
					_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
					must(e)
				}
				if fault == "agent-during-native" {
					pod, e := api.CoreV1().Pods(scope.Namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
					must(e)
					original := pod.DeepCopy()
					pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
					_, e = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
					must(e)
					defer func() {
						current, e := api.CoreV1().Pods(scope.Namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
						must(e)
						current.Status = original.Status
						_, e = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
						must(e)
					}()
				}
				return commit()
			}, nil)
			valid := fault == "none" || fault == "previous-boot"
			if (e == nil) != valid {
				t.Fatalf("valid %t, got %v", valid, e)
			}
			want := 1
			if fault == "missing-fence" || fault == "missing-journal" || fault == "old-key" || fault == "unpublished-site" || fault == "changed-site-key" {
				want = 0
			}
			if calls != want {
				t.Fatalf("native calls %d want %d", calls, want)
			}
			if fault == "native-error" && !errors.Is(e, denied) {
				t.Fatal("native failure lost", e)
			}
			retained, e := os.ReadFile(lockPath + "." + store.Name + ".json")
			if fault != "missing-journal" {
				must(e)
				if string(retained) != string(originalJournal) {
					t.Fatal("retained journal rewritten")
				}
			}
		})
	}
}
