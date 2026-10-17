package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type receiveSetReader struct {
	typedcore.ConfigMapInterface
	before func(string) error
}

func (r receiveSetReader) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	if err := r.before(name); err != nil {
		return nil, err
	}
	return r.ConfigMapInterface.Get(ctx, name, opts)
}

// The callback is a simulated native verifier. These tests prove complete-set
// admission and durable fencing, not network continuity or native completion.
func verifyReceiveSetAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, mesh *corev1.Secret, first tunnel.ReceiveTransitionPlan, site wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	_, originalIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := wgtypes.GeneratePrivateKey()
	other := first
	other.MachineName, other.MachineUID = "worker-two", "second-machine-uid"
	other.CloudNodeName, other.CloudNodeUID = "cloud-two", "second-cloud-uid"
	other.RootPublicKey = otherKey.PublicKey().String()
	adoption, err := api.CoreV1().Secrets(scope.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(other.MachineName)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other.AdoptionName, other.AdoptionUID = adoption.Name, string(adoption.UID)
	oldBinding := first.Bindings[site.PublicKey().String()]
	binding := oldBinding
	binding.PublicKey = receiverKey(otherKey, scope.MeshUID, site.PublicKey().String()).PublicKey().String()
	other.Bindings = map[string]tunnel.ReceiveBinding{site.PublicKey().String(): binding}
	other.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: other.RootPublicKey, Remote: true, Endpoint: "198.51.100.2:51820", WGAllowedIPs: []string{"172.29.0.2/32"}}}}
	client, err := tunnel.PlanSiteReceiveClient(scope.MeshUID, site.PublicKey().String(), other.RootPublicKey, binding, other.SiteDesired, other.SiteDesired)
	if err != nil {
		t.Fatal(err)
	}
	siteInfo := first.Sites[site.PublicKey().String()]
	siteInfo.Client = client
	other.Sites = map[string]tunnel.ReceiveTransitionSite{site.PublicKey().String(): siteInfo}
	if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, other.MachineUID, store.OwnerReferences); err != nil {
		t.Fatal(err)
	}
	otherStore := store
	otherStore.Name = tunnel.ReceiveTransitionName(other.MachineUID)
	defer func() {
		if err := store.ConfigMaps.Delete(ctx, otherStore.Name, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		current.Data = originalIndex.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}()
	raw, _ := json.Marshal(other)
	spec, err := tunnel.ReceiveTransitionSpec(other, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tr, err := handover.NewTransition(spec, now, handover.Policy{RoundLifetime: 45 * time.Second, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := otherStore.CreateWithPlan(ctx, tr, raw)
	if err != nil {
		t.Fatal(err)
	}
	tr = snapshot.Transition()
	view, _ := tr.View()
	hash, _ := view.Round.Digest()
	var proofs []handover.AuthenticatedReceipt
	for _, m := range view.Round.Members {
		proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: now}})
	}
	next, err := tr.Advance(now, proofs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherStore.Commit(ctx, snapshot, next); err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: "wgset", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key")}
	calls := 0
	verify := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(now.Add(45*time.Second)) {
			t.Fatal("combined work did not use the earliest local round expiry")
		}
		if plan.MachineUID != first.MachineUID || view.Round.Phase != handover.Prepare || uid != siteInfo.NodeUID {
			t.Fatal("wrong target passed to native callback")
		}
		sources, found := routes.Sources[uid]
		if !found || len(sources.Addresses) != 1 || sources.Addresses[0] != "10.90.0.2/24" || len(sources.Hosts) != 0 {
			t.Fatal("native callback lost the admitted site allocation or invented source exceptions")
		}
		selection, found := routes.Receivers[uid]["172.29.0.2/32"]
		if !found || selection.Interface != client.Interface || len(routes.Receivers[uid]) != 1 || len(routes.Legacy) != 1 || routes.Legacy[first.MachineUID].MachineUID != first.MachineUID {
			t.Fatal("combined intent lost the other worker or selected the preparing worker")
		}
		return nil
	}
	if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err != nil || len(got) != 1 || calls != 1 {
		t.Fatalf("two-worker execution admission: %v", err)
	}
	lockPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock")
	for _, suffix := range []string{"inventory.json", store.Name + ".json", otherStore.Name + ".json"} {
		if info, err := os.Stat(lockPath + "." + suffix); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("missing private complete-set checkpoint %s: %v", suffix, err)
		}
	}
	t.Run("missing-other-journal", func(t *testing.T) {
		broken := store
		broken.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
			if name == otherStore.Name {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
			}
			return nil
		}}
		before := calls
		if got, err := executeReceivePhase(ctx, api, broken, scope, cfg, site, "container", verify); !apierrors.IsNotFound(err) || len(got) != 0 || calls != before {
			t.Fatal("missing other worker permitted native work")
		}
	})
	for _, target := range []string{otherStore.Name, originalIndex.Name} {
		t.Run("changed-during-read-"+target, func(t *testing.T) {
			count := 0
			raced := store
			raced.ConfigMaps = receiveSetReader{ConfigMapInterface: store.ConfigMaps, before: func(name string) error {
				if name != target {
					return nil
				}
				count++
				if count != 2 {
					return nil
				}
				object, err := store.ConfigMaps.Get(ctx, target, metav1.GetOptions{})
				if err != nil {
					return err
				}
				object.Labels = map[string]string{"read-race": fmt.Sprint(time.Now().UnixNano())}
				_, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
				return err
			}}
			before := calls
			if got, err := executeReceivePhase(ctx, api, raced, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
				t.Fatal("changing complete set permitted native work")
			}
		})
		t.Run("changed-during-native-"+target, func(t *testing.T) {
			native := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
				if err := verify(ctx, plan, view, uid, routes); err != nil {
					return err
				}
				object, err := store.ConfigMaps.Get(ctx, target, metav1.GetOptions{})
				if err != nil {
					return err
				}
				object.Labels = map[string]string{"native-race": fmt.Sprint(time.Now().UnixNano())}
				_, err = store.ConfigMaps.Update(ctx, object, metav1.UpdateOptions{})
				return err
			}
			before := calls
			if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", native); err == nil || len(got) != 0 || calls != before+1 {
				t.Fatal("changed other worker or inventory produced receipts", err)
			}
		})
	}
	t.Run("inventory-shrink", func(t *testing.T) {
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		saved := current.DeepCopy()
		current.Data = originalIndex.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "regressed") || len(got) != 0 || calls != before {
			t.Fatal("shrunk inventory discarded another worker")
		}
		current, err = store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Data = saved.Data
		if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("corrupt-inventory-checkpoint", func(t *testing.T) {
		path := lockPath + ".inventory.json"
		good, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || len(got) != 0 || calls != before {
			t.Fatal("corrupt checkpoint permitted live fallback")
		}
		if err := os.WriteFile(path, good, 0600); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unindexed-local-checkpoint", func(t *testing.T) {
		path := lockPath + "." + tunnel.ReceiveTransitionName("omitted-worker") + ".json"
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(path)
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "omitted a checkpointed") || len(got) != 0 || calls != before {
			t.Fatal("unindexed local journal permitted native work", err)
		}
	})
	for _, offset := range []time.Duration{-2 * time.Minute, time.Minute} {
		t.Run("other-worker-not-executable-"+offset.String(), func(t *testing.T) {
			current, err := store.ConfigMaps.Get(ctx, otherStore.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			good := current.DeepCopy()
			tr, err := handover.NewTransition(spec, time.Now().Add(offset), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := tr.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			current.Data["transition.json"] = string(raw)
			if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			fresh := cfg
			fresh.privateKeyFile = filepath.Join(t.TempDir(), "key")
			before := calls
			if got, err := executeReceivePhase(ctx, api, store, scope, fresh, site, "container", verify); err == nil || !strings.Contains(err.Error(), "another local handover") || len(got) != 0 || calls != before {
				t.Fatal("inactive other worker allowed combined route execution", err)
			}
			current, err = store.ConfigMaps.Get(ctx, otherStore.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			current.Data = good.Data
			if _, err := store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("replaced-inventory", func(t *testing.T) {
		current, err := store.ConfigMaps.Get(ctx, originalIndex.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ConfigMaps.Delete(ctx, current.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		current.UID, current.ResourceVersion = "", ""
		if _, err := store.ConfigMaps.Create(ctx, current, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		before := calls
		if got, err := executeReceivePhase(ctx, api, store, scope, cfg, site, "container", verify); err == nil || !strings.Contains(err.Error(), "checkpoint identity changed") || len(got) != 0 || calls != before {
			t.Fatal("replacement inventory inherited durable admission", err)
		}
	})
}
