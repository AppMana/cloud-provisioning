package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// A tunnel-class cloud node joins the site's pod network over its tunnel.
// On a Calico site it runs the site's Calico, but not the site's
// calico-node DaemonSet: that one detects each node's address
// (IP=autodetect), and on a cloud node detection finds the cloud
// provider's address, which the site cannot reach, and keeps finding it
// again after the product states the tunnel address. A Calico agent stops
// detecting only per DaemonSet, so the product runs its own copy of the
// site's DaemonSet on tunnel-class nodes with detection off (IP=none and,
// where the site runs IPv6, IP6=none); Calico then keeps the stored
// address, which the product writes.
//
// The tunnel carries no BGP. A Calico node configured with a peer that
// never establishes reports itself unready, so the site's BGP mesh is
// limited to site nodes, which leaves cloud nodes with no peers.

const (
	// calicoAgent is the site's Linux Calico DaemonSet and its container.
	calicoAgent = "calico-node"
	// tunnelAgentSuffix names the product's copy beside the site's.
	tunnelAgentSuffix = "-tunnel"
	// tunnelComponent is the agent's ComponentLabel value and pod selector.
	tunnelComponent = "calico-tunnel"
	// tunnelHashAnnotation records the site template the agent was made from.
	tunnelHashAnnotation = "cloud-provisioning.appmana.com/site-template"
	// siteMeshPeer replaces the full mesh with one limited to site nodes.
	siteMeshPeer = "cloud-provisioning-site-mesh"
)

var (
	bgpConfigurationGVK = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "BGPConfiguration"}
	bgpPeerGVK          = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "BGPPeer"}
	// siteOnly selects Calico nodes that this product did not provision.
	siteOnly = "!has(" + platform.RoleLabel + ")"
)

// isCalicoAgent reports whether a DaemonSet is the site's Linux Calico.
func isCalicoAgent(ds *appsv1.DaemonSet) bool {
	if ds.Name != calicoAgent {
		return false
	}
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == calicoAgent {
			return true
		}
	}
	return false
}

// cloudWorkers keeps the site's Calico off every node this product
// provisions; the tunnel agent covers tunnel-class ones.
var cloudWorkers = corev1.NodeSelectorRequirement{Key: platform.RoleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{platform.RoleValue}}

// TunnelAgent is the product's Calico agent for tunnel-class cloud nodes,
// made from the site's calico-node.
func TunnelAgent(site *appsv1.DaemonSet) (*appsv1.DaemonSet, error) {
	spec := site.Spec.Template.Spec.DeepCopy()
	spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: platform.RoleLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{platform.RoleValue}},
				{Key: platform.CNILabel, Operator: corev1.NodeSelectorOpDoesNotExist},
			},
		}}},
	}}
	for i := range spec.Containers {
		if spec.Containers[i].Name == calicoAgent {
			spec.Containers[i].Env = statedAddresses(spec.Containers[i].Env)
		}
	}
	// The site's selector keys come off the agent's pods, so neither
	// DaemonSet selects the other's.
	labels := map[string]string{}
	for k, v := range site.Spec.Template.Labels {
		labels[k] = v
	}
	if site.Spec.Selector != nil {
		for k := range site.Spec.Selector.MatchLabels {
			delete(labels, k)
		}
		for _, r := range site.Spec.Selector.MatchExpressions {
			delete(labels, r.Key)
		}
	}
	labels[platform.ComponentLabel] = tunnelComponent
	annotations := map[string]string{}
	for k, v := range site.Spec.Template.Annotations {
		annotations[k] = v
	}
	agent := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      site.Name + tunnelAgentSuffix,
			Namespace: site.Namespace,
			Labels:    map[string]string{platform.ComponentLabel: tunnelComponent},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: site.Name, UID: site.UID, Controller: ptrTo(true),
			}},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector:             &metav1.LabelSelector{MatchLabels: map[string]string{platform.ComponentLabel: tunnelComponent}},
			UpdateStrategy:       site.Spec.UpdateStrategy,
			MinReadySeconds:      site.Spec.MinReadySeconds,
			RevisionHistoryLimit: site.Spec.RevisionHistoryLimit,
			Template:             corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: *spec},
		},
	}
	raw, err := json.Marshal(agent.Spec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	agent.Annotations = map[string]string{tunnelHashAnnotation: hex.EncodeToString(sum[:8])}
	return agent, nil
}

// statedAddresses turns Calico's address detection off for each family the
// site runs: IP is always set, IP6 only where the site sets it.
func statedAddresses(env []corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	seenIP := false
	for _, e := range env {
		switch e.Name {
		case "IP_AUTODETECTION_METHOD", "IP6_AUTODETECTION_METHOD":
			continue
		case "IP", "IP6":
			seenIP = seenIP || e.Name == "IP"
			e = corev1.EnvVar{Name: e.Name, Value: "none"}
		}
		out = append(out, e)
	}
	if !seenIP {
		out = append(out, corev1.EnvVar{Name: "IP", Value: "none"})
	}
	return out
}

func ptrTo[T any](v T) *T { return &v }

// ensureTunnelAgent creates or refreshes the agent from the site's calico-node.
func (e *Excluder) ensureTunnelAgent(ctx context.Context, site *appsv1.DaemonSet) error {
	desired, err := TunnelAgent(site)
	if err != nil {
		return err
	}
	existing := &appsv1.DaemonSet{}
	err = e.Reader.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := e.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating the tunnel-node Calico agent: %w", err)
		}
		logf.FromContext(ctx).Info("created the tunnel-node Calico agent", "daemonSet", client.ObjectKeyFromObject(desired))
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Annotations[tunnelHashAnnotation] == desired.Annotations[tunnelHashAnnotation] && tunnelAgentIntact(existing) {
		return nil
	}
	existing.Labels, existing.Annotations, existing.OwnerReferences = desired.Labels, desired.Annotations, desired.OwnerReferences
	existing.Spec = desired.Spec
	if err := e.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating the tunnel-node Calico agent: %w", err)
	}
	return nil
}

// tunnelAgentIntact reports whether the agent still has detection off; an
// edit that left the recorded hash would otherwise hide it.
func tunnelAgentIntact(ds *appsv1.DaemonSet) bool {
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != calicoAgent {
			continue
		}
		for _, v := range c.Env {
			if v.Name == "IP" {
				return v.Value == "none"
			}
		}
	}
	return false
}

// verifyTunnelAgent reports whether the agent exists and is current.
func (e *Excluder) verifyTunnelAgent(ctx context.Context, site *appsv1.DaemonSet) error {
	desired, err := TunnelAgent(site)
	if err != nil {
		return err
	}
	existing := &appsv1.DaemonSet{}
	if err := e.Reader.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return &PendingError{DaemonSets: []string{client.ObjectKeyFromObject(desired).String() + " (not yet created)"}}
		}
		return err
	}
	if existing.Annotations[tunnelHashAnnotation] != desired.Annotations[tunnelHashAnnotation] || !tunnelAgentIntact(existing) {
		return &PendingError{DaemonSets: []string{client.ObjectKeyFromObject(desired).String() + " (behind the site's calico-node)"}}
	}
	return nil
}

// ensureBGPScope replaces the full mesh with a mesh of site nodes. Peering a
// site already configured is left as it is; verifyBGPScope judges it.
func (e *Excluder) ensureBGPScope(ctx context.Context) error {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(bgpConfigurationGVK)
	err := e.Reader.Get(ctx, client.ObjectKey{Name: "default"}, config)
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading BGPConfiguration default: %w", err)
	}
	exists := err == nil
	if mesh, found, _ := unstructured.NestedBool(config.Object, "spec", "nodeToNodeMeshEnabled"); exists && found && !mesh {
		return nil
	}
	// The site mesh first, so site nodes never lose their peers.
	peer := &unstructured.Unstructured{}
	peer.SetGroupVersionKind(bgpPeerGVK)
	peer.SetName(siteMeshPeer)
	peer.SetLabels(map[string]string{platform.ComponentLabel: tunnelComponent})
	peer.Object["spec"] = map[string]any{"nodeSelector": siteOnly, "peerSelector": siteOnly}
	if err := e.Client.Create(ctx, peer); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the site BGP mesh: %w", err)
	}
	if !exists {
		config.SetName("default")
		config.Object["spec"] = map[string]any{"nodeToNodeMeshEnabled": false}
		if err := e.Client.Create(ctx, config); err != nil {
			return fmt.Errorf("creating BGPConfiguration default: %w", err)
		}
	} else {
		patched := config.DeepCopy()
		if err := unstructured.SetNestedField(patched.Object, false, "spec", "nodeToNodeMeshEnabled"); err != nil {
			return err
		}
		if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(config, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("limiting the BGP mesh to site nodes: %w", err)
		}
	}
	logf.FromContext(ctx).Info("limited the Calico BGP mesh to site nodes", "peer", siteMeshPeer)
	return nil
}

// verifyBGPScope reports whether any BGP peering reaches a cloud node.
func (e *Excluder) verifyBGPScope(ctx context.Context) error {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(bgpConfigurationGVK)
	err := e.Reader.Get(ctx, client.ObjectKey{Name: "default"}, config)
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading BGPConfiguration default: %w", err)
	}
	if mesh, found, _ := unstructured.NestedBool(config.Object, "spec", "nodeToNodeMeshEnabled"); err != nil || !found || mesh {
		return &PendingError{DaemonSets: []string{"BGPConfiguration default (the full mesh peers every node with every cloud node)"}}
	}
	reaching, err := PeersReachingCloudNodes(ctx, e.Reader)
	if err != nil {
		return err
	}
	if len(reaching) > 0 {
		return &UnsupportedError{Reason: fmt.Sprintf("BGPPeer %s can peer with cloud nodes, over a tunnel that carries no BGP; scope its nodeSelector and peerSelector with %q", strings.Join(reaching, ", "), siteOnly)}
	}
	return nil
}

// PeersReachingCloudNodes names the BGPPeers that could peer a cloud node,
// which the product does not change: they are the operator's to scope.
func PeersReachingCloudNodes(ctx context.Context, reader client.Reader) ([]string, error) {
	peers := &unstructured.UnstructuredList{}
	peers.SetGroupVersionKind(bgpPeerGVK.GroupVersion().WithKind(bgpPeerGVK.Kind + "List"))
	if err := reader.List(ctx, peers); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing BGPPeers: %w", err)
	}
	var reaching []string
	for _, peer := range peers.Items {
		reaches, err := peerReachesCloudNodes(ctx, reader, &peer)
		if err != nil {
			return nil, err
		}
		if reaches {
			reaching = append(reaching, peer.GetName())
		}
	}
	return reaching, nil
}

// peerReachesCloudNodes reports whether a BGPPeer could apply to, or select
// as a peer, a node this product provisions. A selector counts as excluding
// cloud nodes only when siteOnly is one of its conjuncts and it has no
// alternatives.
func peerReachesCloudNodes(ctx context.Context, reader client.Reader, peer *unstructured.Unstructured) (bool, error) {
	nodeName, _, _ := unstructured.NestedString(peer.Object, "spec", "node")
	nodeSelector, _, _ := unstructured.NestedString(peer.Object, "spec", "nodeSelector")
	peerSelector, _, _ := unstructured.NestedString(peer.Object, "spec", "peerSelector")
	if peerSelector != "" && !scopedToSite(peerSelector) {
		return true, nil
	}
	if nodeName != "" {
		node := &corev1.Node{}
		if err := reader.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		_, cloud := node.Labels[platform.RoleLabel]
		return cloud, nil
	}
	return !scopedToSite(nodeSelector), nil
}

func scopedToSite(selector string) bool {
	// An alternative could select cloud nodes whatever the conjuncts say.
	if strings.Contains(selector, "||") {
		return false
	}
	for _, term := range strings.Split(selector, "&&") {
		if strings.TrimSpace(term) == siteOnly {
			return true
		}
	}
	return false
}
