package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API admission and durable fences with a simulated serving callback.
// This exercises initial observers, not native routes or workload continuity.
func verifyInitialReceiveObserver(t *testing.T, api kubernetes.Interface, store handover.Store, cfg config, key wgtypes.Key, inventory tunnel.ReceiveInventory) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mesh, err := api.CoreV1().Secrets(inventory.Namespace).Get(ctx, inventory.MeshName, metav1.GetOptions{})
	must(err)
	index, err := store.ConfigMaps.Get(ctx, tunnel.ReceiveInventoryName(inventory.MeshUID), metav1.GetOptions{})
	must(err)
	originalMesh, originalIndex := mesh.DeepCopy(), index.DeepCopy()
	defer func() {
		m, e := api.CoreV1().Secrets(inventory.Namespace).Get(ctx, inventory.MeshName, metav1.GetOptions{})
		must(e)
		m.Data = originalMesh.Data
		_, e = api.CoreV1().Secrets(inventory.Namespace).Update(ctx, m, metav1.UpdateOptions{})
		must(e)
		i, e := store.ConfigMaps.Get(ctx, index.Name, metav1.GetOptions{})
		must(e)
		i.Data = originalIndex.Data
		_, e = store.ConfigMaps.Update(ctx, i, metav1.UpdateOptions{})
		must(e)
	}()
	inventory.Released = nil
	raw, err := json.Marshal(inventory)
	must(err)
	index.Data["inventory.json"] = string(raw)
	_, err = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
	must(err)
	for _, prefix := range []string{tunnel.NodePublicKeyPrefix, tunnel.NodeTunnelAddressPrefix, tunnel.NodeTunnelAddress6Prefix} {
		delete(mesh.Data, prefix+cfg.nodeName)
	}
	mesh, err = api.CoreV1().Secrets(inventory.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	for _, fault := range []string{"fresh", "retry", "orphaned-follower", "orphaned-journal", "orphaned-neighbors", "corrupt-inventory", "wrong-agent", "cancelled", "mesh-race"} {
		t.Run("initial-observer/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			run := cfg
			run.privateKeyFile = filepath.Join(t.TempDir(), "key")
			must(os.WriteFile(run.privateKeyFile, []byte(key.String()), 0600))
			lockPath := filepath.Join(filepath.Dir(run.privateKeyFile), run.iface+".reconcile.lock")
			switch fault {
			case "orphaned-follower":
				must(os.WriteFile(lockPath+".follower.json", []byte("{}"), 0600))
			case "orphaned-journal":
				must(os.WriteFile(lockPath+".receive-transition-old.json", []byte("{}"), 0600))
			case "orphaned-neighbors":
				must(os.WriteFile(lockPath+".neighbors.json", []byte("{}"), 0600))
			case "corrupt-inventory":
				must(os.WriteFile(lockPath+".inventory.json", []byte("{}"), 0600))
			case "wrong-agent":
				run.receiptPodUID = "replacement"
			}
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			native := func(context.Context, wgtypes.Key, []tunnel.PeerSpec, []string, []tunnel.PeerSpec, []string, func() error) error {
				t.Fatal("observer reached ordinary native writer")
				return nil
			}
			calls, writes := 0, 0
			follower := func(useCtx context.Context, gotKey wgtypes.Key, gotPath string, admitted receiveObserverAdmission, check func() error) error {
				calls++
				if gotKey != key || gotPath != lockPath || len(admitted.Inventory.Released) != 0 || len(admitted.Journals) == 0 {
					t.Fatal("initial observer lost its live scope")
				}
				if fault == "mesh-race" {
					m, e := api.CoreV1().Secrets(inventory.Namespace).Get(useCtx, inventory.MeshName, metav1.GetOptions{})
					must(e)
					m.Annotations = map[string]string{"test/initial-observer": "changed"}
					_, e = api.CoreV1().Secrets(inventory.Namespace).Update(useCtx, m, metav1.UpdateOptions{})
					must(e)
				}
				if err := check(); err != nil {
					return err
				}
				writes++
				return nil
			}
			err := reconcileReceiveNeighbors(runCtx, api, run, native, nil, follower)
			if fault == "fresh" || fault == "retry" {
				must(err)
				if calls != 1 || writes != 1 {
					t.Fatal("initial serving callback not executed")
				}
				if fault == "retry" {
					must(reconcileReceiveNeighbors(runCtx, api, run, native, nil, follower))
					if calls != 2 || writes != 2 {
						t.Fatal("observer retry lost authority")
					}
				}
				raw, e := readPrivateRetirementCache(lockPath + ".inventory.json")
				must(e)
				var saved receiveInventoryCheckpoint
				must(json.Unmarshal(raw, &saved))
				if len(saved.Inventory.Released) != 0 || len(saved.Inventory.MachineUIDs) != len(inventory.MachineUIDs) {
					t.Fatal("initial checkpoint omitted live inventory")
				}
			} else {
				if err == nil || writes != 0 {
					t.Fatalf("invalid initial observer accepted: %s calls=%d writes=%d", fault, calls, writes)
				}
				if fault != "mesh-race" && fault != "corrupt-inventory" {
					if _, e := os.Lstat(lockPath + ".inventory.json"); !os.IsNotExist(e) {
						t.Fatal("rejected initial admission wrote inventory")
					}
				}
			}
		})
	}
	// Discovery must select the phase-aware neighbor writer even before the first
	// retirement. No receiver execution or receipt delivery is needed for a follower.
	dispatch := cfg
	dispatch.receiptURL = "https://unused.invalid"
	dispatch.receiptCAFile = "unused"
	dispatch.receiptTokenFile = "unused"
	handled, err := dispatchReceivePhases(ctx, api, dispatch, func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error {
		return fmt.Errorf("follower reached receiver executor")
	}, nil)
	must(err)
	if !handled {
		t.Fatal("first active journal fell through to legacy relay election")
	}
}
