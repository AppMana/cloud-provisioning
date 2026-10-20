package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Uses the existing real-API fixture with one active and one released journal.
// This proves dispatch selection and durable-file fencing, not native adoption,
// packet continuity, actual worker reboot or address reuse.
func verifyUnreservedCloudDispatch(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, activeKey, consumerKey wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	node, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fresh-cloud-worker"}}, metav1.CreateOptions{})
	must(err)
	defer func() { must(api.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})) }()
	key, err := wgtypes.GeneratePrivateKey()
	must(err)
	retired, err := store.Load(ctx)
	must(err)
	retiredPlan, err := tunnel.DecodeReceiveTransition(retired, store.ClusterUID)
	must(err)
	for _, fault := range []string{"fresh", "active-key", "retired-adoption", "retained-consumer-key", "saved-fence", "missing-node", "cancelled"} {
		t.Run("unreserved-cloud/"+fault, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config{iface: "freshcloud", nodeName: node.Name, peersFile: filepath.Join(dir, "peers.json"), peersSecretNamespace: scope.Namespace, peersSecretName: "fresh-cloud-worker-tunnel-peers", receiptMeshName: scope.MeshName, receiptURL: "https://unused.invalid", receiptCAFile: "unused", receiptTokenFile: "unused", receiptPodName: "fresh-agent", receiptPodUID: "fresh-agent-uid"}
			identity := key
			switch fault {
			case "active-key":
				identity = activeKey
			case "retired-adoption":
				cfg.peersSecretName = retiredPlan.AdoptionName
			case "retained-consumer-key":
				identity = consumerKey
			case "saved-fence":
				must(os.WriteFile(filepath.Join(dir, cfg.iface+".reconcile.lock.inventory.json"), []byte("{}"), 0600))
			case "missing-node":
				cfg.nodeName = "absent-worker"
			}
			raw, err := json.Marshal(tunnel.PeersFileDoc{PrivateKey: identity.String(), LocalAddress: "172.29.0.99/32"})
			must(err)
			must(os.WriteFile(cfg.peersFile, raw, 0600))
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			calls := 0
			native := func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error {
				calls++
				return nil
			}
			handled, err := dispatchReceivePhases(runCtx, api, cfg, native, native)
			if calls != 0 {
				t.Fatal("dispatch unexpectedly executed a receiver phase")
			}
			if fault == "fresh" {
				if err != nil || handled {
					t.Fatalf("unrelated release blocked fresh worker: handled=%t err=%v", handled, err)
				}
			} else if !handled {
				t.Fatalf("%s escaped receiver fencing: %v", fault, err)
			}
		})
	}
}
