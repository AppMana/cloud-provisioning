package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// Copy the persisted withdrawal fixture into a fake API for dispatch. Real TLS
// delivery is exercised, but the token and native callback are synthetic. No
// kernel absence, token authentication or controller completion is proved here.
// A retained keyless follower must be discovered without receiver membership.
func verifyReceiveRetirementDispatch(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, key wgtypes.Key, pod *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := api.CoreV1().Nodes().Get(ctx, "site", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	maps, err := store.ConfigMaps.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"none", "follower", "missing-backend", "native-error", "container-race", "delivery-failure", "later-api-error"} {
		t.Run("retirement-dispatch/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			currentMesh, currentNode, currentPod := mesh.DeepCopy(), node.DeepCopy(), pod.DeepCopy()
			objects := []runtime.Object{}
			for _, cm := range maps.Items {
				objects = append(objects, cm.DeepCopy())
			}
			if fault == "later-api-error" {
				// A second discovered target fails its Pod read after the first
				// target has already rejected a missing retirement backend.
				// This fixture exercises dispatch only, not route admission.
				var extra *corev1.ConfigMap
				for _, object := range objects {
					cm := object.(*corev1.ConfigMap)
					if cm.Name != store.Name {
						continue
					}
					extra = cm.DeepCopy()
				}
				if extra == nil {
					t.Fatal("missing source journal")
				}
				var plan tunnel.ReceiveTransitionPlan
				must(json.Unmarshal([]byte(extra.Data["plan.json"]), &plan))
				plan.MachineUID = scope.MachineUID + "z"
				raw, e := json.Marshal(plan)
				must(e)
				spec, e := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
				must(e)
				transition, e := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
				must(e)
				encoded, e := transition.Bytes()
				must(e)
				extra.Name = tunnel.ReceiveTransitionName(plan.MachineUID)
				extra.UID = "later-journal"
				extra.Data = map[string]string{"plan.json": string(raw), "transition.json": string(encoded)}
				for _, object := range objects {
					cm := object.(*corev1.ConfigMap)
					if cm.Name != tunnel.ReceiveInventoryName(scope.MeshUID) {
						continue
					}
					var inventory tunnel.ReceiveInventory
					must(json.Unmarshal([]byte(cm.Data["inventory.json"]), &inventory))
					inventory.MachineUIDs = append(inventory.MachineUIDs, plan.MachineUID)
					inventory.Publications[plan.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(extra.UID)}
					encoded, e := json.Marshal(inventory)
					must(e)
					cm.Data["inventory.json"] = string(encoded)
				}
				objects = append(objects, extra)
			}
			if fault == "follower" {
				currentNode.Name = "follower"
				currentNode.UID = "follower"
				currentPod.Spec.NodeName = currentNode.Name
				currentPod.UID = "follower-pod"
				currentMesh.Data[tunnel.SiteAddressesPrefix+currentNode.Name] = []byte("10.90.0.8")
				for _, object := range objects {
					cm := object.(*corev1.ConfigMap)
					if cm.Name != tunnel.ReceiveInventoryName(scope.MeshUID) {
						continue
					}
					var inventory tunnel.ReceiveInventory
					must(json.Unmarshal([]byte(cm.Data["inventory.json"]), &inventory))
					publication := inventory.Publications[scope.MachineUID]
					var record map[string]json.RawMessage
					must(json.Unmarshal(publication.Retirement, &record))
					var intent attachment.RemoteWithdrawalIntent
					must(json.Unmarshal(record["withdrawal"], &intent))
					intent.Consumers = append(intent.Consumers, attachment.ConsumerTarget{Site: true, NodeName: currentNode.Name, NodeUID: string(currentNode.UID)})
					record["withdrawal"], err = json.Marshal(intent)
					must(err)
					publication.Retirement, err = json.Marshal(record)
					must(err)
					inventory.Publications[scope.MachineUID] = publication
					raw, e := json.Marshal(inventory)
					must(e)
					cm.Data["inventory.json"] = string(raw)
				}
				objects = append(objects, node.DeepCopy())
			}
			objects = append(objects, currentMesh, currentNode, currentPod, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(store.ClusterUID)}})
			client := fake.NewClientset(objects...)
			laterError := errors.New("later Pod read failed")
			if fault == "later-api-error" {
				reads := 0
				client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					reads++
					if reads == 2 {
						return true, nil, laterError
					}
					return false, nil, nil
				})
			}
			delivered := make(chan handover.ReceiptRequest, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing projected credential")
				}
				var request handover.ReceiptRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				delivered <- request
				if fault == "delivery-failure" {
					w.WriteHeader(http.StatusConflict)
				} else {
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			cfg := config{iface: "retiredispatch", nodeName: currentNode.Name, secretNamespace: scope.Namespace, secretName: scope.MeshName, privateKeyFile: filepath.Join(dir, "key"), receiptURL: server.URL, receiptCAFile: filepath.Join(dir, "ca"), receiptTokenFile: filepath.Join(dir, "token"), receiptMeshName: scope.MeshName, receiptPodName: currentPod.Name, receiptPodUID: string(currentPod.UID)}
			must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
			must(os.WriteFile(cfg.receiptTokenFile, []byte("fixture-token"), 0600))
			must(os.WriteFile(cfg.receiptCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
			calls, phaseCalls := 0, 0
			phase := func(context.Context, tunnel.ReceiveTransitionPlan, handover.View, string, tunnel.ReceiveJournalRoutes, wgtypes.Key) error {
				phaseCalls++
				return fmt.Errorf("retirement fell through to serving phase")
			}
			native := receiveDispatchRetirement(func(ctx context.Context, s handover.Store, gotScope receiveJournalScope, gotKey wgtypes.Key, lockPath string, a admittedReceiveRetirement) error {
				calls++
				if s.Name != store.Name || gotScope != scope || gotKey != key || a.NodeUID != string(currentNode.UID) {
					return fmt.Errorf("retirement dispatch lost identity")
				}
				if _, err := os.Stat(lockPath + ".inventory.json"); err != nil {
					return err
				}
				if fault == "native-error" {
					return fmt.Errorf("incomplete native cleanup")
				}
				if fault == "container-race" {
					p, err := client.CoreV1().Pods(scope.Namespace).Get(ctx, currentPod.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					p.Status.ContainerStatuses[0].ContainerID = "replaced"
					_, err = client.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
					return err
				}
				return nil
			})
			if fault == "missing-backend" || fault == "later-api-error" {
				native = nil
			}
			handled, err := dispatchReceivePhases(ctx, client, cfg, phase, phase, native)
			if fault == "later-api-error" && (!errors.Is(err, laterError) || !strings.Contains(err.Error(), "retirement requires a complete native cleanup backend")) {
				t.Fatalf("lost an earlier dispatch failure: %v", err)
			}
			success := fault == "none" || fault == "follower"
			if !handled || (err == nil) != success || phaseCalls != 0 {
				t.Fatal("wrong dispatch outcome", handled, err, phaseCalls)
			}
			expectedCalls := 1
			if fault == "missing-backend" || fault == "later-api-error" {
				expectedCalls = 0
			}
			if calls != expectedCalls {
				t.Fatal("native path not exercised", calls, err)
			}
			expectDelivery := success || fault == "delivery-failure"
			select {
			case receipt := <-delivered:
				if !expectDelivery || receipt.MachineUID != scope.MachineUID || len(receipt.Receipts) != 1 || receipt.Receipts[0].BearerToken != "" || receipt.Receipts[0].Receipt.DeviceID != "" {
					t.Fatal("invalid delivery")
				}
			default:
				if expectDelivery {
					t.Fatal("missing receipt delivery")
				}
			}
		})
	}
}
