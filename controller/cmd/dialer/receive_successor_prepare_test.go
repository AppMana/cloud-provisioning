package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API storage and inventory admission; native preparation is simulated.
// Changed authority must synchronously reject further work/readiness, with no
// forwarding mutation or allowed interruption. No transition starts, so rollback
// is unnecessary. This does not prove device coexistence, workload continuity,
// site handshake readiness, offline replay or atomicity across API objects.
func verifySuccessorMaintenanceAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cloudKey, siteKey wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := tunnel.NewReceivePreparePredecessor(snapshot, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		current, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, saved.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Data = saved.Data
		if _, err := api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}()
	for _, role := range []string{"cloud", "site"} {
		key := cloudKey
		if role == "site" {
			key = siteKey
		}
		for _, fault := range []string{"none", "missing", "original", "consumed", "stale-predecessor", "inventory-before", "inventory-during", "proposal-during"} {
			t.Run("maintenance-preparation-"+role+"-"+fault, func(t *testing.T) {
				cfg := config{nodeName: role}
				boot, err := localReceiveBootID()
				if err != nil {
					t.Fatal(err)
				}
				ref := predecessor
				request := tunnel.ReceivePrepare{Version: 1, Predecessor: &ref, ReceiverGeneration: strings.Repeat("a", 64), MeshUID: plan.MeshUID, NodeName: plan.CloudNodeName, NodeUID: plan.CloudNodeUID, BootID: boot, RootPublicKey: plan.RootPublicKey, BaseHash: tunnel.HashPeerList(plan.Base), Desired: plan.Desired, Native: plan.Native, Ports: map[string]int{}}
				// JSON null and an empty native-prefix list have different protocol meaning.
				if request.Native == nil {
					request.Native = []netip.Prefix{}
				}
				for root, port := range plan.Ports {
					request.Ports[root] = port + 100
				}
				switch fault {
				case "original":
					request.Predecessor = nil
				case "consumed":
					request.ReceiverGeneration = plan.ReceiverGeneration
				case "stale-predecessor":
					ref.JournalSHA256 = strings.Repeat("0", 64)
				}
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				secret, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				secret.Data = map[string][]byte{tunnel.CloudPeersKey: plan.Base, tunnel.ReceivePrepareKey: raw}
				if fault == "missing" {
					delete(secret.Data, tunnel.ReceivePrepareKey)
				}
				secret, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
				if err != nil {
					t.Fatal(err)
				}
				local := set.Local[plan.MachineUID]
				view, err := local.Snapshot.Transition().View()
				if err != nil {
					t.Fatal(err)
				}
				admittedSecret, _, check, err := admitReceiveSuccessorPreparation(ctx, api, cfg, key, plan, view, local.NodeUID, set.Routes)
				if fault == "stale-predecessor" {
					if err == nil || admittedSecret != nil {
						t.Fatal("stale predecessor admitted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if fault == "missing" || fault == "original" || fault == "consumed" {
					if admittedSecret != nil || check != nil {
						t.Fatal("absent or consumed proposal reached preparation")
					}
					return
				}
				if admittedSecret == nil || check == nil {
					t.Fatal("successor was not admitted")
				}
				mutate := func() error {
					if fault == "proposal-during" {
						current, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						delete(current.Data, tunnel.ReceivePrepareKey)
						_, err = api.CoreV1().Secrets(scope.Namespace).Update(ctx, current, metav1.UpdateOptions{})
						return err
					}
					inventory, err := store.ConfigMaps.Get(ctx, set.Metadata.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if inventory.Annotations == nil {
						inventory.Annotations = map[string]string{}
					}
					inventory.Annotations["test-revision"] = inventory.ResourceVersion
					_, err = store.ConfigMaps.Update(ctx, inventory, metav1.UpdateOptions{})
					return err
				}
				if fault == "inventory-before" {
					if err := mutate(); err != nil {
						t.Fatal(err)
					}
				}
				if role == "site" {
					if fault == "inventory-during" || fault == "proposal-during" {
						if err := mutate(); err != nil {
							t.Fatal(err)
						}
					}
					err = check()
					if (fault == "none") != (err == nil) {
						t.Fatal("incorrect site admission recheck", err)
					}
					return
				}
				calls := 0
				err = reconcileReceivePreparationScoped(ctx, api, admittedSecret, cfg.nodeName, key, func(r tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
					calls++
					scope, err := r.ReceiveScope()
					if err != nil {
						return nil, err
					}
					paths, err := tunnel.PlanReceiveHandover(scope, previous, r.Desired, r.Native)
					if err != nil {
						return nil, err
					}
					var bindings []receiveBinding
					for _, path := range paths.Receivers {
						bindings = append(bindings, receiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: receiverKey(key, scope, path.PublicKey).PublicKey().String(), ListenPort: r.Ports[path.PublicKey]})
					}
					if fault == "inventory-during" || fault == "proposal-during" {
						if err := mutate(); err != nil {
							return nil, err
						}
					}
					return bindings, nil
				}, check)
				current, getErr := api.CoreV1().Secrets(scope.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
				if getErr != nil {
					t.Fatal(getErr)
				}
				if fault == "none" {
					if err != nil || calls != 1 || len(current.Data[tunnel.ReceiveReadyKey]) == 0 {
						t.Fatal("valid preparation failed", err, calls)
					}
				} else {
					expected := 1
					if fault == "inventory-before" {
						expected = 0
					}
					if err == nil || calls != expected || len(current.Data[tunnel.ReceiveReadyKey]) != 0 {
						t.Fatal("changed authority earned readiness or unexpected native calls", err, calls)
					}
				}
			})
		}
	}
}
