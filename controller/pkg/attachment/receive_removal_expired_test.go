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

// Real API storage/publication with synthetic history and drain authorization.
// No native receipts are supplied: cleanup must remain incomplete and reserved.
func verifyExpiredReceiveRemoval(t *testing.T, api kubernetes.Interface) {
	for _, phase := range []string{"prepare", "switch", "drain", "retire", "abort"} {
		t.Run("expired-removal/"+phase, func(t *testing.T) {
			ctx := context.Background()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ns := "expired-" + phase
			_, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			must(err)
			mesh, err := api.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
			must(err)
			w, fixture, inventory, _ := retirementFixtureForConsumer(t, "interrupted-"+phase, false, string(mesh.UID), ns, "site", "site-uid")
			mesh.Data = map[string][]byte{tunnel.NodePublicKeyPrefix + "site": []byte(w.Consumers[0].PublicKey), tunnel.PeerPublicKeyPrefix + w.MachineName: []byte(w.PublicKey), tunnel.PeerEndpointPrefix + w.MachineName: []byte("198.51.100.1:51820"), tunnel.PeerAllowedIPsPrefix + w.MachineName: []byte("172.29.0.1/32"), tunnel.PeerRouteHostsPrefix + w.MachineName: []byte("172.29.0.1")}
			mesh, err = api.CoreV1().Secrets(ns).Update(ctx, mesh, metav1.UpdateOptions{})
			must(err)
			w.SourceVersion = mesh.ResourceVersion
			w.SourceDataHash = withdrawalSourceDataHash(mesh.Data)
			raw, err := fixture.Checkpoint()
			must(err)
			var cp struct {
				Object corev1.ConfigMap `json:"object"`
			}
			must(json.Unmarshal(raw, &cp))
			cp.Object.UID = ""
			cp.Object.ResourceVersion = ""
			maps := api.CoreV1().ConfigMaps(ns)
			journal, err := maps.Create(ctx, &cp.Object, metav1.CreateOptions{})
			must(err)
			store := handover.Store{ConfigMaps: maps, ClusterUID: "cluster", Name: journal.Name, ExpectedUID: string(journal.UID)}
			inventory.Publications[w.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: string(journal.UID)}
			raw, err = json.Marshal(inventory)
			must(err)
			index, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(w.MeshUID)}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
			must(err)
			authorized := false
			calls, rejectAt := 0, 0
			authorize := func(_ context.Context, intent RemoteWithdrawalIntent) error {
				calls++
				if !authorized || (rejectAt != 0 && calls == rejectAt) || !reflect.DeepEqual(intent, w) {
					return fmt.Errorf("drain authority unavailable")
				}
				return nil
			}
			advance := func() (bool, error) {
				var err error
				mesh, err = api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				index, err = maps.Get(ctx, index.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				return AdvanceReceiveRemoval(ctx, api, store, index.ObjectMeta, mesh, w, "cloud", "cloud-uid", time.Minute, authorize)
			}
			if _, err := advance(); err == nil {
				t.Fatal("missing drain authorization admitted withdrawal")
			}
			unchanged, err := store.Load(ctx)
			must(err)
			if unchanged.Frozen() {
				t.Fatal("unauthorized freeze")
			}
			authorized = true
			originalIntent := w
			for _, fault := range []string{"missing-consumer", "missing-drain", "changed-source"} {
				w = originalIntent
				switch fault {
				case "missing-consumer":
					w.Consumers = nil
				case "missing-drain":
					w.DrainIntent = ""
				case "changed-source":
					w.SourceDataHash = "changed"
				}
				if _, err := advance(); err == nil {
					t.Fatal("invalid capture accepted without error", fault)
				}
				stillLive, err := store.Load(ctx)
				must(err)
				if stillLive.Frozen() {
					t.Fatal("invalid capture persisted an irreversible fence", fault)
				}
			}
			w = originalIntent
			calls, rejectAt = 0, 2
			if _, err := advance(); err == nil {
				t.Fatal("lost write-boundary authorization accepted")
			}
			denied, err := store.Load(ctx)
			must(err)
			if denied.Frozen() {
				t.Fatal("write-boundary denial froze journal")
			}
			calls, rejectAt = 0, 0
			for stage := 0; stage < 4; stage++ {
				ready, err := advance()
				must(err)
				if ready {
					t.Fatal("retirement completed without native receipts")
				}
				frozen, err := store.Load(ctx)
				must(err)
				view, err := frozen.Transition().View()
				must(err)
				if !frozen.RetirementFrozen() || view.Complete || string(view.Round.Phase) != phase {
					t.Fatal("lost interrupted history")
				}
				if stage == 0 {
					wrong := w
					wrong.DrainIntent = "replacement"
					if err := wrong.validateJournalWithdrawalFence(frozen); err == nil {
						t.Fatal("changed drain identity accepted")
					}
				}
			}
			live, _, err := tunnel.ReadReceiveInventory(ctx, maps, "cluster", ns, "mesh", w.MeshUID)
			must(err)
			publication := live.Publications[w.MachineUID]
			if len(live.MachineUIDs) != 1 || len(live.Released) != 0 || len(publication.Retirement) == 0 || len(publication.RetirementCompletion) != 0 || len(publication.RetirementFinalization) != 0 {
				t.Fatal("lost reservation or invented absence evidence")
			}
			current, err := api.CoreV1().Secrets(ns).Get(ctx, "mesh", metav1.GetOptions{})
			must(err)
			if len(current.Data[tunnel.PeerPublicKeyPrefix+w.MachineName]) != 0 {
				t.Fatal("withdrawal was not published")
			}
			set, err := ReadReceiveRetirementSet(ctx, api, store, ns, "mesh", w.MeshUID, w.MachineUID)
			must(err)
			if len(set.Routes.Serving.Plans) != 0 || len(set.Routes.Reserved.Plans) != 1 {
				t.Fatal("withdrawal lost reserved cleanup ownership")
			}
		})
	}
}
