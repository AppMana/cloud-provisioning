package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
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
)

// This exercises dispatch, host fencing and HTTPS delivery with a simulated
// native verifier. Packet continuity and real token authentication have their
// own tests; a callback here is not evidence of native completion.
func verifyReceiveDispatch(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, original tunnel.ReceiveTransitionPlan, root wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	data, _ := json.Marshal(original)
	var plan tunnel.ReceiveTransitionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	plan.CloudAddresses = []string{"172.29.0.1/32"}
	plan.SiteDesired.Peers[0].RouteHosts = []string{"172.29.0.1"}
	mesh = mesh.DeepCopy()
	for public, site := range plan.Sites {
		site.RouteSources = []string{"10.90.0.2"}
		site.Legacy.Sources = append(site.Legacy.Sources, "10.90.0.2/32")
		forward := *site.Legacy
		site.Forward = &forward
		var err error
		site.Client, err = tunnel.PlanSiteReceiveClient(plan.MeshUID, public, plan.RootPublicKey, plan.Bindings[public], plan.SiteDesired, plan.SiteDesired)
		if err != nil {
			t.Fatal(err)
		}
		plan.Sites[public] = site
		mesh.Data[tunnel.NodeAddressesPrefix+site.NodeName] = []byte("10.90.0.2")
	}
	raw, _ := json.Marshal(plan)
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := tr.Bytes()
	_, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		t.Fatal(err)
	}
	adoption, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := api.CoreV1().Nodes().Get(ctx, plan.CloudNodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	objects := []runtime.Object{mesh, index, adoption, node, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(store.ClusterUID)}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: mesh.Namespace, Name: store.Name, UID: "dispatch-journal", ResourceVersion: "1"}, Data: map[string]string{"transition.json": string(encoded), "plan.json": string(raw)}}}
	for _, fault := range []string{"valid", "allocation", "pod", "credential", "native", "complete", "complete-native", "publication-pending", "publication-replaced"} {
		t.Run("dispatch-"+fault, func(t *testing.T) {
			copies := []runtime.Object{}
			for _, obj := range objects {
				copies = append(copies, obj.DeepCopyObject())
			}
			for _, obj := range copies {
				cm, ok := obj.(*corev1.ConfigMap)
				if !ok || cm.Name != index.Name {
					continue
				}
				var inventory tunnel.ReceiveInventory
				if err := json.Unmarshal([]byte(cm.Data["inventory.json"]), &inventory); err != nil {
					t.Fatal(err)
				}
				publication := tunnel.ReceiveJournalPublication{JournalUID: "dispatch-journal"}
				if fault == "publication-pending" {
					publication = tunnel.ReceiveJournalPublication{Plan: raw}
				}
				if fault == "publication-replaced" {
					publication.JournalUID = "previous-journal"
				}
				inventory.Publications = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: publication}
				encoded, err := json.Marshal(inventory)
				if err != nil {
					t.Fatal(err)
				}
				cm.Data["inventory.json"] = string(encoded)
			}
			terminal := fault == "complete" || fault == "complete-native"
			if terminal {
				finished, err := tr.Rollback(time.Now())
				if err != nil {
					t.Fatal(err)
				}
				v, _ := finished.View()
				digest, _ := v.Round.Digest()
				var proofs []handover.AuthenticatedReceipt
				for _, m := range v.Round.Members {
					proofs = append(proofs, handover.AuthenticatedReceipt{AuthenticatedNodeUID: m.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: m.NodeUID, DeviceID: m.DeviceID, FromKey: m.FromKey, ToKey: m.ToKey, FromPeerKey: m.FromPeerKey, ToPeerKey: m.ToPeerKey, ObservedAt: time.Now()}})
				}
				finished, err = finished.Advance(time.Now(), proofs)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := finished.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				for _, obj := range copies {
					if journal, ok := obj.(*corev1.ConfigMap); ok && journal.Name == store.Name {
						journal.Data["transition.json"] = string(encoded)
					}
				}
			}
			copies = append(copies, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: mesh.Namespace, Name: "agent", UID: "pod"}, Spec: corev1.PodSpec{NodeName: plan.CloudNodeName}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://agent", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}})
			k := fake.NewClientset(copies...)
			delivered := make(chan handover.ReceiptRequest, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer projected-token" {
					t.Error("missing projected credential")
				}
				var request handover.ReceiptRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				delivered <- request
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			directory := t.TempDir()
			cfg := config{iface: "wgdispatch", nodeName: plan.CloudNodeName, peersSecretNamespace: mesh.Namespace, peersFile: filepath.Join(directory, "peers.json"), receiptURL: server.URL, receiptMeshName: mesh.Name, receiptCAFile: filepath.Join(directory, "ca.crt"), receiptTokenFile: filepath.Join(directory, "token"), receiptPodName: "agent", receiptPodUID: "pod"}
			allocation := plan.CloudAddresses[0]
			if fault == "allocation" {
				allocation = "172.29.0.2/32"
			}
			identity, _ := json.Marshal(tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: allocation})
			if err := os.WriteFile(cfg.peersFile, identity, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.receiptCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			if fault != "credential" {
				if err := os.WriteFile(cfg.receiptTokenFile, []byte("projected-token"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "pod" {
				cfg.receiptPodUID = "old-pod"
			}
			nativeCalls := 0
			handled, err := dispatchReceivePhases(ctx, k, cfg, func(_ context.Context, p tunnel.ReceiveTransitionPlan, v handover.View, uid string, intent tunnel.ReceiveJournalRoutes, key wgtypes.Key) error {
				nativeCalls++
				if lock, err := lockReconciliation(cfg); err == nil {
					lock.Close()
					t.Error("native execution escaped shared host lock")
				}
				phase := handover.Prepare
				if terminal {
					phase = handover.Abort
				}
				if uid != plan.CloudNodeUID || key != root || v.Round.Phase != phase || v.Complete != terminal {
					t.Error("native dispatch changed admitted identity")
				}
				if fault == "native" || fault == "complete-native" {
					return fmt.Errorf("native observation failed")
				}
				return nil
			})
			if !handled || (err == nil) != (fault == "valid" || fault == "complete") {
				t.Fatalf("handled=%v error=%v", handled, err)
			}
			if fault == "valid" {
				if nativeCalls != 1 {
					t.Fatal("native phase did not execute exactly once")
				}
				select {
				case request := <-delivered:
					if request.MachineUID != plan.MachineUID || len(request.Receipts) != 1 || request.Receipts[0].BootID != node.Status.NodeInfo.BootID || request.Receipts[0].Receipt.AgentContainerID != "containerd://agent" {
						t.Fatal("receipt lost execution identity")
					}
				default:
					t.Fatal("verified native observation was not submitted")
				}
			} else {
				select {
				case <-delivered:
					t.Fatal("failed dispatch emitted receipts")
				default:
				}
				if terminal && nativeCalls != 1 {
					t.Fatal("completed journal was skipped instead of verified")
				}
				if fault != "native" && !terminal && nativeCalls != 0 {
					t.Fatal("invalid identity reached native mutation")
				}
			}
		})
	}
}
