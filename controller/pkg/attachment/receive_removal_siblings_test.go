package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Two expired histories share a captured mesh. Real API updates exercise both
// serial publication orders. Lifecycle authorization is simulated. No consumer
// is removed and no native cleanup, authenticated completion or release is proved.
func verifySiblingReceiveRemoval(t *testing.T, api kubernetes.Interface) {
	for _, order := range [][]int{{0, 1}, {1, 0}} {
		t.Run(fmt.Sprintf("sibling-removal/%d-first", order[0]), func(t *testing.T) {
			ctx := context.Background()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ns := fmt.Sprintf("sibling-%d", order[0])
			_, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			must(err)
			mesh, err := api.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
			must(err)
			maps := api.CoreV1().ConfigMaps(ns)
			intents := []RemoteWithdrawalIntent{}
			stores := []handover.Store{}
			plans := []tunnel.ReceiveTransitionPlan{}
			inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: ns, MeshName: "mesh", MeshUID: string(mesh.UID), Publications: map[string]tunnel.ReceiveJournalPublication{}}
			mesh.Data = map[string][]byte{}
			for _, seed := range []byte{4, 6} {
				w, fixture, _, _ := retirementFixtureForConsumer(t, "interrupted-switch", false, string(mesh.UID), ns, "site", "site-uid", seed)
				plan, err := tunnel.DecodeReceiveTransition(fixture, "cluster")
				must(err)
				plans = append(plans, plan)
				raw, err := fixture.Checkpoint()
				must(err)
				var cp struct {
					Object corev1.ConfigMap `json:"object"`
				}
				must(json.Unmarshal(raw, &cp))
				cp.Object.UID = ""
				cp.Object.ResourceVersion = ""
				journal, err := maps.Create(ctx, &cp.Object, metav1.CreateOptions{})
				must(err)
				stores = append(stores, handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)})
				inventory.MachineUIDs = append(inventory.MachineUIDs, w.MachineUID)
				inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
				mesh.Data[tunnel.NodePublicKeyPrefix+"site"] = []byte(w.Consumers[0].PublicKey)
				mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte(w.PublicKey)
				mesh.Data[tunnel.PeerEndpointPrefix+w.MachineName] = []byte(fmt.Sprintf("198.51.100.%d:51820", seed))
				mesh.Data[tunnel.PeerAllowedIPsPrefix+w.MachineName] = []byte(fmt.Sprintf("172.29.0.%d/32", seed))
				mesh.Data[tunnel.PeerRouteHostsPrefix+w.MachineName] = []byte(fmt.Sprintf("172.29.0.%d", seed))
				intents = append(intents, w)
			}
			mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
			must(err)
			for i := range intents {
				other := plans[1-i]
				intents[i].SourceVersion = mesh.ResourceVersion
				intents[i].SourceDataHash = withdrawalSourceDataHash(mesh.Data)
				intents[i].Consumers = append(intents[i].Consumers, ConsumerTarget{NodeName: other.CloudNodeName, NodeUID: other.CloudNodeUID, MachineName: other.MachineName, SecretUID: other.AdoptionUID, PublicKey: other.RootPublicKey, TunnelAddress: fmt.Sprintf("172.29.0.%d", []byte{4, 6}[1-i])})
			}
			originalIntents, err := json.Marshal(intents)
			must(err)
			raw, err := json.Marshal(inventory)
			must(err)
			index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID))}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
			must(err)
			advance := func(i int) error {
				current, err := api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
				if err != nil {
					return err
				}
				idx, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				ready, err := AdvanceReceiveRemoval(ctx, api, stores[i], idx.ObjectMeta, current, intents[i], plans[i].CloudNodeName, plans[i].CloudNodeUID, time.Minute, func(_ context.Context, w RemoteWithdrawalIntent) error {
					if !reflect.DeepEqual(w, intents[i]) {
						return fmt.Errorf("captured lifecycle authority changed")
					}
					return nil
				})
				if ready {
					t.Fatal("sibling retirement succeeded without consumer observations")
				}
				return err
			}
			first, second := order[0], order[1]
			for step := 0; step < 2; step++ {
				must(advance(first))
			}
			// Corrupt a field inside the allowed worker scope. Publication must check
			// the recorded values against the still-published peer, not just their keys.
			idx, err := maps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			originalIndexData := idx.Data["inventory.json"]
			var damaged tunnel.ReceiveInventory
			must(json.Unmarshal([]byte(originalIndexData), &damaged))
			pub := damaged.Publications[intents[first].MachineUID]
			rec, err := decodeReceiveWithdrawalRecord(pub.Retirement, intents[first].MachineUID)
			must(err)
			rec.PeerData[tunnel.PeerEndpointPrefix+intents[first].MachineName] = []byte("198.51.100.99:51820")
			pub.Retirement, err = json.Marshal(rec)
			must(err)
			damaged.Publications[intents[first].MachineUID] = pub
			changed, err := json.Marshal(damaged)
			must(err)
			idx.Data["inventory.json"] = string(changed)
			_, err = maps.Update(ctx, idx, metav1.UpdateOptions{})
			must(err)
			if err := advance(first); err == nil {
				t.Fatal("publication accepted changed captured peer fields")
			}
			stillPublished, err := api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
			must(err)
			if withdrawalSourceDataHash(stillPublished.Data) != intents[first].SourceDataHash {
				t.Fatal("rejected publication changed the mesh")
			}
			idx, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
			must(err)
			idx.Data["inventory.json"] = originalIndexData
			_, err = maps.Update(ctx, idx, metav1.UpdateOptions{})
			must(err)
			must(advance(first))
			mesh, err = api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
			must(err)
			if len(mesh.Data[tunnel.PeerPublicKeyPrefix+intents[first].MachineName]) != 0 {
				t.Fatal("first withdrawal not published")
			}
			original := mesh.DeepCopy()
			for _, fault := range []string{"unrelated-data", "partial-peer", "replacement-key"} {
				current, err := api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
				must(err)
				switch fault {
				case "unrelated-data":
					current.Data["unexpected-source-change"] = []byte("changed")
				case "partial-peer":
					current.Data[tunnel.PeerEndpointPrefix+intents[first].MachineName] = []byte("198.51.100.9:51820")
				case "replacement-key":
					current.Data[tunnel.PeerPublicKeyPrefix+intents[first].MachineName] = []byte(intents[second].PublicKey)
				}
				_, err = api.CoreV1().Secrets(ns).Update(ctx, current, metav1.UpdateOptions{})
				must(err)
				if err := advance(second); err == nil {
					t.Fatal("unrelated source change accepted", fault)
				}
				untouched, err := stores[second].Load(ctx)
				must(err)
				if untouched.Frozen() {
					t.Fatal("invalid sibling source froze second journal", fault)
				}
				current, err = api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
				must(err)
				current.Data = original.DeepCopy().Data
				_, err = api.CoreV1().Secrets(ns).Update(ctx, current, metav1.UpdateOptions{})
				must(err)
			}
			for step := 0; step < 4; step++ {
				must(advance(second))
			}
			must(advance(first))
			live, _, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, "mesh", string(mesh.UID))
			must(err)
			if len(live.MachineUIDs) != 2 || len(live.Released) != 0 {
				t.Fatal("publication released reservations")
			}
			mesh, err = api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
			must(err)
			for i, w := range intents {
				publication := live.Publications[w.MachineUID]
				record, err := decodeReceiveWithdrawalRecord(publication.Retirement, w.MachineUID)
				must(err)
				if record.Version != 2 || !reflect.DeepEqual(record.Withdrawal, w) || len(record.PeerData) != 4 || len(publication.RetirementCompletion) != 0 || len(publication.RetirementFinalization) != 0 {
					t.Fatal("capture changed or cleanup evidence invented")
				}
				if len(mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName]) != 0 {
					t.Fatal("second withdrawal not published")
				}
				source, err := ReadReceiveRetirementSet(ctx, api, stores[i], ns, "mesh", string(mesh.UID), w.MachineUID)
				must(err)
				if len(source.Routes.Serving.Plans) != 0 || len(source.Routes.Reserved.Plans) != 2 || len(source.Withdrawal.Consumers) != 2 {
					t.Fatal("lost retained sibling obligations")
				}
			}
			after, err := json.Marshal(intents)
			must(err)
			if string(after) != string(originalIntents) {
				t.Fatal("source validation mutated input captures")
			}
		})
	}
}
