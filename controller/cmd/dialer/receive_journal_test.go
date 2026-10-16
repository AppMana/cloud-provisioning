package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type changingJournalReader struct {
	typedcore.ConfigMapInterface
	reads  int
	change func() error
}

func (r *changingJournalReader) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	r.reads++
	if r.reads == 2 {
		if err := r.change(); err != nil {
			return nil, err
		}
	}
	return r.ConfigMapInterface.Get(ctx, name, opts)
}

func TestReceiveJournalAdmissionWithRealAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires real API binaries")
	}
	e := &envtest.Environment{}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	namespace, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "journal-admission"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	boot, err := localReceiveBootID()
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*corev1.Node{}
	for _, name := range []string{"cloud", "site"} {
		node, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		node.Status.NodeInfo.BootID = boot
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		nodes[name], err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	mesh, err := api.CoreV1().Secrets(namespace.Name).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	adoption, err := api.CoreV1().Secrets(namespace.Name).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker")}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := wgtypes.GeneratePrivateKey()
	site, _ := wgtypes.GeneratePrivateKey()
	peer := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: site.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.0/24"}}}}
	base, _ := json.Marshal(peer)
	preparation, err := tunnel.PlanReceiveHandover(string(mesh.UID), peer, peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := tunnel.ReceiveBinding{Interface: preparation.Receivers[0].Interface, PeerPublicKey: site.PublicKey().String(), PublicKey: receiverKey(root, string(mesh.UID), site.PublicKey().String()).PublicKey().String(), ListenPort: 51821}
	siteDoc := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root.PublicKey().String(), Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32"}}}}
	client, err := tunnel.PlanSiteReceiveClient(string(mesh.UID), site.PublicKey().String(), root.PublicKey().String(), binding, siteDoc, siteDoc)
	if err != nil {
		t.Fatal(err)
	}
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: namespace.Name, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineName: "worker", MachineUID: "machine-uid", AdoptionName: adoption.Name, AdoptionUID: string(adoption.UID), CloudNodeName: "cloud", CloudNodeUID: string(nodes["cloud"].UID), RootPublicKey: root.PublicKey().String(), Base: base, Desired: peer, SiteDesired: siteDoc, Native: []netip.Prefix{}, Ports: map[string]int{site.PublicKey().String(): 51821}, Bindings: map[string]tunnel.ReceiveBinding{site.PublicKey().String(): binding}, Sites: map[string]tunnel.ReceiveTransitionSite{site.PublicKey().String(): {NodeName: "site", NodeUID: string(nodes["site"].UID), Client: client}}}
	raw, _ := json.Marshal(plan)
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(namespace.Name), ClusterUID: "admitted-cluster", Name: tunnel.ReceiveTransitionName(plan.MachineUID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "controller", UID: "controller-uid"}}}
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.ReserveReceiveJournal(ctx, store.ConfigMaps, store.ClusterUID, mesh, plan.MachineUID, store.OwnerReferences); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateWithPlan(ctx, transition, raw)
	if err != nil {
		t.Fatal(err)
	}
	// A valid payload and matching hash do not permit moving a journal to
	// another machine name or namespace.
	original, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"journal-name", "journal-namespace"} {
		t.Run(fault, func(t *testing.T) {
			copyStore := store
			copyNamespace := namespace.Name
			if fault == "journal-name" {
				copyStore.Name = tunnel.ReceiveTransitionName("another-machine")
			} else {
				other, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-journal-scope"}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				copyNamespace = other.Name
				copyStore.ConfigMaps = api.CoreV1().ConfigMaps(copyNamespace)
			}
			if _, err := copyStore.ConfigMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: copyStore.Name, Namespace: copyNamespace}, Data: original.Data}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			copied, err := copyStore.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tunnel.DecodeReceiveTransition(copied, copyStore.ClusterUID); err == nil {
				t.Fatal("admitted a journal copied outside its public scope")
			}
		})
	}
	metadata := snapshot.Metadata()
	metadata.OwnerReferences[0].Name = "changed"
	if snapshot.Metadata().OwnerReferences[0].Name != "controller" {
		t.Fatal("metadata accessor mutated committed snapshot")
	}
	scope := receiveJournalScope{Namespace: namespace.Name, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUID: plan.MachineUID}
	for name, key := range map[string]wgtypes.Key{"cloud": root, "site": site} {
		admitted, err := loadReceiveJournal(ctx, api, store, scope, name, key)
		if err != nil || admitted.NodeUID != string(nodes[name].UID) || admitted.BootID != boot {
			t.Fatalf("valid %s admission failed: %v", name, err)
		}
		if admitted.Snapshot.Metadata().ResourceVersion != snapshot.Metadata().ResourceVersion {
			t.Fatal("read-only admission changed journal")
		}
	}
	t.Run("complete-set-admission", func(t *testing.T) {
		verifyReceiveSetAdmission(t, api, store, scope, mesh, plan, site)
	})
	t.Run("durable-recovery", func(t *testing.T) {
		cfg := config{iface: "wgtest", nodeName: "cloud", privateKeyFile: filepath.Join(t.TempDir(), "key")}
		path := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock."+store.Name+".json")
		if _, err := checkpointReceiveJournal(ctx, api, store, scope, cfg, root); err != nil {
			t.Fatal(err)
		}
		for _, apply := range []func() error{
			func() error { return reconcile(ctx, nil, nil, cfg) },
			func() error { return reconcileNode(ctx, nil, nil, cfg, nil) },
		} {
			if err := apply(); !errors.Is(err, errReceivePhaseRequired) {
				t.Fatalf("legacy writer entered after durable admission: %v", err)
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("checkpoint permissions: %v", err)
		}
		// The recovery reader uses no API client.
		restored, err := readReceiveJournalCheckpoint(path, handover.Store{Name: store.Name, ClusterUID: store.ClusterUID}, scope, cfg.nodeName, root)
		if err != nil || restored.Snapshot.Metadata().UID != snapshot.Metadata().UID {
			t.Fatalf("offline recovery: %v", err)
		}
		lock, err := lockReconciliation(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, conflict := checkpointReceiveJournal(ctx, api, store, scope, cfg, root)
		lock.Close()
		if conflict == nil {
			t.Fatal("checkpoint ignored native reconciliation lock")
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			t.Fatal(err)
		}
		hash, err := view.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		var proofs []handover.AuthenticatedReceipt
		for _, member := range view.Round.Members {
			proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: member.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: now}})
		}
		next, err := snapshot.Transition().Advance(now, proofs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Commit(ctx, snapshot, next); err != nil {
			t.Fatal(err)
		}
		if _, err := checkpointReceiveJournal(ctx, api, store, scope, cfg, root); err != nil {
			t.Fatal(err)
		}
		good, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkpointReceiveJournal(ctx, nil, store, scope, cfg, root); err == nil {
			t.Fatal("offline checkpoint refresh claimed live admission")
		}
		preserved, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(preserved, good) {
			t.Fatal("failed live admission changed recovery state")
		}
		advanced, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		regressed := advanced.DeepCopy()
		regressed.Data = original.Data
		regressed, err = store.ConfigMaps.Update(ctx, regressed, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := checkpointReceiveJournal(ctx, api, store, scope, cfg, root); err == nil {
			t.Fatal("admitted a regressed live journal")
		}
		unchanged, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(unchanged, good) {
			t.Fatal("rejected regression changed durable state")
		}
		regressed.Data = advanced.Data
		if _, err := store.ConfigMaps.Update(ctx, regressed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, fault := range []string{"truncated", "boot", "key", "node", "scope"} {
			t.Run(fault, func(t *testing.T) {
				bad := good[:len(good)/2]
				if fault != "truncated" {
					var saved receiveJournalCheckpoint
					if err := json.Unmarshal(good, &saved); err != nil {
						t.Fatal(err)
					}
					switch fault {
					case "boot":
						saved.BootID = "another-boot"
					case "key":
						saved.PublicKey = site.PublicKey().String()
					case "node":
						saved.NodeUID = "replacement"
					case "scope":
						saved.Scope.MeshUID = "another-mesh"
					}
					bad, err = json.Marshal(saved)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := checkpointReceiveJournal(ctx, api, store, scope, cfg, root); err == nil {
					t.Fatal("invalid checkpoint fell back to live admission")
				}
				if err := os.WriteFile(path, good, 0600); err != nil {
					t.Fatal(err)
				}
			})
		}
		if _, err := checkpointReceiveJournal(ctx, api, store, scope, cfg, root); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("phase-execution-boundary", func(t *testing.T) {
		cfg := config{iface: "wgphase", nodeName: "cloud", privateKeyFile: filepath.Join(t.TempDir(), "key")}
		calls := 0
		verify := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
			calls++
			if uid != string(nodes["cloud"].UID) || plan.MachineUID != scope.MachineUID || view.Round.Phase != handover.Switch {
				t.Fatal("wrong phase or participant passed to native verifier")
			}
			if lock, err := lockReconciliation(cfg); err == nil {
				lock.Close()
				t.Fatal("native verification did not retain reconciliation lock")
			}
			return nil
		}
		receipts, err := executeReceivePhase(ctx, api, store, scope, cfg, root, "container-incarnation", verify)
		if err != nil || len(receipts) != 1 || calls != 1 {
			t.Fatalf("execution boundary: receipts=%d calls=%d error=%v", len(receipts), calls, err)
		}
		current, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		v, err := current.Transition().View()
		if err != nil {
			t.Fatal(err)
		}
		hash, err := v.Round.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if receipts[0].BearerToken != "" || receipts[0].BootID != boot || receipts[0].Receipt.RoundHash != hash || receipts[0].Receipt.AgentContainerID != "container-incarnation" {
			t.Fatal("receipt lost observation binding")
		}
		for _, fault := range []string{"native-error", "cancel", "journal-race", "node-race"} {
			t.Run(fault, func(t *testing.T) {
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				native := func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes) error {
					switch fault {
					case "native-error":
						return fmt.Errorf("native readback failed")
					case "cancel":
						cancel()
					case "journal-race":
						cm, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
						if err != nil {
							return err
						}
						cm.Labels = map[string]string{"phase-race": "true"}
						_, err = store.ConfigMaps.Update(ctx, cm, metav1.UpdateOptions{})
						return err
					case "node-race":
						node, err := api.CoreV1().Nodes().Get(ctx, "cloud", metav1.GetOptions{})
						if err != nil {
							return err
						}
						node.Status.NodeInfo.BootID = "changed-boot"
						_, err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
						return err
					}
					return nil
				}
				got, err := executeReceivePhase(runCtx, api, store, scope, cfg, root, "container-incarnation", native)
				if err == nil || len(got) != 0 {
					t.Fatal("failed or superseded native work produced receipts")
				}
				if fault == "node-race" {
					node, err := api.CoreV1().Nodes().Get(ctx, "cloud", metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					node.Status.NodeInfo.BootID = boot
					if _, err := api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				lock, err := lockReconciliation(cfg)
				if err != nil {
					t.Fatal("execution leaked lock", err)
				}
				lock.Close()
			})
		}
		// Delay the final API revalidation past the observation's receipt age.
		// The returned timestamp must not relabel old native work as fresh.
		slow := store
		slowReader := &changingJournalReader{ConfigMapInterface: store.ConfigMaps, reads: -1000, change: func() error { time.Sleep(11 * time.Second); return nil }}
		slow.ConfigMaps = slowReader
		slowVerify := func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, uid string, routes tunnel.ReceiveJournalRoutes) error {
			if err := verify(ctx, plan, view, uid, routes); err != nil {
				return err
			}
			slowReader.reads = 1
			return nil
		}
		if got, err := executeReceivePhase(ctx, api, slow, scope, cfg, root, "container-incarnation", slowVerify); err == nil || !strings.Contains(err.Error(), "aged out") || len(got) != 0 {
			t.Fatalf("aged native observation produced receipts: %v", err)
		}
		before, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		advance := func(tr handover.Transition, at time.Time) handover.Transition {
			v, err := tr.View()
			if err != nil {
				t.Fatal(err)
			}
			hash, err := v.Round.Digest()
			if err != nil {
				t.Fatal(err)
			}
			var proofs []handover.AuthenticatedReceipt
			for _, m := range v.Round.Members {
				proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: at}})
			}
			next, err := tr.Advance(at, proofs)
			if err != nil {
				t.Fatal(err)
			}
			return next
		}
		for _, fault := range []string{"expired", "future", "drain-too-early", "complete"} {
			t.Run(fault, func(t *testing.T) {
				now := time.Now()
				start := now.Add(-10 * time.Second)
				policy := handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 10 * time.Second, DrainInterval: time.Second}
				if fault == "expired" {
					start = now.Add(-2 * time.Minute)
				}
				if fault == "future" {
					start = now.Add(time.Minute)
				}
				if fault == "drain-too-early" {
					policy.DrainInterval = time.Minute
				}
				tr, err := handover.NewTransition(spec, start, policy)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "drain-too-early" || fault == "complete" {
					tr = advance(tr, start.Add(time.Second))
					tr = advance(tr, start.Add(2*time.Second))
				}
				if fault == "complete" {
					tr = advance(tr, start.Add(3*time.Second))
					tr = advance(tr, start.Add(4*time.Second))
				}
				raw, err := tr.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				changed, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				changed.Data["transition.json"] = string(raw)
				changed, err = store.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				fresh := cfg
				fresh.privateKeyFile = filepath.Join(t.TempDir(), "key")
				called := false
				if got, err := executeReceivePhase(ctx, api, store, scope, fresh, root, "container-incarnation", func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes) error {
					called = true
					return nil
				}); err == nil || called || len(got) != 0 {
					t.Fatal("invalid round reached native work")
				}
				changed.Data = before.DeepCopy().Data
				if _, err := store.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			})
		}

	})
	for _, fault := range []string{"mesh", "namespace", "machine", "cluster", "local-key", "node-name", "boot", "not-ready"} {
		t.Run(fault, func(t *testing.T) {
			badScope, badStore, key, name := scope, store, root, "cloud"
			switch fault {
			case "mesh":
				badScope.MeshUID = "replaced"
			case "namespace":
				badScope.Namespace = "elsewhere"
			case "machine":
				badScope.MachineUID = "replaced"
			case "cluster":
				badStore.ClusterUID = "another-cluster"
			case "local-key":
				key, _ = wgtypes.GeneratePrivateKey()
			case "node-name":
				name = "site"
			case "boot", "not-ready":
				node, err := api.CoreV1().Nodes().Get(ctx, "cloud", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if fault == "boot" {
					node.Status.NodeInfo.BootID = "old-boot"
				} else {
					node.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				if _, err := api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					node, err := api.CoreV1().Nodes().Get(ctx, "cloud", metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						return
					}
					node.Status.NodeInfo.BootID = boot
					node.Status.Conditions[0].Status = corev1.ConditionTrue
					if _, err := api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
						t.Error(err)
					}
				})
			}
			if _, err := loadReceiveJournal(ctx, api, badStore, badScope, name, key); err == nil {
				t.Fatal("admitted changed identity")
			}
		})
	}
	raced := store
	raced.ConfigMaps = &changingJournalReader{ConfigMapInterface: store.ConfigMaps, change: func() error {
		cm, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cm.Labels = map[string]string{"changed": "true"}
		_, err = store.ConfigMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}}
	if _, err := loadReceiveJournal(ctx, api, raced, scope, "cloud", root); err == nil {
		t.Fatal("admitted a journal changed between reads")
	}
	if err := api.CoreV1().Nodes().Delete(ctx, "cloud", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	replacement, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.Status = nodes["cloud"].Status
	if _, err := api.CoreV1().Nodes().UpdateStatus(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReceiveJournal(ctx, api, store, scope, "cloud", root); err == nil {
		t.Fatal("replacement node adopted the previous incarnation's journal")
	}
	// Use the still-valid site participant so node replacement cannot mask
	// rejection of a replaced adoption object or a deleting journal.
	if _, err := loadReceiveJournal(ctx, api, store, scope, "site", site); err != nil {
		t.Fatal(err)
	}
	if err := api.CoreV1().Secrets(namespace.Name).Delete(ctx, adoption.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CoreV1().Secrets(namespace.Name).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: adoption.Name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReceiveJournal(ctx, api, store, scope, "site", site); err == nil {
		t.Fatal("replacement adoption object admitted an old journal")
	}
	cm, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Finalizers = []string{"testing.example.com/retain"}
	if _, err := store.ConfigMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigMaps.Delete(ctx, store.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deleting, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.DecodeReceiveTransition(deleting, store.ClusterUID); err == nil {
		t.Fatal("shared decoder admitted a deleting journal")
	}

}
