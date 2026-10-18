package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Reuse the dispatch fixture's admitted cloud identity and journal. Native
// callbacks are simulated; this does not establish packet or reboot continuity.
func verifyCloudNeighborAdmission(t *testing.T, objects []runtime.Object, plan tunnel.ReceiveTransitionPlan, root wgtypes.Key, inventoryName string) {
	t.Helper()
	verifyCloudLegacyImport(t, plan)
	for _, fault := range []string{"valid", "allocation", "overlap", "inventory", "mesh-race", "agent-race", "legacy-before-retirement", "legacy-after-retirement"} {
		t.Run("cloud-neighbor-"+fault, func(t *testing.T) {
			plan := plan
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			neighbor, err := wgtypes.GeneratePrivateKey()
			must(err)
			copies := make([]runtime.Object, 0, len(objects)+1)
			for _, object := range objects {
				copies = append(copies, object.DeepCopyObject())
			}
			legacy := strings.HasPrefix(fault, "legacy-")
			var old tunnel.PeerSpec
			if legacy {
				oldKey, err := wgtypes.GeneratePrivateKey()
				must(err)
				old = tunnel.PeerSpec{PublicKey: oldKey.PublicKey().String(), Remote: true, Endpoint: "198.51.100.99:51820", WGAllowedIPs: []string{"172.29.0.2/32", "10.244.8.0/26"}, RouteHosts: []string{"172.29.0.2"}}
				var base tunnel.PeerListDoc
				must(json.Unmarshal(plan.Base, &base))
				base.Peers = append(base.Peers, old)
				plan.Base, err = json.Marshal(base)
				must(err)
				plan.Desired.Peers = append(append([]tunnel.PeerSpec{}, plan.Desired.Peers...), old)
				var clusterUID string
				for _, object := range copies {
					if ns, ok := object.(*corev1.Namespace); ok && ns.Name == "kube-system" {
						clusterUID = string(ns.UID)
					}
				}
				raw, err := json.Marshal(plan)
				must(err)
				spec, err := tunnel.ReceiveTransitionSpec(plan, clusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
				must(err)
				tr, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
				must(err)
				encoded, err := tr.Bytes()
				must(err)
				for _, object := range copies {
					if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == tunnel.ReceiveTransitionName(plan.MachineUID) {
						cm.Data["transition.json"], cm.Data["plan.json"] = string(encoded), string(raw)
					}
				}
			}
			for _, object := range copies {
				if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == inventoryName {
					var inventory tunnel.ReceiveInventory
					must(json.Unmarshal([]byte(cm.Data["inventory.json"]), &inventory))
					inventory.Publications = map[string]tunnel.ReceiveJournalPublication{plan.MachineUID: {JournalUID: "dispatch-journal"}}
					raw, err := json.Marshal(inventory)
					must(err)
					cm.Data["inventory.json"] = string(raw)
					if fault == "inventory" {
						cm.Name = "unrelated-inventory"
					}
				}
				if mesh, ok := object.(*corev1.Secret); ok && mesh.Name == plan.MeshName {
					mesh.Data[tunnel.PeerPublicKeyPrefix+"self"] = []byte(root.PublicKey().String())
					mesh.Data[tunnel.PeerRouteHostsPrefix+"self"] = []byte("172.29.0.1")
					mesh.Data[tunnel.PeerAllowedIPsPrefix+"self"] = []byte("172.29.0.1/32")
					mesh.Data[tunnel.PeerPublicKeyPrefix+"other"] = []byte(neighbor.PublicKey().String())
					mesh.Data[tunnel.PeerEndpointPrefix+"other"] = []byte("198.51.100.2:51820")
					mesh.Data[tunnel.PeerRouteHostsPrefix+"other"] = []byte("172.29.0.2")
					mesh.Data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.29.0.2/32,10.244.8.0/26")
					if fault == "overlap" {
						mesh.Data[tunnel.PeerAllowedIPsPrefix+"other"] = []byte("172.29.0.2/32,10.90.0.0/24")
					}
				}
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: plan.Namespace, Name: "cloud-neighbor-agent", UID: "cloud-neighbor-pod"}, Spec: corev1.PodSpec{NodeName: plan.CloudNodeName}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://initial", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
			copies = append(copies, pod)
			api := fake.NewClientset(copies...)
			cfg := config{iface: "wgcloudnbrs", nodeName: plan.CloudNodeName, peersSecretNamespace: plan.Namespace, receiptMeshName: plan.MeshName, receiptPodName: pod.Name, receiptPodUID: string(pod.UID), peersFile: filepath.Join(t.TempDir(), "identity.json")}
			allocation := plan.CloudAddresses[0]
			if fault == "allocation" {
				allocation = "172.29.0.99/32"
			}
			raw, err := json.Marshal(tunnel.PeersFileDoc{PrivateKey: root.String(), LocalAddress: allocation})
			must(err)
			must(os.WriteFile(cfg.peersFile, raw, 0600))
			reads := 0
			api.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.(k8stesting.GetAction).GetName() == plan.MeshName {
					reads++
					if fault == "mesh-race" && reads > 1 {
						obj, err := api.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), plan.Namespace, plan.MeshName)
						if err != nil {
							return true, nil, err
						}
						copy := obj.(*corev1.Secret).DeepCopy()
						copy.ResourceVersion += "-changed"
						return true, copy, nil
					}
				}
				return false, nil, nil
			})
			called := false
			calls := 0
			retired := false
			native := func(_ context.Context, key wgtypes.Key, peers []tunnel.PeerSpec, own []string, retiring []tunnel.PeerSpec, allocated []string, commit func() error) error {
				called = true
				calls++
				markers, err := filepath.Glob(filepath.Join(filepath.Dir(cfg.peersFile), "*.cloud-legacy-*.json"))
				must(err)
				if len(markers) != 1 {
					t.Fatal("native work preceded durable legacy import")
				}
				wantRetiring := 0
				if legacy && !retired {
					wantRetiring = 1
				}
				if len(retiring) != wantRetiring || (wantRetiring == 1 && !reflect.DeepEqual(retiring[0], old)) {
					t.Fatalf("lost or replayed legacy retirement: %+v", retiring)
				}
				if key != root || len(peers) != 1 || peers[0].PublicKey != neighbor.PublicKey().String() || !peers[0].Remote || len(own) != 0 || !reflect.DeepEqual(allocated, plan.CloudAddresses) {
					t.Fatal("cloud neighbor projection included site transport or lost cloud allocation")
				}
				if !reflect.DeepEqual(peers[0].WGAllowedIPs, []string{"172.29.0.2/32", "10.244.8.0/26"}) {
					t.Fatal("cloud pod permission omitted")
				}
				if fault == "agent-race" {
					changed := pod.DeepCopy()
					changed.UID = "replacement"
					must(api.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, plan.Namespace))
				}
				if legacy {
					if calls == 1 && fault == "legacy-before-retirement" {
						return errors.New("native retirement interrupted")
					}
					must(commit())
					retired = true
					if calls == 1 {
						return errors.New("native installation interrupted after retirement")
					}
					return nil
				}
				return commit()
			}
			prepare := func(context.Context, wgtypes.Key, *corev1.Secret, []tunnel.PeerSpec, []tunnel.ReceiveTransitionPlan, func() error) error {
				t.Fatal("cloud edge entered site receiver preparation")
				return nil
			}
			err = reconcileReceiveNeighbors(ctx, api, cfg, native, prepare)
			if legacy {
				if err == nil || calls != 1 {
					t.Fatalf("did not reach injected native interruption: %v", err)
				}
				must(reconcileReceiveNeighbors(ctx, api, cfg, native, prepare))
				must(reconcileReceiveNeighbors(ctx, api, cfg, native, prepare))
				if calls != 3 || !retired {
					t.Fatal("legacy retirement did not recover")
				}
			} else if fault == "valid" {
				must(err)
				if !called {
					t.Fatal("reserved cloud worker skipped ordinary peer reconciliation")
				}
			} else {
				if err == nil {
					t.Fatal("invalid cloud neighbor admission accepted")
				}
				if fault != "agent-race" && called {
					t.Fatal("native callback entered before admission")
				}
			}
		})
	}
}
