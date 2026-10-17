package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Exercise the production admission boundary with journal objects produced by
// the real-API fixture. The native callback is simulated; packet behavior is
// measured separately by TestNativeReceiveNeighbors.
func verifyReceiveNeighborAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, plan tunnel.ReceiveTransitionPlan, site, root wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	must(err)
	journal, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	must(err)
	adoption, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
	must(err)
	node, err := api.CoreV1().Nodes().Get(ctx, "site", metav1.GetOptions{})
	must(err)
	var inventory tunnel.ReceiveInventory
	must(json.Unmarshal([]byte(index.Data["inventory.json"]), &inventory))
	inventory.Publications = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: {JournalUID: string(journal.UID)}}
	encoded, err := json.Marshal(inventory)
	must(err)
	index = index.DeepCopy()
	index.Data["inventory.json"] = string(encoded)
	neighbor, err := wgtypes.GeneratePrivateKey()
	must(err)
	mesh = mesh.DeepCopy()
	mesh.Data[tunnel.PeerPublicKeyPrefix+"joining"] = []byte(neighbor.PublicKey().String())
	mesh.Data[tunnel.PeerEndpointPrefix+"joining"] = []byte("198.51.100.2:51820")
	mesh.Data[tunnel.PeerAllowedIPsPrefix+"joining"] = []byte("172.29.0.2/32")
	mesh.Data[tunnel.PeerRouteHostsPrefix+"joining"] = []byte("172.29.0.2")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: mesh.Namespace, Name: "neighbor-agent", UID: "neighbor-pod"}, Spec: corev1.PodSpec{NodeName: "site"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://initial", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	objects := []runtime.Object{mesh, index, journal, adoption, node, pod, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(store.ClusterUID)}}}
	for _, fault := range []string{"valid", "complete", "previous-boot", "previous-boot-stale-api", "empty-boot", "mesh-before", "mesh-after", "journal-after", "cluster-after", "inventory-after", "boot", "agent", "agent-after", "checkpoint", "overlap", "missing-inventory", "native", "prepare-after"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			copies := make([]runtime.Object, 0, len(objects))
			for _, object := range objects {
				copies = append(copies, object.DeepCopyObject())
			}
			k := fake.NewClientset(copies...)
			cfg := config{iface: "wgneighbors", nodeName: "site", secretNamespace: mesh.Namespace, secretName: mesh.Name, receiptMeshName: mesh.Name, receiptPodName: pod.Name, receiptPodUID: string(pod.UID), privateKeyFile: filepath.Join(t.TempDir(), "key")}
			must(os.WriteFile(cfg.privateKeyFile, []byte(site.String()), 0600))
			checkpointPath := filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock.neighbors.json")
			var oldCheckpoint []byte
			var cachedRetiredKey string
			if fault == "native" {
				retired, err := wgtypes.GeneratePrivateKey()
				must(err)
				oldCheckpoint, err = json.Marshal(receiveNeighborCheckpoint{Version: 1, InventoryUID: string(index.UID), MeshUID: string(mesh.UID), MeshVersion: "old", NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, PublicKey: site.PublicKey().String(), Peers: []tunnel.PeerSpec{{PublicKey: retired.PublicKey().String(), Remote: true, Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{"172.29.99.1/32"}}}})
				must(err)
				must(os.WriteFile(checkpointPath, oldCheckpoint, 0600))
				cachedKey, err := wgtypes.GeneratePrivateKey()
				must(err)
				cachedRetiredKey = cachedKey.PublicKey().String()
				cachedMesh := mesh.DeepCopy()
				cachedMesh.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName] = []byte(site.PublicKey().String())
				cachedMesh.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName] = []byte("10.100.0.1/24")
				cachedMesh.Data[tunnel.PeerPublicKeyPrefix+"retired"] = []byte(cachedRetiredKey)
				cachedMesh.Data[tunnel.PeerEndpointPrefix+"retired"] = []byte("198.51.100.98:51820")
				cachedMesh.Data[tunnel.PeerAllowedIPsPrefix+"retired"] = []byte("172.29.99.2/32")
				cachedMesh.Data[tunnel.PeerRouteHostsPrefix+"retired"] = []byte("172.29.99.2")
				must(saveSiteForwardingCache(cfg, site, cachedMesh, nil, map[string]bool{}, nil))
			}
			if fault == "previous-boot" || fault == "previous-boot-stale-api" || fault == "empty-boot" {
				boot := "previous-boot"
				if fault == "empty-boot" {
					boot = ""
				}
				oldCheckpoint, err = json.Marshal(receiveNeighborCheckpoint{Version: 1, InventoryUID: string(index.UID), MeshUID: string(mesh.UID), MeshVersion: "old", NodeUID: string(node.UID), BootID: boot, PublicKey: site.PublicKey().String(), Peers: []tunnel.PeerSpec{}})
				must(err)
				must(os.WriteFile(checkpointPath, oldCheckpoint, 0600))
			}

			changeMesh := func() error {
				current, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				current.ResourceVersion += "-changed"
				_, err = k.CoreV1().Secrets(mesh.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				return err
			}
			switch fault {
			case "complete":
				localStore := store
				localStore.ConfigMaps = k.CoreV1().ConfigMaps(mesh.Namespace)
				snapshot, err := localStore.Load(ctx)
				must(err)
				terminal, err := snapshot.Transition().Rollback(time.Now())
				must(err)
				view, err := terminal.View()
				must(err)
				hash, err := view.Round.Digest()
				must(err)
				var proofs []handover.AuthenticatedReceipt
				for _, member := range view.Round.Members {
					proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: member.NodeUID, Receipt: handover.Receipt{RoundHash: hash, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: time.Now()}})
				}
				terminal, err = terminal.Advance(time.Now(), proofs)
				must(err)
				encoded, err := terminal.Bytes()
				must(err)
				current := journal.DeepCopy()
				current.Data["transition.json"] = string(encoded)
				_, err = k.CoreV1().ConfigMaps(mesh.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			case "mesh-before":
				reads := 0
				k.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					if action.(k8stesting.GetAction).GetName() != mesh.Name {
						return false, nil, nil
					}
					reads++
					if reads != 2 {
						return false, nil, nil
					}
					current := mesh.DeepCopy()
					current.ResourceVersion += "-changed"
					return false, nil, k.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), current, mesh.Namespace)
				})
			case "boot", "previous-boot-stale-api":
				current := node.DeepCopy()
				current.Status.NodeInfo.BootID = "different"
				_, err := k.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
				must(err)
			case "agent":
				cfg.receiptPodUID = "different"
			case "checkpoint":
				must(os.WriteFile(filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock.neighbors.json"), []byte("{"), 0600))
			case "overlap":
				current := mesh.DeepCopy()
				current.Data[tunnel.PeerAllowedIPsPrefix+"joining"] = []byte("172.29.0.1/32")
				_, err := k.CoreV1().Secrets(mesh.Namespace).Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			case "missing-inventory":
				must(k.CoreV1().ConfigMaps(mesh.Namespace).Delete(ctx, index.Name, metav1.DeleteOptions{}))
			}
			calls := 0
			native := func(ctx context.Context, key wgtypes.Key, peers []tunnel.PeerSpec, own []string, retiring []tunnel.PeerSpec, allocated []string, commitRetirement func() error) error {
				calls++
				if key != site || len(peers) != 1 || peers[0].PublicKey != neighbor.PublicKey().String() {
					t.Fatal("native received a different identity or partition")
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".reconcile.lock.neighbors.json")); err != nil {
					t.Fatal("native ran before durable intent", err)
				}
				if lock, err := lockReconciliation(cfg); err == nil {
					lock.Close()
					t.Fatal("native ran without host lock")
				}
				switch fault {
				case "cluster-after":
					current, err := k.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.UID = "replacement"
					_, err = k.CoreV1().Namespaces().Update(ctx, current, metav1.UpdateOptions{})
					return err
				case "inventory-after":
					return tunnel.ReserveReceiveJournal(ctx, k.CoreV1().ConfigMaps(mesh.Namespace), store.ClusterUID, mesh, "new-reservation", store.OwnerReferences)
				case "mesh-after":
					return changeMesh()
				case "journal-after":
					current, err := k.CoreV1().ConfigMaps(mesh.Namespace).Get(ctx, journal.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					current.Annotations = map[string]string{"changed": "true"}
					_, err = k.CoreV1().ConfigMaps(mesh.Namespace).Update(ctx, current, metav1.UpdateOptions{})
					return err
				case "agent-after":
					current := pod.DeepCopy()
					current.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
					_, err := k.CoreV1().Pods(mesh.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
					return err
				case "native":
					return errors.New("native readback failed")
				}
				return nil
			}
			preparations := 0
			prepare := func(ctx context.Context, key wgtypes.Key, admitted *corev1.Secret, peers []tunnel.PeerSpec, plans []tunnel.ReceiveTransitionPlan, check func() error) error {
				preparations++
				if key != site || admitted.UID != mesh.UID || len(peers) != 1 || peers[0].PublicKey != neighbor.PublicKey().String() {
					t.Fatal("preparation received unadmitted peers")
				}
				if lock, err := lockReconciliation(cfg); err == nil {
					lock.Close()
					t.Fatal("preparation ran without host lock")
				}
				if fault == "prepare-after" {
					return changeMesh()
				}
				return check()
			}
			err := reconcileReceiveNeighbors(ctx, k, cfg, native, prepare)
			if fault == "valid" || fault == "complete" || fault == "previous-boot" {
				must(err)
				if fault == "previous-boot" {
					raw, err := os.ReadFile(checkpointPath)
					must(err)
					var checkpoint receiveNeighborCheckpoint
					must(json.Unmarshal(raw, &checkpoint))
					if checkpoint.BootID != node.Status.NodeInfo.BootID || len(checkpoint.Peers) != 1 {
						t.Fatal("live recovery did not replace old-boot intent with current projection")
					}
				}
				if calls != 1 || preparations != 1 {
					t.Fatal("valid neighbor not installed and prepared")
				}
				must(reconcileReceiveNeighbors(ctx, k, cfg, native, nil))
				// Shared-rule repair uses the same real-API-created inventory,
				// including a completed journal, but gets its route authority
				// from the current mesh instead of cached phase receipts.
				currentMesh, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
				must(err)
				currentMesh.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte(plan.RootPublicKey)
				currentMesh.Data[tunnel.PeerEndpointPrefix+"worker"] = []byte("198.51.100.1:51820")
				currentMesh.Data[tunnel.PeerAllowedIPsPrefix+"worker"] = []byte("172.29.0.1/32")
				currentMesh.Data[tunnel.PeerRouteHostsPrefix+"worker"] = []byte("")
				_, err = k.CoreV1().Secrets(mesh.Namespace).Update(ctx, currentMesh, metav1.UpdateOptions{})
				must(err)
				localStore := store
				localStore.ConfigMaps = k.CoreV1().ConfigMaps(mesh.Namespace)
				scope := receiveJournalScope{Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUID: plan.MachineUID}
				set, err := loadReceiveSet(ctx, k, localStore, scope, cfg.nodeName, site)
				must(err)
				view, err := set.Local[plan.MachineUID].Snapshot.Transition().View()
				must(err)
				for _, drift := range []bool{false, true} {
					calls := 0
					err := admitReceivePolicyRepair(ctx, k, cfg, site, plan, view, string(node.UID), set.Routes, func(groups []receiveAdvertisementGroup, check func() error) error {
						calls++
						if len(groups) != 2 {
							t.Fatal("policy repair omitted a published worker")
						}
						if drift {
							must(changeMesh())
						}
						return check()
					})
					if calls != 1 || (err != nil) != drift {
						t.Fatalf("policy admission calls=%d drift=%v error=%v", calls, drift, err)
					}
				}

				if fault == "complete" {
					// The cloud agent uses the adoption namespace and has no
					// site secret name. Exercise live revalidation with its
					// actual journal participant, not a site identity.
					cloudNode, err := api.CoreV1().Nodes().Get(ctx, plan.CloudNodeName, metav1.GetOptions{})
					must(err)
					_, err = k.CoreV1().Nodes().Create(ctx, cloudNode, metav1.CreateOptions{})
					must(err)
					cloudPod := pod.DeepCopy()
					cloudPod.Name, cloudPod.UID, cloudPod.Spec.NodeName = "cloud-agent", "cloud-pod", plan.CloudNodeName
					_, err = k.CoreV1().Pods(mesh.Namespace).Create(ctx, cloudPod, metav1.CreateOptions{})
					must(err)
					currentAdoption, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
					must(err)
					currentAdoption.Data = map[string][]byte{tunnel.CloudPeersKey: plan.Base}
					_, err = k.CoreV1().Secrets(mesh.Namespace).Update(ctx, currentAdoption, metav1.UpdateOptions{})
					must(err)
					cloudCfg := config{nodeName: plan.CloudNodeName, peersSecretNamespace: mesh.Namespace, receiptMeshName: mesh.Name, receiptPodName: cloudPod.Name, receiptPodUID: string(cloudPod.UID)}
					cloudSet, err := loadReceiveSet(ctx, k, localStore, scope, cloudCfg.nodeName, root)
					must(err)
					for _, drift := range []bool{false, true} {
						calls := 0
						err := admitReceivePolicyRepair(ctx, k, cloudCfg, root, plan, view, string(cloudNode.UID), cloudSet.Routes, func(groups []receiveAdvertisementGroup, check func() error) error {
							calls++
							if len(groups) == 0 || !groups[0].legacy {
								t.Fatal("cloud terminal rollback projection missing")
							}
							if drift {
								changed := currentAdoption.DeepCopy()
								changed.ResourceVersion += "-changed"
								_, err := k.CoreV1().Secrets(mesh.Namespace).Update(ctx, changed, metav1.UpdateOptions{})
								must(err)
							}
							return check()
						})
						if calls != 1 || (err != nil) != drift {
							t.Fatalf("cloud policy calls=%d drift=%v error=%v", calls, drift, err)
						}
					}
				}
			} else {
				if err == nil {
					t.Fatal("unsafe neighbor admission succeeded")
				}
				if fault == "previous-boot-stale-api" || fault == "empty-boot" {
					current, err := os.ReadFile(checkpointPath)
					must(err)
					if !bytes.Equal(current, oldCheckpoint) {
						t.Fatal("failed boot admission replaced neighbor checkpoint")
					}
				}
				want := 0
				if fault == "mesh-after" || fault == "journal-after" || fault == "agent-after" || fault == "cluster-after" || fault == "inventory-after" || fault == "native" || fault == "prepare-after" {
					want = 1
				}
				if fault != "prepare-after" && preparations != 0 {
					t.Fatal("failed admission reached preparation")
				}
				if calls != want {
					t.Fatalf("native calls=%d, want %d: %v", calls, want, err)
				}
				if fault == "native" {
					// The failed native operation must not discard the previous
					// owner, including across another reconciliation attempt.
					for attempt := 0; attempt < 2; attempt++ {
						raw, err := os.ReadFile(checkpointPath + ".retiring")
						must(err)
						var saved, prior receiveNeighborCheckpoint
						must(json.Unmarshal(raw, &saved))
						must(json.Unmarshal(oldCheckpoint, &prior))
						if saved.Version != 1 || len(saved.Retiring) != 2 || saved.Retiring[0].PublicKey != prior.Peers[0].PublicKey || saved.Retiring[1].PublicKey != cachedRetiredKey || len(saved.Peers) != 1 {
							t.Fatal("failed installation discarded old ownership")
						}
						mainRaw, err := os.ReadFile(checkpointPath)
						must(err)
						var original map[string]json.RawMessage
						must(json.Unmarshal(mainRaw, &original))
						if _, added := original["retiring"]; added || string(original["version"]) != "1" {
							t.Fatal("retirement changed the rollback binary's checkpoint format")
						}
						if attempt == 0 {
							must(os.Remove(siteForwardingCachePath(cfg)))
						}
						if err := reconcileReceiveNeighbors(ctx, k, cfg, native, nil); err == nil {
							t.Fatal("failed native retry accepted")
						}
					}
					for attempt := 0; attempt < 2; attempt++ {
						completeRetirement := func(ctx context.Context, key wgtypes.Key, peers []tunnel.PeerSpec, own []string, retiring []tunnel.PeerSpec, allocated []string, commit func() error) error {
							want := 2
							if attempt == 1 {
								want = 0
							}
							if len(retiring) != want {
								t.Fatalf("retirement replay after later installation failure: got %d, want %d", len(retiring), want)
							}
							must(commit())
							return errors.New("replacement installation failed")
						}
						if err := reconcileReceiveNeighbors(ctx, k, cfg, completeRetirement, nil); err == nil {
							t.Fatal("failed replacement installation accepted")
						}
					}
				}
			}
		})
	}
}
