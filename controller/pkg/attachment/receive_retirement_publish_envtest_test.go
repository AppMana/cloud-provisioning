package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type lostWithdrawalAPI struct{ kubernetes.Interface }
type lostWithdrawalCore struct{ typedcore.CoreV1Interface }
type lostWithdrawalSecrets struct{ typedcore.SecretInterface }

func (a lostWithdrawalAPI) CoreV1() typedcore.CoreV1Interface {
	return lostWithdrawalCore{a.Interface.CoreV1()}
}
func (a lostWithdrawalCore) Secrets(ns string) typedcore.SecretInterface {
	return lostWithdrawalSecrets{a.CoreV1Interface.Secrets(ns)}
}
func (a lostWithdrawalSecrets) Update(ctx context.Context, s *corev1.Secret, o metav1.UpdateOptions) (*corev1.Secret, error) {
	result, err := a.SecretInterface.Update(ctx, s, o)
	if err == nil {
		return nil, errors.New("injected lost publication response")
	}
	return result, err
}

// Isolated API/etcd publication and retry tests. Lifecycle authorization and
// terminal history are synthetic: no actual drain, provider deletion, native
// cleanup, workload interruption bound or reservation release is established.
func TestReceiveWithdrawalPublicationWithRealAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires isolated API assets")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"none", "metadata", "denied", "missing-authority", "wrong-index", "missing-reserved-journal", "mesh-race", "inventory-race", "lost-response"} {
		t.Run(fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			ns := "publish-" + fault
			_, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			must(err)
			mesh, err := api.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
			must(err)
			w, fixture, inventory, _ := retirementFixtureForConsumer(t, "forward", true, string(mesh.UID), ns, "site", "site-uid")
			mesh.Data = map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(w.Consumers[0].PublicKey), tunnel.PeerPublicKeyPrefix + w.MachineName: []byte(w.PublicKey), tunnel.PeerEndpointPrefix + w.MachineName: []byte("198.51.100.1:51820"), tunnel.PeerAllowedIPsPrefix + w.MachineName: []byte("172.29.0.1/32"), tunnel.PeerRouteHostsPrefix + w.MachineName: []byte("172.29.0.1")}
			mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
			must(err)
			w.SourceVersion = mesh.ResourceVersion
			if fault == "metadata" {
				w.SourceDataHash = withdrawalSourceDataHash(mesh.Data)
			}
			raw, err := fixture.Checkpoint()
			must(err)
			var checkpoint struct {
				Object corev1.ConfigMap `json:"object"`
			}
			must(json.Unmarshal(raw, &checkpoint))
			checkpoint.Object.UID, checkpoint.Object.ResourceVersion = "", ""
			maps := api.CoreV1().ConfigMaps(ns)
			journal, err := maps.Create(ctx, &checkpoint.Object, metav1.CreateOptions{})
			must(err)
			store := handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)}
			snapshot, err := store.Load(ctx)
			must(err)
			inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
			raw, err = json.Marshal(inventory)
			must(err)
			index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(w.MeshUID)}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
			must(err)
			must(w.ReserveReceiveWithdrawal(ctx, maps, "cluster", string(index.UID), mesh, snapshot))
			prepared, err := ReadPreparedReceiveRetirementSet(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID)
			must(err)
			if len(prepared.Routes.Reserved.Plans) != 1 || len(prepared.Routes.Serving.Plans) != 1 {
				t.Fatal("unpublished withdrawal lost serving intent or ownership")
			}
			if fault == "metadata" {
				mesh.Labels = map[string]string{"metadata": "after-reservation"}
				mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
				must(err)
			}
			if fault == "missing-reserved-journal" {
				must(tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, "other-machine", nil))
			}
			calls := 0
			authorize := func(ctx context.Context, got RemoteWithdrawalIntent) error {
				calls++
				if got.MachineUID != w.MachineUID || got.DrainIntent != w.DrainIntent {
					return errors.New("wrong lifecycle binding")
				}
				switch fault {
				case "denied":
					return errors.New("drain denied")
				case "mesh-race":
					changed := mesh.DeepCopy()
					changed.Labels = map[string]string{"changed": "true"}
					_, err := api.CoreV1().Secrets(ns).Update(ctx, changed, metav1.UpdateOptions{})
					return err
				case "inventory-race":
					return tunnel.ReserveReceiveJournal(ctx, maps, "cluster", mesh, "other-machine", nil)
				}
				return nil
			}
			if fault == "missing-authority" {
				authorize = nil
			}
			expectedUID := string(index.UID)
			if fault == "wrong-index" {
				expectedUID = "replacement"
			}
			var publishingAPI kubernetes.Interface = api
			if fault == "lost-response" {
				publishingAPI = lostWithdrawalAPI{api}
			}
			result, err := PublishReceiveWithdrawal(ctx, publishingAPI, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, expectedUID, authorize)
			if fault == "none" || fault == "metadata" {
				must(err)
				if result == nil {
					t.Fatal("missing publication")
				}
			} else if err == nil {
				t.Fatal("invalid or ambiguous publication accepted")
			}
			current, e := api.CoreV1().Secrets(ns).Get(ctx, mesh.Name, metav1.GetOptions{})
			must(e)
			removed := fault == "none" || fault == "metadata" || fault == "lost-response"
			if (len(current.Data[tunnel.PeerPublicKeyPrefix+w.MachineName]) == 0) != removed {
				t.Fatal("unexpected peer mutation")
			}
			if string(current.Data[tunnel.NodePublicKeyPrefix+"site"]) != w.Consumers[0].PublicKey {
				t.Fatal("surviving endpoint changed")
			}
			if fault != "missing-authority" && fault != "wrong-index" && fault != "missing-reserved-journal" && calls != 1 {
				t.Fatalf("authorization was not reached: %d, %v", calls, err)
			}
			if removed {
				withdrawn, err := ReadReceiveRetirementSet(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID)
				must(err)
				if len(withdrawn.Routes.Reserved.Plans) != 1 || len(withdrawn.Routes.Serving.Plans) != 0 {
					t.Fatal("published withdrawal retained serving intent or released ownership")
				}
				before := current.ResourceVersion
				retried, err := PublishReceiveWithdrawal(ctx, api, store, ns, mesh.Name, string(mesh.UID), w.MachineUID, string(index.UID), authorize)
				must(err)
				if retried.ResourceVersion != before {
					t.Fatal("already-published retry rewrote mesh")
				}
				stored, _, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, mesh.Name, string(mesh.UID))
				must(err)
				if len(stored.MachineUIDs) != 1 || len(stored.Publications[w.MachineUID].Retirement) == 0 || len(stored.Publications[w.MachineUID].RetirementCompletion) != 0 {
					t.Fatal("publication released reservation or fabricated completion")
				}
			}
		})
	}
}
