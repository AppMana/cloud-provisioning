package attachment

import (
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
)

// ReceiveRetirementPlan binds a previously persisted withdrawal intent to a
// frozen, still-reserved journal and a published withdrawal. It does not infer
// cleanup from Machine or Node absence. Every recorded receiver participant
// remains required even when unavailable, and new published consumers require
// an explicit extension of the persisted intent.
//
// This is input validation, not native execution authority or acknowledgement.
// Callers must load these objects live, authenticate journal ancestry, verify
// the persisted intent has not changed, and recheck under the native lock.
// Prepared devices outside the journal still require durable ownership records.
// Keep the reservation until all required native retirement receipts are durable.
func (w RemoteWithdrawalIntent) ReceiveRetirementPlan(snapshot handover.Snapshot, inventory tunnel.ReceiveInventory, mesh *corev1.Secret) (tunnel.ReceiveTransitionPlan, error) {
	fail := func(message string) (tunnel.ReceiveTransitionPlan, error) {
		return tunnel.ReceiveTransitionPlan{}, fmt.Errorf("receive retirement: %s", message)
	}
	if mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil ||
		w.SourceVersion == "" || w.SourceVersion == mesh.ResourceVersion || w.DrainIntent == "" || w.MachineName == "" || w.MachineUID == "" || w.PublicKey == "" ||
		w.MeshNamespace != mesh.Namespace || w.MeshName != mesh.Name || w.MeshUID != string(mesh.UID) {
		return fail("withdrawal requires exact live mesh and persisted drain identity")
	}
	if inventory.Version != 1 || inventory.ClusterUID == "" || inventory.Namespace != w.MeshNamespace || inventory.MeshName != w.MeshName || inventory.MeshUID != w.MeshUID {
		return fail("inventory scope differs from withdrawal")
	}
	meta := snapshot.Metadata()
	if !snapshot.Frozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil {
		return fail("journal must be live and frozen")
	}
	// Legacy reservations without a published UID cannot bind this destructive
	// operation to an API incarnation, even though ordinary readers accept them.
	publication := inventory.Publications[w.MachineUID]
	if publication.JournalUID != string(meta.UID) || len(publication.Plan) != 0 {
		return fail("journal incarnation is not explicitly reserved")
	}
	if err := inventory.CheckJournal(w.MachineUID, string(meta.UID)); err != nil {
		return fail(err.Error())
	}
	p, err := tunnel.DecodeReceiveTransition(snapshot, inventory.ClusterUID)
	if err != nil {
		return fail(err.Error())
	}
	view, err := snapshot.Transition().View()
	if err != nil || !view.Complete || view.ServingGeneration == "" {
		return fail("journal has not completed")
	}
	if p.Namespace != w.MeshNamespace || p.MeshName != w.MeshName || p.MeshUID != w.MeshUID || p.MachineName != w.MachineName || p.MachineUID != w.MachineUID || p.RootPublicKey != w.PublicKey {
		return fail("withdrawal differs from journal ownership")
	}
	if _, present, err := tunnel.WithdrawRemotePeer(mesh.Data, w.MachineName, w.PublicKey); err != nil || present {
		return fail("complete peer withdrawal has not been published")
	}
	names, uids, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	sites, remotes := map[string]ConsumerTarget{}, map[string]ConsumerTarget{}
	for _, c := range w.Consumers {
		if c.NodeName == "" || c.NodeUID == "" || c.PublicKey == "" || names[c.NodeName] || uids[c.NodeUID] || keys[c.PublicKey] ||
			c.NodeName == p.CloudNodeName || c.NodeUID == p.CloudNodeUID || c.PublicKey == p.RootPublicKey {
			return fail("incomplete, duplicate or retiring consumer identity")
		}
		names[c.NodeName], uids[c.NodeUID], keys[c.PublicKey] = true, true, true
		if c.Site {
			sites[c.NodeName] = c
		} else {
			if c.MachineName == "" || c.MachineName == w.MachineName || c.SecretUID == "" || c.TunnelAddress == "" || remotes[c.MachineName].NodeUID != "" {
				return fail("incomplete or duplicate remote consumer")
			}
			remotes[c.MachineName] = c
		}
	}
	for public, site := range p.Sites {
		c, found := sites[site.NodeName]
		if !found || c.NodeUID != site.NodeUID || c.PublicKey != public {
			return fail("withdrawal omits or replaces a journal participant")
		}
	}
	for field, value := range mesh.Data {
		for _, kind := range []struct {
			prefix  string
			targets map[string]ConsumerTarget
			key     bool
		}{
			{tunnel.NodePublicKeyPrefix, sites, true},
			{tunnel.SiteAddressesPrefix, sites, false},
			{tunnel.PeerPublicKeyPrefix, remotes, true},
		} {
			if !strings.HasPrefix(field, kind.prefix) {
				continue
			}
			c, found := kind.targets[strings.TrimPrefix(field, kind.prefix)]
			if !found || (kind.key && string(value) != c.PublicKey) {
				return fail("published consumer membership changed")
			}
		}
	}
	return p, nil
}
