package attachment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Exact source reconstruction only, not live journal admission or cleanup.
func TestCapturedWithdrawalSourceExplainsOnlyRecordedSiblingRemoval(t *testing.T) {
	for _, fault := range []string{"none", "released", "unrecorded", "legacy", "different-capture", "wrong-mesh", "changed-field", "missing-field", "foreign-field", "partial-peer", "replacement", "unrelated-data", "v1-fields", "v2-orphan"} {
		t.Run(fault, func(t *testing.T) {
			w, _, inventory, mesh := retirementFixture(t, "forward", true)
			w.SourceVersion = "before"
			sibling := w
			sibling.MachineUID = "sibling-uid"
			sibling.MachineName = "sibling"
			sibling.PublicKey = w.Consumers[0].PublicKey
			mesh.Data[tunnel.PeerPublicKeyPrefix+w.MachineName] = []byte(w.PublicKey)
			mesh.Data[tunnel.PeerPublicKeyPrefix+sibling.MachineName] = []byte(sibling.PublicKey)
			mesh.Data[tunnel.PeerEndpointPrefix+sibling.MachineName] = []byte("198.51.100.4:51820")
			w.SourceDataHash = withdrawalSourceDataHash(mesh.Data)
			sibling.SourceDataHash = w.SourceDataHash
			peerData := map[string][]byte{tunnel.PeerPublicKeyPrefix + sibling.MachineName: []byte(sibling.PublicKey), tunnel.PeerEndpointPrefix + sibling.MachineName: []byte("198.51.100.4:51820")}
			record := receiveWithdrawalRecord{Version: 2, JournalSHA256: strings.Repeat("a", 64), Withdrawal: sibling, PeerData: peerData}
			var err error
			mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, sibling.MachineName, sibling.PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "legacy":
				record.Version = 1
				record.PeerData = nil
			case "different-capture":
				record.Withdrawal.SourceDataHash = strings.Repeat("f", 64)
			case "wrong-mesh":
				record.Withdrawal.MeshUID = "other"
			case "changed-field":
				record.PeerData[tunnel.PeerEndpointPrefix+sibling.MachineName] = []byte("198.51.100.9:51820")
			case "missing-field":
				delete(record.PeerData, tunnel.PeerEndpointPrefix+sibling.MachineName)
			case "foreign-field":
				record.PeerData[tunnel.NodePublicKeyPrefix+"site"] = []byte("foreign")
			case "partial-peer":
				mesh.Data[tunnel.PeerEndpointPrefix+sibling.MachineName] = []byte("198.51.100.4:51820")
			case "replacement":
				mesh.Data[tunnel.PeerPublicKeyPrefix+sibling.MachineName] = []byte(w.PublicKey)
			case "unrelated-data":
				mesh.Data["changed"] = []byte("data")
			case "v1-fields":
				record.Version = 1
			case "v2-orphan":
				record.Withdrawal.Orphaned = true
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			publication := tunnel.ReceiveJournalPublication{JournalUID: "sibling-journal", Retirement: raw}
			if fault == "released" {
				inventory.Released = map[string]tunnel.ReceiveJournalPublication{sibling.MachineUID: publication}
			} else if fault != "unrecorded" {
				inventory.Publications[sibling.MachineUID] = publication
			}
			before := mesh.DeepCopy()
			err = w.validateCapturedWithdrawalSource(inventory, mesh)
			if (err == nil) != (fault == "none" || fault == "released") {
				t.Fatalf("unexpected reconstruction result: %v", err)
			}
			if !reflect.DeepEqual(before, mesh) {
				t.Fatal("validation restored peer fields into input")
			}
		})
	}
}

func TestLegacyWithdrawalSourceKeepsExactRevision(t *testing.T) {
	w := RemoteWithdrawalIntent{MeshNamespace: "test", MeshName: "mesh", MeshUID: "mesh", SourceVersion: "original"}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "mesh", ResourceVersion: "original"}}
	if err := w.validateCapturedWithdrawalSource(tunnel.ReceiveInventory{}, mesh); err != nil {
		t.Fatal(err)
	}
	mesh.ResourceVersion = "later"
	if err := w.validateCapturedWithdrawalSource(tunnel.ReceiveInventory{}, mesh); err == nil {
		t.Fatal("legacy source silently gained reconstruction semantics")
	}
}
