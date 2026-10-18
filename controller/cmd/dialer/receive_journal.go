package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Scope comes from admitted configuration and live API object identities, not
// readiness payloads. The store must read controller-owned, read-only journals.
type receiveJournalScope struct {
	Namespace, MeshName, MeshUID, MachineUID string
}

type admittedReceiveJournal struct {
	Snapshot handover.Snapshot
	Plan     tunnel.ReceiveTransitionPlan
	NodeUID  string
	BootID   string
}

// loadReceiveJournal performs read-only admission. It does not execute a phase,
// persist recovery state, or issue a receipt. The phase executor must hold the
// native reconciliation lock and revalidate identities before acting; API reads
// cannot fence a node restart or a journal update that happens afterward.
func loadReceiveJournal(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key) (admittedReceiveJournal, error) {
	fail := func(err error) (admittedReceiveJournal, error) { return admittedReceiveJournal{}, err }
	if api == nil || scope.Namespace == "" || scope.MeshName == "" || scope.MeshUID == "" || scope.MachineUID == "" || nodeName == "" || key == (wgtypes.Key{}) || store.Name != tunnel.ReceiveTransitionName(scope.MachineUID) {
		return fail(fmt.Errorf("receiver journal requires admitted scope and local identity"))
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
		return fail(err)
	}
	metadata := snapshot.Metadata()
	if metadata.Namespace != scope.Namespace || metadata.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver journal scope changed or is being deleted"))
	}
	// A fresh process has no checkpoint to compare. It still requires every
	// pinned predecessor, including read access, before admitting a successor.
	if _, found := snapshot.Predecessor(); found {
		if _, err := store.ArchiveNames(ctx, snapshot); err != nil {
			return fail(err)
		}
	}
	if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
		return fail(err)
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return fail(err)
	}
	uid, err := receiveJournalParticipant(plan, scope, nodeName, key)
	if err != nil {
		return fail(err)
	}
	mesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(mesh.UID) != scope.MeshUID || mesh.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver journal mesh identity changed"))
	}
	hasTunnelProjection := false
	for public, site := range plan.Sites {
		if site.RouteSources != nil && !slices.Equal(site.RouteSources, tunnel.ReceiveSiteRouteSources(mesh.Data, site.NodeName)) {
			return fail(fmt.Errorf("receiver site route sources changed after journal creation"))
		}
		if site.Legacy == nil || site.Legacy.Tunnel == nil {
			continue
		}
		hasTunnelProjection = true
		if strings.TrimSpace(string(mesh.Data[tunnel.NodePublicKeyPrefix+site.NodeName])) != public {
			return fail(fmt.Errorf("receiver site key changed after journal creation"))
		}
		if err := tunnel.ValidateReceiveSiteTunnelAllocation(*site.Legacy.Tunnel, mesh.Data, site.NodeName); err != nil {
			return fail(err)
		}
	}
	adoption, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(adoption.UID) != plan.AdoptionUID || adoption.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver journal adoption identity changed"))
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return fail(err)
	}
	node, err := api.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if err := validateReceiveParticipantNode(node, uid, boot); err != nil {
		return fail(err)
	}
	if hasTunnelProjection {
		currentMesh, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
		if err != nil {
			return fail(err)
		}
		if currentMesh.UID != mesh.UID || currentMesh.ResourceVersion != mesh.ResourceVersion || currentMesh.DeletionTimestamp != nil {
			return fail(fmt.Errorf("receiver mesh allocation changed during admission"))
		}
	}
	current, err := store.Load(ctx)
	if err != nil {
		return fail(err)
	}
	if current.ContinuesExecution(snapshot) != nil {
		return fail(fmt.Errorf("receiver journal changed during admission"))
	}
	return admittedReceiveJournal{Snapshot: current, Plan: plan, NodeUID: uid, BootID: boot}, nil
}

func validateReceiveParticipantNode(node *corev1.Node, uid, boot string) error {
	if node == nil || uid == "" || boot == "" {
		return fmt.Errorf("receiver participant requires node and boot identity")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
	}
	if string(node.UID) != uid || node.Status.NodeInfo.BootID != boot || node.DeletionTimestamp != nil || !ready {
		return fmt.Errorf("receiver journal participant identity, boot or readiness changed")
	}
	return nil
}

// Kubelet heartbeats advance the Node resource version without changing its
// identity or forwarding authority. Compare the remaining source fields so
// routine status refreshes do not prevent bounded retirement from completing.
// Readiness transitions, boot changes, addresses and allocation remain fenced.
func sameReceiveParticipantNode(a, b *corev1.Node) bool {
	if a == nil || b == nil || a.UID == "" || b.UID == "" || a.ResourceVersion == "" || b.ResourceVersion == "" || a.DeletionTimestamp != nil || b.DeletionTimestamp != nil {
		return false
	}
	normalize := func(node *corev1.Node) *corev1.Node {
		node = node.DeepCopy()
		node.ResourceVersion = ""
		node.ManagedFields = nil
		// Cached images change during pulls and garbage collection. They do not
		// identify the running agent; observation authentication separately
		// checks its Pod, container incarnation and admitted runtime image.
		node.Status.Images = nil
		for i := range node.Status.Conditions {
			node.Status.Conditions[i].LastHeartbeatTime = metav1.Time{}
			// Reporters may refresh a network or pressure transition timestamp
			// without changing its condition. These timestamps are informational;
			// condition values remain fenced. Preserve Ready transition history
			// to reject a readiness cycle between admission reads.
			if node.Status.Conditions[i].Type != corev1.NodeReady {
				node.Status.Conditions[i].LastTransitionTime = metav1.Time{}
			}
		}
		return node
	}
	return equality.Semantic.DeepEqual(normalize(a), normalize(b))
}

func receiveJournalParticipant(plan tunnel.ReceiveTransitionPlan, scope receiveJournalScope, nodeName string, key wgtypes.Key) (string, error) {
	if scope.Namespace == "" || scope.MeshName == "" || scope.MeshUID == "" || scope.MachineUID == "" || nodeName == "" || key == (wgtypes.Key{}) || plan.Namespace != scope.Namespace || plan.MeshName != scope.MeshName || plan.MeshUID != scope.MeshUID || plan.MachineUID != scope.MachineUID {
		return "", fmt.Errorf("receiver journal differs from admitted mesh or machine")
	}
	uid := ""
	if key.PublicKey().String() == plan.RootPublicKey && nodeName == plan.CloudNodeName {
		uid = plan.CloudNodeUID
	} else if site, found := plan.Sites[key.PublicKey().String()]; found && site.NodeName == nodeName {
		uid = site.NodeUID
	}
	if uid == "" {
		return "", fmt.Errorf("local identity is not a journal participant")
	}
	return uid, nil
}
