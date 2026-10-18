package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real tokens, API CAS and retained identities; native cleanup is synthetic.
// Historical verification does not qualify current absence, recovery or release.
func verifyAuthenticatedRetirementFinalization(t *testing.T, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, node *corev1.Node, machineUID, inventoryUID string, policy handover.PodReceiptAuthenticator, submission handover.PodReceipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Load(ctx)
	must(err)
	prepare := func() {
		scope, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
		must(err)
		entry := submission
		entry.Receipt.RoundHash = scope.Digest()
		entry.Receipt.ObservedAt = time.Now()
		must(RecordReceiveRetirementWithPodReceipts(ctx, api, store, inventoryUID, scope.Digest(), mesh, snapshot, []handover.PodReceipt{entry}, map[string]handover.PodReceiptAuthenticator{entry.Receipt.NodeUID: policy}, time.Minute, func() error { return nil }))
	}
	read := func() (ReceiveRetirementSet, bool, error) {
		return ReadReceiveRetirementFinalization(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute)
	}
	_, ok, err := read()
	must(err)
	if ok {
		t.Fatal("missing finalization accepted")
	}
	changeInventory := func() error {
		inventory, err := store.ConfigMaps.Get(ctx, tunnel.ReceiveInventoryName(string(mesh.UID)), metav1.GetOptions{})
		if err != nil {
			return err
		}
		inventory.Annotations = map[string]string{"finalization-race": time.Now().String()}
		_, err = store.ConfigMaps.Update(ctx, inventory, metav1.UpdateOptions{})
		return err
	}
	for _, fault := range []string{"missing-check", "owner", "inventory", "boot", "expiry", "CAS"} {
		t.Run(fault, func(t *testing.T) {
			prepare()
			calls := 0
			age := time.Minute
			candidate := store
			denied := errors.New("owner or consumer policy changed")
			check := func() error {
				calls++
				if calls != 2 {
					return nil
				}
				switch fault {
				case "owner":
					return denied
				case "inventory":
					return changeInventory()
				case "boot":
					n, err := api.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					original := n.Status.NodeInfo.BootID
					t.Cleanup(func() {
						current, e := api.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
						if e != nil {
							t.Error(e)
							return
						}
						current.Status.NodeInfo.BootID = original
						_, e = api.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
						if e != nil {
							t.Error(e)
						}
					})
					n.Status.NodeInfo.BootID = "changed-before-finalization"
					_, err = api.CoreV1().Nodes().UpdateStatus(ctx, n, metav1.UpdateOptions{})
					return err
				case "expiry":
					time.Sleep(2100 * time.Millisecond)
				}
				return nil
			}
			if fault == "missing-check" {
				check = nil
			}
			if fault == "expiry" {
				age = 2 * time.Second
			}
			if fault == "CAS" {
				candidate.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, before: changeInventory}
			}
			err := FinalizeReceiveRetirementCompletion(ctx, api, candidate, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, age, check)
			if err == nil {
				t.Fatal("unsafe finalization accepted")
			}
			if fault == "owner" && !errors.Is(err, denied) {
				t.Fatal("owner check not preserved", err)
			}
			if fault == "CAS" && !apierrors.IsConflict(err) {
				t.Fatal("conditional write conflict not preserved", err)
			}
			if fault == "expiry" && calls != 2 {
				t.Fatal("expiry fixture did not reach write boundary")
			}
			_, ok, err := read()
			must(err)
			if ok {
				t.Fatal("failed finalization persisted")
			}
		})
	}
	prepare()
	lost := store
	lost.ConfigMaps = retirementInventoryWriter{ConfigMapInterface: store.ConfigMaps, lostResponse: true}
	if err := FinalizeReceiveRetirementCompletion(ctx, api, lost, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute, func() error { return nil }); err == nil {
		t.Fatal("ambiguous finalization response concealed")
	}
	source, ok, err := read()
	must(err)
	if !ok {
		t.Fatal("persisted finalization not verifiable after lost response")
	}
	publication := source.Inventory.Publications[machineUID]
	if strings.Contains(string(publication.RetirementFinalization), submission.BearerToken) || len(source.Inventory.MachineUIDs) != 1 || publication.JournalUID != string(snapshot.Metadata().UID) {
		t.Fatal("finalization leaked credential or released ownership")
	}
	// A deterministic time projection checks historical semantics, not elapsed soak.
	ok, err = validateReceiveRetirementFinalization(source, store.ClusterUID, machineUID, inventoryUID, time.Minute, time.Now().Add(2*time.Minute))
	must(err)
	if !ok {
		t.Fatal("historical evidence incorrectly required ongoing refresh")
	}
	must(changeInventory())
	source, ok, err = read()
	must(err)
	if !ok {
		t.Fatal("unrelated later inventory metadata invalidated historical evidence")
	}
	if err := FinalizeReceiveRetirementCompletion(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), machineUID, inventoryUID, time.Minute, func() error { return nil }); err == nil {
		t.Fatal("finalized evidence was blindly rewritten")
	}
	for _, fault := range []string{"version", "inventory", "revision", "machine", "journal", "mesh", "policy", "future", "late", "withdrawal", "completion", "metadata", "unknown", "missing-receipts", "duplicate-receipts", "missing-boot", "receipt-binding"} {
		t.Run("record/"+fault, func(t *testing.T) {
			candidate := source
			candidate.Inventory.Publications = make(map[string]tunnel.ReceiveJournalPublication, len(source.Inventory.Publications))
			for uid, p := range source.Inventory.Publications {
				candidate.Inventory.Publications[uid] = p
			}
			var f receiveRetirementFinalization
			must(json.Unmarshal(publication.RetirementFinalization, &f))
			switch fault {
			case "version":
				f.Version++
			case "inventory":
				f.Inventory.UID = "replaced"
			case "revision":
				f.Inventory.ResourceVersion = source.Metadata.ResourceVersion
			case "machine":
				f.MachineUID = "other"
			case "journal":
				f.JournalUID = "replaced"
			case "mesh":
				f.MeshUID = "replaced"
			case "policy":
				f.MaxReceiptAge++
			case "future":
				f.FinalizedAt = time.Now().Add(time.Hour)
			case "late":
				f.FinalizedAt = f.FinalizedAt.Add(2 * time.Minute)
			case "withdrawal":
				f.WithdrawalSHA256 = "changed"
			case "completion":
				f.CompletionSHA256 = "changed"
			case "metadata":
				f.Inventory.Annotations = map[string]string{"changed": "authority"}
			}
			p := publication
			if fault == "missing-receipts" || fault == "duplicate-receipts" || fault == "missing-boot" || fault == "receipt-binding" {
				var completion receiveRetirementCompletion
				must(json.Unmarshal(p.RetirementCompletion, &completion))
				switch fault {
				case "missing-receipts":
					completion.Receipts = nil
				case "duplicate-receipts":
					completion.Receipts = append(completion.Receipts, completion.Receipts[0])
				case "missing-boot":
					completion.Receipts[0].ObservedBootID = ""
				case "receipt-binding":
					completion.Receipts[0].RoundHash = strings.Repeat("0", 64)
				}
				p.RetirementCompletion, err = json.Marshal(completion)
				must(err)
				f.CompletionSHA256 = tunnel.HashPeerList(p.RetirementCompletion)
			}
			p.RetirementFinalization, err = json.Marshal(f)
			must(err)
			if fault == "unknown" {
				p.RetirementFinalization = append(p.RetirementFinalization[:len(p.RetirementFinalization)-1], []byte(`,"unknown":true}`)...)
			}
			candidate.Inventory.Publications[machineUID] = p
			now := time.Now()
			if fault == "late" {
				now = now.Add(3 * time.Minute)
			}
			if ok, err := validateReceiveRetirementFinalization(candidate, store.ClusterUID, machineUID, inventoryUID, time.Minute, now); err == nil || ok {
				t.Fatal("changed finalization accepted", fault)
			}
		})
	}
	// Historical consumption is deliberately distinct from live incarnation.
	currentNode, err := api.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	must(err)
	previousBoot := currentNode.Status.NodeInfo.BootID
	currentNode.Status.NodeInfo.BootID = "reboot-after-finalization"
	currentNode, err = api.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
	must(err)
	defer func() {
		currentNode.Status.NodeInfo.BootID = previousBoot
		_, err := api.CoreV1().Nodes().UpdateStatus(ctx, currentNode, metav1.UpdateOptions{})
		if err != nil {
			t.Error(err)
		}
	}()
	var completion receiveRetirementCompletion
	must(json.Unmarshal(publication.RetirementCompletion, &completion))
	if err := handover.VerifyObservationIncarnation(ctx, api, mesh.Namespace, node.Name, completion.Receipts[0]); err == nil {
		t.Fatal("old receipt authorized the new boot")
	}
	_, ok, err = read()
	must(err)
	if !ok {
		t.Fatal("consumer reboot erased historical finalization")
	}
	verifyReleasedJournalAdmission(t, api, store, mesh, machineUID, inventoryUID)
}
