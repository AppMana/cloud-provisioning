package main

import (
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
func verifyReceiveNeighborAdmission(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, plan tunnel.ReceiveTransitionPlan, site wgtypes.Key) {
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
	for _, fault := range []string{"valid", "complete", "mesh-before", "mesh-after", "journal-after", "cluster-after", "inventory-after", "boot", "agent", "agent-after", "checkpoint", "overlap", "missing-inventory", "native"} {
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
			case "boot":
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
			native := func(ctx context.Context, key wgtypes.Key, peers []tunnel.PeerSpec, own []string) error {
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
			err := reconcileReceiveNeighbors(ctx, k, cfg, native)
			if fault == "valid" || fault == "complete" {
				must(err)
				if calls != 1 {
					t.Fatal("valid neighbor not installed")
				}
				must(reconcileReceiveNeighbors(ctx, k, cfg, native))
			} else {
				if err == nil {
					t.Fatal("unsafe neighbor admission succeeded")
				}
				want := 0
				if fault == "mesh-after" || fault == "journal-after" || fault == "agent-after" || fault == "cluster-after" || fault == "inventory-after" || fault == "native" {
					want = 1
				}
				if calls != want {
					t.Fatalf("native calls=%d, want %d: %v", calls, want, err)
				}
			}
		})
	}
}
