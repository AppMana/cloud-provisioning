package attachment

import (
	"bytes"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
)

// Validate the exact original capture without writing reconstructed fields back
// to the mesh. Only recorded sibling withdrawals from the same source hash may
// explain absent peer fields. All other data, including acknowledgements and
// address reservations, must still match. This grants no consumer exemption.
func (w RemoteWithdrawalIntent) validateCapturedWithdrawalSource(inventory tunnel.ReceiveInventory, mesh *corev1.Secret) error {
	if mesh == nil || mesh.Namespace != w.MeshNamespace || mesh.Name != w.MeshName || string(mesh.UID) != w.MeshUID {
		return fmt.Errorf("withdrawal source scope changed")
	}
	if w.SourceDataHash == "" {
		if w.SourceVersion != mesh.ResourceVersion {
			return fmt.Errorf("mesh version differs from withdrawal stage")
		}
		return nil
	}
	if w.SourceDataHash == withdrawalSourceDataHash(mesh.Data) {
		return nil
	}
	if inventory.Namespace != w.MeshNamespace || inventory.MeshName != w.MeshName || inventory.MeshUID != w.MeshUID {
		return fmt.Errorf("withdrawal reconstruction inventory scope changed")
	}
	restored := make(map[string][]byte, len(mesh.Data))
	for field, value := range mesh.Data {
		restored[field] = bytes.Clone(value)
	}
	for _, publications := range []map[string]tunnel.ReceiveJournalPublication{inventory.Publications, inventory.Released} {
		for uid, publication := range publications {
			if uid == w.MachineUID || len(publication.Retirement) == 0 {
				continue
			}
			record, err := decodeReceiveWithdrawalRecord(publication.Retirement, uid)
			if err != nil {
				return err
			}
			sibling := record.Withdrawal
			if record.Version != 2 || sibling.SourceDataHash != w.SourceDataHash {
				continue
			}
			if publication.JournalUID == "" || sibling.MeshNamespace != w.MeshNamespace || sibling.MeshName != w.MeshName || sibling.MeshUID != w.MeshUID {
				return fmt.Errorf("sibling withdrawal scope changed")
			}
			_, present, err := tunnel.WithdrawRemotePeer(mesh.Data, sibling.MachineName, sibling.PublicKey)
			if err != nil {
				return err
			}
			if present {
				continue
			}
			for field, value := range record.PeerData {
				if _, exists := restored[field]; exists {
					return fmt.Errorf("sibling withdrawal reconstruction overlaps existing fields")
				}
				restored[field] = bytes.Clone(value)
			}
		}
	}
	if withdrawalSourceDataHash(restored) != w.SourceDataHash {
		return fmt.Errorf("mesh data differs from captured withdrawal beyond recorded sibling removals")
	}
	return nil
}
