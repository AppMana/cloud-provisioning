// Command endpoint-controller is the cloud-provisioning operator. It
// runs three reconcilers over one manager:
//
//   - claim: expands a ProvisionedNodeClaim (the single resource a
//     user commits) into the CAPI Machine + provider machine pair.
//   - join: renders the tunnel-bootstrapping userdata into each
//     Machine's own bootstrap Secret.
//   - mesh (this file): owns the tunnel mesh. It allocates each selected
//     tunnel-endpoint node's address, mirrors Machine external
//     addresses into the peer Secret, renders each remote's adoption
//     config, and creates both dialer DaemonSets directly (no CRD, no
//     hand-authored pod spec).
//
// Cluster API's own Machine controller copies the address up from
// whatever infrastructure provider sits underneath (AWSMachine today,
// anything else later) into Machine.status.addresses. That is the one
// thing the mesh reconciler depends on; it never reads AWSMachine (or
// any other provider-specific type) directly.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/claim"
	claimpkg "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/discover"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joindocker "github.com/appmana/cloud-provisioning/controller/pkg/join/docker"
	joink0s "github.com/appmana/cloud-provisioning/controller/pkg/join/k0s"
	joinkubeadm "github.com/appmana/cloud-provisioning/controller/pkg/join/kubeadm"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// machineGVK is v1beta2 throughout this module: v1beta1 is gone from
// current Cluster API, and a stale version here produces a watch that
// never fires, so no DaemonSet is created.
var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

var gatewayGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "Gateway",
}

const externalDNSTargetAnnotation = "external-dns.alpha.kubernetes.io/target"

// cloudWorkerTaintKey is the taint each cloud-worker node registers
// itself with (via kubelet's own --register-with-taints, baked into
// the join-pattern template) and the toleration the public Gateway's
// data-plane DaemonSet carries. The on-prem dialer DaemonSet does not
// tolerate it.
//
// It is a single Go constant rather than independently-configured flag
// defaults, so the taint key cannot drift between where it is applied
// and where it is tolerated. A node carrying a taint nothing tolerates
// cannot schedule its own Gateway data-plane pod.
const cloudWorkerTaintKey = "cloud-provisioning.appmana.com/internet-facing"

// cloudWorkerRoleLabel/Value select which Machine this operator
// treats as a remote peer. They are also the default
// --machine-selector value, so the label a node registers with and the
// selector used to find its Machine cannot drift either.
const (
	cloudWorkerRoleLabel = "cloud-provisioning.appmana.com/role"
	cloudWorkerRoleValue = "cloud-worker"
)

// controlPlaneLabel marks control-plane nodes. The on-prem dialer
// DaemonSet excludes them by nodeAffinity, not merely by lacking a
// toleration, so the other side of a cloud tunnel does not land on a
// controller. Control planes therefore carry no WireGuard interface
// and no tunnel routes, so a tunnel cannot cost a control plane its
// default route; the exclusion is enforced by scheduling, not only by
// code. Remotes reach the API through a designated worker that
// masquerades tunnel-sourced traffic.
const controlPlaneLabel = "node-role.kubernetes.io/control-plane"

// meshReconciler owns the tunnel mesh.
type meshReconciler struct {
	client.Client
	// reader is the manager's uncached API reader. The Secret, Gateway
	// and DaemonSets are read once per reconcile and not watched:
	// routing those Gets through the cached client would make
	// controller-runtime start cluster-wide informers for those types,
	// needing list/watch RBAC this identity does not have.
	reader           client.Reader
	secretNamespace  string
	secretName       string
	secretKey        string
	port             string
	gatewayNamespace string
	gatewayName      string

	// Tunnel-endpoint placement: which local nodes terminate tunnels.
	// Empty selector = every Linux worker. Control-plane nodes are
	// excluded unless explicitly selected.
	tunnelEndpointSelector labels.Selector
	tunnelEndpointsRaw     string
	tunnelSubnet           string
	localAddressBase       string

	// Dialer DaemonSets: this operator owns both specs directly. There
	// is no CRD and they are not hand-authored in gitops.
	dialerDaemonSetName   string
	dialerServiceAccount  string
	dialerImage           string
	dialerImagePullSecret string
	dialerPrivateKeyDir   string
	ifaceName             string
	apiVIP                string

	// ownerRef ties everything this controller creates at runtime
	// (both DaemonSets, the peer Secret, per-machine adoption Secrets)
	// to an object the installer owns, its own Deployment. Without it,
	// uninstalling the release leaves the DaemonSets running, and a
	// tunnel interface on each endpoint node with nothing managing it.
	ownerRef *metav1.OwnerReference
	// network is how this cluster carries pod traffic, which decides
	// whether a peer needs pod prefixes at all and where they are read
	// from. Re-detected when the network's own configuration changes.
	network cni.Network
	// transitBGPPort is where a tunnel endpoint's speaker listens, and
	// zero when the site needs no transit.
	transitBGPPort int
	transitBGPASN  int

	dialerCloudDaemonSetName string
	dialerCloudListenPort    string
	// dialerCloudImage, when set, is a public base image the remote's
	// DaemonSet runs instead of the project image, executing the host
	// binary that cloud-init already installed and sha-verified. A
	// remote node cannot be preloaded and may have no registry
	// credential; without this its adoption DaemonSet sits in
	// ImagePullBackOff, leaving the node on the frozen bootstrap peer
	// list, so config changes do not reach it.
	dialerCloudImage      string
	dialerCloudHostBinary string
}

// owners returns the ownerReference list to stamp on everything this
// controller creates, so an uninstall garbage-collects it.
// firstAddress is the address the rest of the site reaches a node by.
func firstAddress(addresses []string) string {
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}

func (r *meshReconciler) owners() []metav1.OwnerReference {
	if r.ownerRef == nil {
		return nil
	}
	return []metav1.OwnerReference{*r.ownerRef}
}

func (r *meshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Allocate tunnel addresses and cluster VIPs for every selected
	// endpoint node first: the peer graph the dialers and the join
	// reconciler read is derived from these, and a node that hasn't
	// been allocated one is not a mesh member.
	if err := r.reconcileTunnelEndpoints(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling tunnel endpoints: %w", err)
	}

	// The DaemonSets are unconditional: the tunnel is always wanted
	// whenever this operator runs at all, and the dialer tolerates a
	// peer whose endpoint is still "pending".
	if err := r.ensureDialerDaemonSet(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring dialer daemonset: %w", err)
	}
	if err := r.ensureCloudDialerDaemonSet(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring cloud dialer daemonset: %w", err)
	}

	// Node events enqueue a nameless request (they change the endpoint
	// set, not any one Machine): mesh-wide maintenance above is all
	// they ask for. An empty-name Get would be a non-NotFound error,
	// an infinite error requeue rather than a no-op.
	if req.Name == "" {
		return ctrl.Result{}, nil
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Adoption config: a public-data-only peer list for this machine,
	// re-rendered from live cluster state every reconcile. The cloud
	// dialer prefers it over the bootstrap peers.json baked into
	// immutable userdata, which is how post-join corrections (pod/
	// service CIDRs, added or removed peers, changed endpoints) reach
	// the node. It contains no private key: this document lands on an
	// internet-facing machine.
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring adoption config: %w", err)
	}

	// The remote node's own pod blocks, published onto its peer entry
	// so the nodes at home can reach the pods running on it. Its blocks
	// are allocated after it joins, so this is recomputed here rather
	// than written once when the machine was created.
	//
	// A node has no blocks until something is scheduled on it, which is
	// after it joins, so this comes back until it does. Nothing else
	// would bring it back: the reconciler is driven by Machine events,
	// and the machine stops changing once it is running.
	remoteBlocksPending := false
	if nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name"); nodeName != "" {
		published, err := r.publishRemotePodCIDRs(ctx, machine.GetName(), nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		remoteBlocksPending = !published
	}

	// Tell the CNI which address to peer on, once the node exists.
	{
		nodeName, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		tunnelAddr := strings.SplitN(strings.TrimSpace(
			machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"]), "/", 2)[0]
		claimRef := ""
		for _, owner := range machine.GetOwnerReferences() {
			if owner.Kind == "ProvisionedNodeClaim" {
				claimRef = machine.GetNamespace() + "/" + owner.Name
			}
		}
		if err := r.ensureCNINodeAddress(ctx, nodeName, tunnelAddr, claimRef); err != nil {
			return ctrl.Result{}, err
		}
	}

	addresses, found, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading status.addresses: %w", err)
	}
	if !found {
		log.V(1).Info("no status.addresses yet, waiting for the infrastructure provider")
		return ctrl.Result{}, nil
	}

	// The dialing endpoint: ExternalIP when the provider reports one (a
	// real cloud's public address), otherwise InternalIP. Some
	// providers (CAPD containers, private-addressed infra) report only
	// internal addresses, and for them that is the reachable endpoint.
	// No address is invented here; absent both, keep waiting.
	var externalIP, internalIP string
	for _, entry := range addresses {
		address, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		ip, ok := address["address"].(string)
		if !ok || ip == "" {
			continue
		}
		switch address["type"] {
		case "ExternalIP":
			if externalIP == "" {
				externalIP = ip
			}
		case "InternalIP":
			if internalIP == "" {
				internalIP = ip
			}
		}
	}
	if externalIP == "" {
		externalIP = internalIP
	}
	if externalIP == "" {
		log.V(1).Info("no ExternalIP/InternalIP in status.addresses yet, waiting")
		return ctrl.Result{}, nil
	}

	endpoint := fmt.Sprintf("%s:%s", externalIP, r.port)

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting secret %s: %w", secretKey, err)
	}

	// Per-Machine key (r.secretKey is a prefix, e.g. "peer-endpoint-"),
	// not a flat singleton, so a second cloud Machine does not clobber
	// the first's endpoint entry.
	machineKey := r.secretKey + machine.GetName()
	if string(secret.Data[machineKey]) != endpoint {
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[machineKey] = []byte(endpoint)
		if err := r.Patch(ctx, secret, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching secret %s: %w", secretKey, err)
		}
		log.Info("updated dialer peer endpoint", "endpoint", endpoint, "machine", req.NamespacedName)
	}

	if r.gatewayName != "" {
		gw := &unstructured.Unstructured{}
		gw.SetGroupVersionKind(gatewayGVK)
		gwKey := types.NamespacedName{Namespace: r.gatewayNamespace, Name: r.gatewayName}
		if err := r.reader.Get(ctx, gwKey, gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("getting Gateway %s: %w", gwKey, err)
		}
		if gw.GetAnnotations()[externalDNSTargetAnnotation] != externalIP {
			gwPatch := client.MergeFrom(gw.DeepCopy())
			annotations := gw.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[externalDNSTargetAnnotation] = externalIP
			gw.SetAnnotations(annotations)
			if err := r.Patch(ctx, gw, gwPatch); err != nil {
				return ctrl.Result{}, fmt.Errorf("patching Gateway %s: %w", gwKey, err)
			}
			log.Info("updated Gateway external-dns target", "ip", externalIP, "gateway", gwKey)
		}
	}
	if remoteBlocksPending {
		// Come back for the blocks this node has not been given yet.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileTunnelEndpoints allocates a tunnel address and records the
// cluster addresses of every node selected to terminate tunnels. The
// selection is a plain node selector (the claim's tunnelEndpoints,
// passed to this operator), which is what makes a fully connected
// mesh, a single sacrificial test node, or workers-only-by-default all
// the same mechanism with different selectors.
//
// A node's own dialer publishes its public key; this loop never sees
// or wants a private key.
func (r *meshReconciler) reconcileTunnelEndpoints(ctx context.Context) error {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, secretKey, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting secret %s: %w", secretKey, err)
		}
		// The peer Secret is controller-managed state; nothing else has
		// to create it (no manual steps, no gitops-authored Secret for a
		// controller-owned object).
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.secretNamespace, Name: r.secretName, OwnerReferences: r.owners()}}
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("creating peer secret %s: %w", secretKey, err)
		}
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	// Existing allocations stay put; new nodes take the next free host
	// in the tunnel subnet.
	used := map[string]bool{}
	for key, val := range secret.Data {
		if strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) {
			used[strings.SplitN(strings.TrimSpace(string(val)), "/", 2)[0]] = true
		}
	}

	changed := false
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !r.isTunnelEndpoint(node) {
			// A node this operator provisioned is on the far side of a
			// tunnel, not at this site. Its addresses and blocks reach
			// the mesh as a peer entry, and naming it here as well
			// would give the same prefixes a second owner, which the
			// accept list resolves by keeping whichever was written
			// last.
			if node.Labels[cloudWorkerRoleLabel] == cloudWorkerRoleValue {
				continue
			}
			// A site node with no tunnel of its own. It is not a peer,
			// but a remote still has to be permitted to reach it, so
			// its addresses and blocks are published for whichever
			// endpoint relays to it. Without this the remote learns
			// the route and drops the traffic on the way out.
			if r.publishSiteNode(ctx, secret, node) {
				changed = true
			}
			continue
		}
		addrKey := tunnel.NodeTunnelAddressPrefix + node.Name
		if len(secret.Data[addrKey]) == 0 {
			addr, err := nextFreeAddress(r.localAddressBase, used)
			if err != nil {
				return err
			}
			used[strings.SplitN(addr, "/", 2)[0]] = true
			secret.Data[addrKey] = []byte(addr)
			changed = true
		}
		// The node's real addresses, which is what the network's own
		// sessions and kubelet traffic use. The tunnel address alone is
		// not enough for either.
		var addresses []string
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address != "" {
				addresses = append(addresses, a.Address)
			}
		}
		addressKey := tunnel.NodeAddressesPrefix + node.Name
		joined := strings.Join(addresses, ",")
		if joined != "" && string(secret.Data[addressKey]) != joined {
			secret.Data[addressKey] = []byte(joined)
			changed = true
		}

		// The pod blocks this node owns, read from the network's own
		// records and recomputed every pass. A block allocated later
		// reaches the peers from here, which is why nothing about pod
		// addressing is configuration.
		// One node's blocks being unreadable must not discard the
		// whole pass. The Secret is patched once, after this loop, so
		// returning here would throw away every node's tunnel address
		// allocation too, and a dialer with no allocated address
		// configures nothing at all. The condition is permanent for a
		// network this does not recognise, so the mesh would never
		// form rather than forming without one node's pod blocks.
		// publishSiteNode already treats this as "leave what is
		// published"; this is the same judgement.
		publishPods := true
		prefixes, err := r.network.PrefixesFor(ctx, r.reader, node.Name)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "leaving this node's published pod blocks alone", "node", node.Name)
			publishPods = false
		}
		texts := make([]string, 0, len(prefixes))
		for _, prefix := range prefixes {
			texts = append(texts, prefix.String())
		}
		// Side effects on the network's own objects. A failure here
		// costs transit or address pinning for this node; it is not a
		// reason to abandon every other node's allocation.
		if err := r.ensureTransitPeering(ctx, node.Name, firstAddress(addresses)); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "no transit peering for this node", "node", node.Name)
		}

		// Keep this node's own address the one its site reaches it by.
		//
		// A CNI that picks a node's address by looking at its
		// interfaces can pick the tunnel, which no other node at the
		// site can reach. Its neighbours then try to peer with it
		// there, the sessions never establish, and a node that was
		// working loses the pod network it already had, in both
		// directions, while the tunnel itself looks healthy. Bringing
		// up a tunnel must never cost a node something it had.
		if err := r.ensureCNINodeAddress(ctx, node.Name, firstAddress(addresses), ""); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "could not pin this node's address for the network", "node", node.Name)
		}

		podKey := tunnel.NodePodCIDRsPrefix + node.Name
		joinedPods := strings.Join(texts, ",")
		if publishPods && string(secret.Data[podKey]) != joinedPods {
			if joinedPods == "" {
				delete(secret.Data, podKey)
			} else {
				secret.Data[podKey] = []byte(joinedPods)
			}
			changed = true
		}
	}
	// Every control plane's address, for the remote's node-local load
	// balancer: a k0s worker fans its API traffic across all control
	// planes, so publishing only the one the join token points at would
	// leave the remote dependent on that single node staying up.
	var apiServers []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if _, isCP := node.Labels[controlPlaneLabel]; !isCP {
			continue
		}
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address != "" {
				apiServers = append(apiServers, a.Address)
			}
		}
	}
	if joined := strings.Join(apiServers, ","); joined != "" && string(secret.Data[tunnel.APIServersKey]) != joined {
		secret.Data[tunnel.APIServersKey] = []byte(joined)
		changed = true
	}

	if !changed {
		return nil
	}
	return r.Patch(ctx, secret, patch)
}

// isTunnelEndpoint reports whether a node should terminate tunnels:
// Linux, and matching the tunnelEndpoints selector. Control-plane
// nodes are excluded unless the selector names them explicitly, so by
// default the other side of the tunnel does not land on a controller.
func (r *meshReconciler) isTunnelEndpoint(node *corev1.Node) bool {
	if node.Labels["kubernetes.io/os"] == "windows" {
		return false
	}
	// A node this operator provisioned is on the far side of a tunnel,
	// never one of the site's ends of it. It reaches the mesh as a peer
	// and is addressed by its tunnel address, so counting it here would
	// give it a second, contradictory identity: a site dialer, and its
	// own address pinned to the one the site cannot reach it by.
	// "all" means every node at this site, not every node in the
	// cluster, and an empty selector means the same.
	if node.Labels[cloudWorkerRoleLabel] == cloudWorkerRoleValue {
		return false
	}
	if _, isCP := node.Labels[controlPlaneLabel]; isCP && !selectorNamesControlPlane(r.tunnelEndpointsRaw) {
		return false
	}
	if isAllNodes(r.tunnelEndpointsRaw) {
		return true
	}
	if r.tunnelEndpointSelector == nil || r.tunnelEndpointSelector.Empty() {
		return true
	}
	return r.tunnelEndpointSelector.Matches(labels.Set(node.Labels))
}

// selectorNamesControlPlane reports whether the operator was asked,
// explicitly, to place tunnels on control-plane nodes.
func selectorNamesControlPlane(raw string) bool {
	return strings.Contains(raw, controlPlaneLabel) || isAllNodes(raw)
}

// isAllNodes is the one selector a label selector cannot express: every
// node, control planes included. A control plane is left out unless it
// is named, and naming it in a selector that also matches workers is
// not something label syntax allows.
func isAllNodes(raw string) bool {
	switch strings.TrimSpace(raw) {
	case "all", "*":
		return true
	}
	return false
}

// nextFreeAddress returns the next unused host address in base's
// subnet (base is e.g. "10.100.0.1/24": the first endpoint gets
// exactly that, subsequent ones the next free host).
func nextFreeAddress(base string, used map[string]bool) (string, error) {
	ip, ipNet, err := net.ParseCIDR(base)
	if err != nil {
		return "", fmt.Errorf("parsing tunnel address base %q: %w", base, err)
	}
	prefixLen, _ := ipNet.Mask.Size()
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("tunnel address base %q must be IPv4", base)
	}
	for last := int(v4[3]); last < 255; last++ {
		candidate := fmt.Sprintf("%d.%d.%d.%d", v4[0], v4[1], v4[2], last)
		if !used[candidate] {
			return fmt.Sprintf("%s/%d", candidate, prefixLen), nil
		}
	}
	return "", fmt.Errorf("tunnel subnet %s is exhausted", base)
}

// ensureAdoptionConfig renders the live, public-data-only peer list
// for one remote machine into its own Secret. This is the mechanism
// that makes "adoption" mean something: the bootstrap peers.json in
// userdata is a frozen snapshot from provisioning time, while this
// Secret is re-derived from cluster state on every reconcile, and the
// cloud dialer prefers it once readable.
// calicoIPv4Annotation and calicoIPv6Annotation are where Calico's
// Kubernetes datastore keeps a node's BGP address (the Node resource's
// spec.bgp.ipv4Address). Setting them per node is Calico's documented
// alternative to a cluster-wide autodetection method.
const (
	calicoIPv4Annotation = "projectcalico.org/IPv4Address"
	calicoIPv6Annotation = "projectcalico.org/IPv6Address"
)

// ensureCNINodeAddress tells the CNI which address to peer on for a
// provisioned node.
//
// Its real address belongs to a cloud provider and means nothing to
// this cluster; its tunnel address is the one every tunnel endpoint can
// reach by construction, and the one the dialer installs a host route
// for. Autodetection on the node itself cannot know that, so the choice
// is stated here rather than guessed there.
// ensureTransitPeering tells the rest of the site to peer with the
// speaker a tunnel endpoint runs, so it can learn which remote nodes are
// reachable through that endpoint.
//
// The peering is on the speaker's own port, because the node's CNI is
// already using 179. Only nodes other than the endpoint itself take it:
// the endpoint has the tunnel and needs telling by nobody.
//
// This is Calico's way of being told. A network that speaks no routing
// protocol has nothing to configure here, and gets host routes instead.
func (r *meshReconciler) ensureTransitPeering(ctx context.Context, endpoint string, addr string) error {
	if r.transitBGPPort == 0 || r.network.Name != cni.Calico || addr == "" {
		return nil
	}
	name := "cloud-provisioning-transit-" + endpoint
	peer := &unstructured.Unstructured{}
	peer.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "BGPPeer",
	})
	peer.SetName(name)
	spec := map[string]any{
		"peerIP":   fmt.Sprintf("%s:%d", addr, r.transitBGPPort),
		"asNumber": int64(r.transitBGPASN),
		// Every node but the endpoint. The selector is Calico's own
		// syntax, not a Kubernetes label selector.
		"nodeSelector": fmt.Sprintf("kubernetes.io/hostname != '%s'", endpoint),
	}
	if err := unstructured.SetNestedMap(peer.Object, spec, "spec"); err != nil {
		return err
	}
	peer.SetOwnerReferences(r.owners())

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(peer.GroupVersionKind())
	err := r.reader.Get(ctx, types.NamespacedName{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, peer); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating the transit peering for %s: %w", endpoint, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the transit peering for %s: %w", endpoint, err)
	}
	if existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec"); equalSpec(existingSpec, spec) {
		return nil
	}
	existing.Object["spec"] = spec
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating the transit peering for %s: %w", endpoint, err)
	}
	return nil
}

// equalSpec compares the fields this controller sets, leaving anything
// else on the object alone.
func equalSpec(existing, want map[string]any) bool {
	for k, v := range want {
		if fmt.Sprint(existing[k]) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

func (r *meshReconciler) ensureCNINodeAddress(ctx context.Context, nodeName, tunnelAddr, claim string) error {
	if nodeName == "" || tunnelAddr == "" {
		return nil
	}
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting node %s: %w", nodeName, err)
	}
	key, want := calicoIPv4Annotation, tunnelAddr+"/32"
	if strings.Contains(tunnelAddr, ":") {
		key, want = calicoIPv6Annotation, tunnelAddr+"/128"
	}
	// Calico rewrites this with the prefix length the address actually
	// carries on the interface, so only the address is compared. Fixing
	// the mask back every pass would be a fight with the thing being
	// configured.
	if existing := node.Annotations[key]; existing != "" &&
		strings.SplitN(existing, "/", 2)[0] == tunnelAddr {
		if claim == "" || node.Annotations[claimpkg.ClaimAnnotation] == claim {
			return nil
		}
		want = existing
	}
	// The claim is recorded here too, so its teardown can find the
	// Node it produced. A Node is cluster-scoped and a claim is not, so
	// an ownerReference cannot express this.
	if node.Annotations[key] == want && (claim == "" || node.Annotations[claimpkg.ClaimAnnotation] == claim) {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[key] = want
	if claim != "" {
		node.Annotations[claimpkg.ClaimAnnotation] = claim
	}
	if err := r.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("annotating node %s with its tunnel address: %w", nodeName, err)
	}
	return nil
}

// publishSiteNode records a node that terminates no tunnel: what it is
// reachable at, and which pods it holds. Reports whether anything
// changed.
//
// This is the half of the mesh that faces the other way. A site node
// learns about a remote from the endpoint's advertisement; a remote
// learns about a site node from here, because it has no session with
// it to learn from and could not open one.
func (r *meshReconciler) publishSiteNode(ctx context.Context, secret *corev1.Secret, node *corev1.Node) bool {
	var addresses []string
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && a.Address != "" {
			addresses = append(addresses, a.Address)
		}
	}
	changed := false
	set := func(key, value string) {
		if value == "" {
			if _, ok := secret.Data[key]; ok {
				delete(secret.Data, key)
				changed = true
			}
			return
		}
		if string(secret.Data[key]) != value {
			secret.Data[key] = []byte(value)
			changed = true
		}
	}
	set(tunnel.SiteAddressesPrefix+node.Name, strings.Join(addresses, ","))

	prefixes, err := r.network.PrefixesFor(ctx, r.reader, node.Name)
	if err != nil {
		// No blocks yet. Leave what is already published rather than
		// withdrawing it: the remote is using it.
		return changed
	}
	texts := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		texts = append(texts, prefix.String())
	}
	set(tunnel.SitePodCIDRsPrefix+node.Name, strings.Join(texts, ","))
	return changed
}

// publishRemotePodCIDRs keeps a remote machine's peer entry carrying
// its own node's blocks, alongside its tunnel address.
func (r *meshReconciler) publishRemotePodCIDRs(ctx context.Context, machineName, nodeName string) (bool, error) {
	prefixes, err := r.network.PrefixesFor(ctx, r.reader, nodeName)
	if err != nil {
		// No block yet, which is not a failure: the caller comes back.
		return false, nil
	}
	if len(prefixes) == 0 {
		// An encapsulated network needs none, so there is nothing
		// pending and nothing to publish.
		return r.network.Encapsulation == cni.Encapsulated, nil
	}
	// A block the network would masquerade toward is still published:
	// it is the operator's to resolve, and refusing here would leave
	// the node with no reachability at all rather than reachability
	// that fails in one direction. Saying so is what was missing.
	if err := r.network.CheckMasquerade(ctx, r.reader, prefixes); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "the remote's pod block will not survive the network's outgoing NAT", "node", nodeName)
	}
	secret := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, secret); err != nil {
		return false, fmt.Errorf("getting peer secret: %w", err)
	}
	key := tunnel.PeerAllowedIPsPrefix + machineName
	entries := tunnel.SplitList(string(secret.Data[key]))
	var hosts []string
	for _, entry := range entries {
		if strings.HasSuffix(entry, "/32") || strings.HasSuffix(entry, "/128") {
			hosts = append(hosts, entry)
		}
	}
	for _, prefix := range prefixes {
		hosts = append(hosts, prefix.String())
	}
	want := strings.Join(hosts, ",")
	if want == string(secret.Data[key]) {
		return true, nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = []byte(want)
	if err := r.Patch(ctx, secret, patch); err != nil {
		return false, fmt.Errorf("publishing the remote node's pod blocks: %w", err)
	}
	return true, nil
}

func (r *meshReconciler) ensureAdoptionConfig(ctx context.Context, machine *unstructured.Unstructured) error {
	peerSecret := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, peerSecret); err != nil {
		return fmt.Errorf("getting peer secret: %w", err)
	}
	selfTunnelAddr := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"]), "/", 2)[0]
	peers, err := tunnel.RemotePeers(peerSecret.Data, selfTunnelAddr, tunnel.SplitList(string(peerSecret.Data[tunnel.APIServersKey]), r.apiVIP))
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return nil
	}
	doc, err := json.Marshal(tunnel.PeerListDoc{Peers: peers})
	if err != nil {
		return err
	}

	name := tunnel.AdoptionSecretName(machine.GetName())
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.secretNamespace, OwnerReferences: r.owners()},
		Data:       map[string][]byte{tunnel.CloudPeersKey: doc},
	}
	existing := &corev1.Secret{}
	err = r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting adoption secret %s/%s: %w", r.secretNamespace, name, err)
	}
	if string(existing.Data[tunnel.CloudPeersKey]) == string(doc) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	existing.Data[tunnel.CloudPeersKey] = doc
	return r.Patch(ctx, existing, patch)
}

// ensureDialerDaemonSet creates or updates the on-prem dialer
// DaemonSet directly: no CRD, no gitops YAML for its pod spec. Its
// desired state is computed entirely from this operator's own
// constants/flags plus the Secret it manages, so it cannot drift out
// of sync with what this operator expects. A hand-authored DaemonSet's
// AllowedIPs and taint values have no such check.
//
// Scheduling: Linux nodes matching the tunnelEndpoints selector,
// with control-plane nodes excluded by nodeAffinity (see
// controlPlaneLabel) and no toleration for the cloud-worker taint (so
// it never lands on the remote node it dials).
func (r *meshReconciler) ensureDialerDaemonSet(ctx context.Context) error {
	nodeSelectorTerms := []corev1.NodeSelectorRequirement{
		{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
	}
	if !selectorNamesControlPlane(r.tunnelEndpointsRaw) {
		nodeSelectorTerms = append(nodeSelectorTerms, corev1.NodeSelectorRequirement{
			Key: controlPlaneLabel, Operator: corev1.NodeSelectorOpDoesNotExist,
		})
	}
	for _, req := range parseSelectorRequirements(r.tunnelEndpointsRaw) {
		nodeSelectorTerms = append(nodeSelectorTerms, req)
	}
	// A control plane carries a NoSchedule taint, so allowing it by
	// affinity is not enough: without a toleration a selected control
	// plane simply never gets a pod, and the mesh silently omits it.
	var tolerations []corev1.Toleration
	if selectorNamesControlPlane(r.tunnelEndpointsRaw) {
		for _, key := range []string{controlPlaneLabel, "node-role.kubernetes.io/master"} {
			tolerations = append(tolerations, corev1.Toleration{
				Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			})
		}
	}

	hostPathDirectoryOrCreate := corev1.HostPathDirectoryOrCreate
	hostPathDirectory := corev1.HostPathDirectory
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            r.dialerDaemonSetName,
			Namespace:       r.secretNamespace,
			OwnerReferences: r.owners(),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": r.dialerDaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": r.dialerDaemonSetName}},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: r.dialerServiceAccount,
					Tolerations:        tolerations,
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: nodeSelectorTerms}},
							},
						},
					},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: r.dialerImagePullSecret}},
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           r.dialerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
							},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
								{Name: "NODE_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
							},
							Args: []string{
								fmt.Sprintf("--secret-namespace=%s", r.secretNamespace),
								fmt.Sprintf("--secret-name=%s", r.secretName),
								fmt.Sprintf("--iface=%s", r.ifaceName),
								fmt.Sprintf("--private-key-file=%s/private.key", r.dialerPrivateKeyDir),
								fmt.Sprintf("--transit-masquerade-source=%s", r.tunnelSubnet),
								// Advertising the remote nodes to the rest of
								// the site. The next hop is this node itself,
								// which is where their traffic has to arrive.
								fmt.Sprintf("--transit-bgp-port=%d", r.transitBGPPort),
								fmt.Sprintf("--transit-bgp-asn=%d", r.transitBGPASN),
								"--transit-bgp-next-hop=$(NODE_IP)",
								"--keepalive-seconds=15",
								// No --mtu: it is derived from the interface the
								// encapsulated packets leave by, so a number written
								// here would override that with a guess about a
								// network this does not run on.
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "dialer-key", MountPath: r.dialerPrivateKeyDir},
								// The node's real sysctls. A container runtime mounts
								// /proc/sys read-only and NET_ADMIN does not change
								// that, so forwarding and reverse path filtering could
								// be read but never set.
								{Name: "sysctl-net", MountPath: tunnel.HostSysctlNet},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dialer-key",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: r.dialerPrivateKeyDir, Type: &hostPathDirectoryOrCreate},
							},
						},
						{
							Name: "sysctl-net",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/proc/sys/net", Type: &hostPathDirectory},
							},
						},
					},
				},
			},
		},
	}

	existing := &appsv1.DaemonSet{}
	err := r.reader.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting existing daemonset %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

// parseSelectorRequirements turns a plain "k=v,k2=v2" selector string
// into node-affinity requirements. Only equality terms are supported
// (that is all a node placement selector needs here); anything else
// is ignored rather than silently mis-scheduling.
func parseSelectorRequirements(raw string) []corev1.NodeSelectorRequirement {
	// "all" is this project's own word for every node, not a selector.
	// A label parser reads it as "the label all must exist", which no
	// node carries, so the DaemonSet would be scheduled nowhere and
	// every node would look like it had simply declined to publish.
	if isAllNodes(raw) {
		return nil
	}
	// The same parser Kubernetes uses for a label selector, rather than
	// splitting on commas: a set based term ("k in (a,b)") contains
	// commas of its own, so splitting turns one requirement into two
	// fragments that match nothing. Silently, which put a dialer on
	// every node instead of the two the selector named.
	selector, err := labels.Parse(raw)
	if err != nil {
		return nil
	}
	requirements, _ := selector.Requirements()
	var out []corev1.NodeSelectorRequirement
	for _, req := range requirements {
		var op corev1.NodeSelectorOperator
		switch req.Operator() {
		case selection.Equals, selection.DoubleEquals, selection.In:
			op = corev1.NodeSelectorOpIn
		case selection.NotEquals, selection.NotIn:
			op = corev1.NodeSelectorOpNotIn
		case selection.Exists:
			op = corev1.NodeSelectorOpExists
		case selection.DoesNotExist:
			op = corev1.NodeSelectorOpDoesNotExist
		default:
			continue
		}
		values := req.Values().List()
		// A node affinity term with a value and no operator to use it
		// would match everything, so an empty set is only valid for the
		// operators that take none.
		if len(values) == 0 && op != corev1.NodeSelectorOpExists && op != corev1.NodeSelectorOpDoesNotExist {
			continue
		}
		out = append(out, corev1.NodeSelectorRequirement{Key: req.Key(), Operator: op, Values: values})
	}
	return out
}

// ensureCloudDialerDaemonSet creates or updates the remote-side dialer
// DaemonSet: the same binary, scheduled onto the cloud-worker node(s)
// only (nodeSelector + toleration for the cloud-worker taint, the
// inverse of the on-prem DaemonSet's scheduling). It keeps its
// identity from the /etc/wg-dialer/peers.json cloud-init wrote
// (hostPath, read-only, so the private key does not travel through the
// API) but takes its peer list from the per-machine adoption Secret
// this operator re-renders every reconcile, so post-join corrections
// reach a node whose userdata is immutable.
//
// It does not disable the wg-dialer.service systemd unit cloud-init
// installed: both converge on the same kernel interface
// (ConfigureDevice is idempotent; nothing calls LinkDel), and if this
// pod could not schedule, a disabled bootstrap tunnel would leave the
// node with no path back to the API. What the DaemonSet adds is a
// Kubernetes-native upgrade path (bump --dialer-image, rolling update)
// instead of host binary swaps.
func (r *meshReconciler) ensureCloudDialerDaemonSet(ctx context.Context) error {
	hostPathDirectory := corev1.HostPathDirectory
	// Default: the project image, which also self-installs onto the
	// host (the upgrade channel). When the image is not pullable on a
	// remote node, a public base image runs the host binary instead;
	// adoption still works, but it stops being an upgrade channel.
	cloudImage := r.dialerImage
	var cloudCommand []string
	if r.dialerCloudImage != "" {
		cloudImage = r.dialerCloudImage
		cloudCommand = []string{r.dialerCloudHostBinary}
	}
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            r.dialerCloudDaemonSetName,
			Namespace:       r.secretNamespace,
			OwnerReferences: r.owners(),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": r.dialerCloudDaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": r.dialerCloudDaemonSetName}},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: r.dialerServiceAccount,
					NodeSelector: map[string]string{
						cloudWorkerRoleLabel: cloudWorkerRoleValue,
						"kubernetes.io/os":   "linux",
					},
					Tolerations: []corev1.Toleration{
						{Key: cloudWorkerTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: r.dialerImagePullSecret}},
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           cloudImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         cloudCommand,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
							},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
							},
							Args: []string{
								fmt.Sprintf("--iface=%s", r.ifaceName),
								"--peers-file=/etc/wg-dialer/peers.json",
								fmt.Sprintf("--peers-secret-namespace=%s", r.secretNamespace),
								// One shared pod spec, per-machine Secrets: the
								// machine's own name is node-local data
								// (cloud-init wrote it), not a per-node flag
								// baked into this template.
								"--machine-name-file=/etc/wg-dialer/machine-name",
								// The image becomes the upgrade channel once the
								// node has joined: this copy installs itself over
								// the host binary the bootstrap unit runs, so a
								// fleet upgrade is one digest bump in gitops and
								// the download URL only ever mattered at first
								// boot.
								fmt.Sprintf("--listen-port=%s", r.dialerCloudListenPort),
								"--keepalive-seconds=15",
								"--mtu=1420",
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "wg-dialer-config", MountPath: "/etc/wg-dialer", ReadOnly: true},
								{Name: "host-bin", MountPath: "/host-bin"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "wg-dialer-config",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/etc/wg-dialer", Type: &hostPathDirectory},
							},
						},
						{
							Name: "host-bin",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/usr/local/bin", Type: &hostPathDirectory},
							},
						},
					},
				},
			},
		},
	}
	// Self-install is the post-join upgrade channel, and applies only
	// when this DaemonSet runs the project image; a public base image
	// carries no binary of its own to install.
	if r.dialerCloudImage == "" {
		c := &desired.Spec.Template.Spec.Containers[0]
		c.Args = append(c.Args, "--install-host-binary=/host-bin/wg-dialer")
	}

	existing := &appsv1.DaemonSet{}
	err := r.reader.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting existing cloud daemonset %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

func main() {
	var (
		machineSelector  string
		secretNamespace  string
		secretName       string
		secretKey        string
		port             string
		metricsAddr      string
		gatewayNamespace string
		gatewayName      string

		tunnelEndpoints     string
		dialerPrivateKeyDir string

		joinEnabled               bool
		joinTemplatePath          string
		joinAPIAddress            string
		joinAPIVIP                string
		joinKubeletExtraArgs      string
		joinSSHAuthorizedKeys     string
		joinTokenTTL              time.Duration
		joinProviderName          string
		wireGuardAddress          string
		wireGuardListenPort       string
		localAddressBase          string
		dialerListenPort          string
		bootstrapSecretNameFormat string
		dialerDaemonSetName       string
		dialerServiceAccount      string
		dialerImage               string
		dialerImagePullSecret     string
		dialerCloudDaemonSetName  string
		dialerCloudImage          string
		transitBGPPort            int
		transitBGPASN             int
		ownerDeployment           string
		dialerCloudHostBinary     string
		dialerBinaryURLARM64      string
		dialerBinarySHA256ARM64   string
		dialerBinaryURLAMD64      string
		dialerBinarySHA256AMD64   string
		cniPluginsURLARM64        string
		cniPluginsSHA256ARM64     string
		cniPluginsURLAMD64        string
		cniPluginsSHA256AMD64     string
		awsConfigNamespace        string
		awsConfigName             string
	)
	flag.StringVar(&machineSelector, "machine-selector", fmt.Sprintf("%s=%s", cloudWorkerRoleLabel, cloudWorkerRoleValue),
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "cloud-provisioning", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "tunnel-peers", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key-prefix", tunnel.PeerEndpointPrefix, "prefix (Machine name is appended) for the Secret key this Machine's endpoint is written into: per-Machine, not a flat singleton")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")
	flag.StringVar(&tunnelEndpoints, "tunnel-endpoints", "", "node selector (k=v,k2=v2) choosing which local nodes terminate tunnels; empty = every Linux worker. Control-plane nodes are excluded unless this selector names node-role.kubernetes.io/control-plane explicitly")
	flag.StringVar(&dialerPrivateKeyDir, "dialer-private-key-dir", "/var/lib/cloud-provisioning", "host directory where each node's dialer keeps its own WireGuard private key (generated on first start; never leaves the node)")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "tunnel-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "cloud-provisioning-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "", "REQUIRED image for the dialer DaemonSets, pinned by digest (tag@sha256:...). Deliberately has no default: a stale built-in default once pointed at a pre-hardening build")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "", "optional imagePullSecret for the dialer DaemonSets; leave empty when the images are publicly pullable. Naming a Secret that does not exist makes every pod on every endpoint node log a pull-secret warning, so this defaults to none")
	flag.IntVar(&transitBGPPort, "transit-bgp-port", 0, "port for the speaker each tunnel endpoint runs, telling the rest of the site which remote nodes are reachable through it. 0 leaves it off, which is right when every node that needs a remote terminates a tunnel of its own. Not 179: a node whose CNI speaks BGP is already there")
	flag.IntVar(&transitBGPASN, "transit-bgp-asn", 64512, "autonomous system for that speaker; match the cluster's own")
	flag.StringVar(&ownerDeployment, "owner-deployment", "", "this controller's own Deployment name; everything it creates at runtime (both DaemonSets, the peer and adoption Secrets) is owned by it, so an uninstall garbage-collects them instead of orphaning a tunnel interface on every endpoint node")
	flag.StringVar(&dialerCloudImage, "dialer-cloud-image", "", "optional PUBLIC base image for the REMOTE node's DaemonSet, which then executes --dialer-cloud-host-binary instead of carrying its own. Use when the project image is not pullable on a remote node (no preload, no registry credential): without it the adoption DaemonSet ImagePullBackOffs and the node stays on its frozen bootstrap peer list forever")
	flag.StringVar(&dialerCloudHostBinary, "dialer-cloud-host-binary", "/host-bin/wg-dialer", "path (inside the pod) of the host binary --dialer-cloud-image executes; the bootstrap already installed and sha-verified it")
	flag.StringVar(&dialerCloudDaemonSetName, "dialer-cloud-daemonset-name", "tunnel-dialer-remote", "name of the remote-side dialer DaemonSet this operator provisions directly")

	flag.BoolVar(&joinEnabled, "join-enabled", true, "enable bootstrap-secret provisioning (join.Reconciler) and claim expansion: the whole point of this operator; disable only for an endpoint-mirror-only deployment")
	flag.StringVar(&joinProviderName, "join-provider", "k0s", "which cluster technology's join specialization mints join credentials (k0s, kubeadm); pair with the matching --join-template-path")
	flag.StringVar(&joinTemplatePath, "join-template-path", "/join-patterns/k0s-worker.cloud-config.tmpl", "path to the join-pattern template to render")
	flag.StringVar(&joinAPIAddress, "join-api-address", "", "REQUIRED cluster API server address used to mint join tokens (bracket IPv6 literals, e.g. https://[fd8f:cf26:522a::1]:6443)")
	flag.StringVar(&joinAPIVIP, "join-api-vip", "", "REQUIRED cluster API VIP the new node must reach through the tunnel before joining")
	flag.StringVar(&joinKubeletExtraArgs, "join-kubelet-extra-args",
		fmt.Sprintf("--node-labels=%s=%s --register-with-taints=%s:NoSchedule", cloudWorkerRoleLabel, cloudWorkerRoleValue, cloudWorkerTaintKey),
		"extra kubelet args applied to every joining cloud-worker node: defaults derived from the same constants the DaemonSet toleration and --machine-selector default use, so they can't drift")
	flag.StringVar(&joinSSHAuthorizedKeys, "join-ssh-authorized-keys", "", "comma-separated SSH public keys to authorize on every new node")
	flag.DurationVar(&joinTokenTTL, "join-token-ttl", 2*time.Hour, "validity window for a minted join token")
	flag.StringVar(&wireGuardAddress, "join-wireguard-address", "10.100.0.128/24", "base WireGuard tunnel address for REMOTE (cloud) nodes; each gets the next free address in this subnet")
	flag.StringVar(&localAddressBase, "tunnel-local-address-base", "10.100.0.1/24", "base WireGuard tunnel address for LOCAL tunnel-endpoint nodes; each selected node gets the next free address in this subnet")
	flag.StringVar(&wireGuardListenPort, "join-wireguard-listen-port", "51820", "WireGuard listen port on the remote side")
	flag.StringVar(&dialerListenPort, "join-dialer-listen-port", "51820", "WireGuard listen port the local dialers expect the remote peer to use")
	flag.StringVar(&bootstrapSecretNameFormat, "join-bootstrap-secret-name-format", "%s-bootstrap", "printf format (with the Machine's name) for the bootstrap Secret's name")
	flag.StringVar(&dialerBinaryURLARM64, "join-dialer-binary-url-arm64", "", "REQUIRED (arm64 nodes) URL cloud-init downloads the dialer binary from; nothing installs it on a stock image")
	flag.StringVar(&dialerBinarySHA256ARM64, "join-dialer-binary-sha256-arm64", "", "REQUIRED (arm64 nodes) sha256 of that binary, verified by cloud-init before the tunnel unit starts")
	flag.StringVar(&dialerBinaryURLAMD64, "join-dialer-binary-url-amd64", "", "URL cloud-init downloads the amd64 dialer binary from")
	flag.StringVar(&dialerBinarySHA256AMD64, "join-dialer-binary-sha256-amd64", "", "sha256 of the amd64 binary")
	flag.StringVar(&cniPluginsURLARM64, "join-cni-plugins-url-arm64", "", "optional per-arch containernetworking-plugins tarball URL; required when the cluster's CNI config chains plugins (bandwidth/portmap/tuning) a stock cloud image does not ship")
	flag.StringVar(&cniPluginsSHA256ARM64, "join-cni-plugins-sha256-arm64", "", "sha256 of the arm64 CNI plugins tarball; a URL without it is ignored, never fetched unverified")
	flag.StringVar(&cniPluginsURLAMD64, "join-cni-plugins-url-amd64", "", "amd64 containernetworking-plugins tarball URL")
	flag.StringVar(&cniPluginsSHA256AMD64, "join-cni-plugins-sha256-amd64", "", "sha256 of the amd64 CNI plugins tarball")
	flag.StringVar(&awsConfigNamespace, "aws-config-namespace", "cloud-provisioning", "namespace of the AWS provider-config Secret (AMIs, subnet, security groups, keypair)")
	flag.StringVar(&awsConfigName, "aws-config-name", "aws-provider-config", "name of the AWS provider-config Secret")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// The dialer image has no discoverable value and no safe default.
	if strings.TrimSpace(dialerImage) == "" {
		fmt.Fprintf(os.Stderr, "--dialer-image is required\n")
		os.Exit(1)
	}

	selector, err := labels.Parse(machineSelector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --machine-selector: %v\n", err)
		os.Exit(1)
	}
	rawTunnelEndpoints := tunnelEndpoints
	if isAllNodes(tunnelEndpoints) {
		// Not a label selector, so it must not be parsed as one; the
		// reconciler still sees the original, which is what tells it
		// control planes are wanted.
		tunnelEndpoints = ""
	}
	endpointSelector, err := labels.Parse(tunnelEndpoints)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --tunnel-endpoints: %v\n", err)
		os.Exit(1)
	}

	if err := v1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "unable to register claim types: %v\n", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to start manager: %v\n", err)
		os.Exit(1)
	}

	// Resolve our own Deployment to own everything created at runtime.
	// Uninstalling the release then garbage-collects the DaemonSets and
	// the Secrets, instead of leaving a tunnel interface on every
	// endpoint node with no controller behind it.
	var runtimeOwner *metav1.OwnerReference
	if ownerDeployment != "" {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create clientset to resolve owner: %v\n", err)
			os.Exit(1)
		}
		dep, err := clientset.AppsV1().Deployments(secretNamespace).Get(context.Background(), ownerDeployment, metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to resolve owner Deployment %s/%s: %v\n", secretNamespace, ownerDeployment, err)
			os.Exit(1)
		}
		runtimeOwner = &metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "Deployment",
			Name: dep.Name, UID: dep.UID,
		}
	}

	var network cni.Network
	// Read from the cluster whatever was not configured. These are all
	// facts the cluster already holds, and a second copy of them in
	// values is a copy that goes stale.
	{
		discoveryClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to build a client for cluster discovery: %v\n", err)
			os.Exit(1)
		}
		ctx := context.Background()
		if joinAPIAddress == "" || joinAPIVIP == "" {
			servers, err := discover.APIServers(ctx, discoveryClient)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cannot determine the API server address (set --join-api-address): %v\n", err)
				os.Exit(1)
			}
			if joinAPIAddress == "" {
				joinAPIAddress = "https://" + servers[0]
			}
			if joinAPIVIP == "" {
				host, _, err := net.SplitHostPort(servers[0])
				if err != nil {
					fmt.Fprintf(os.Stderr, "cannot split the API server address %q: %v\n", servers[0], err)
					os.Exit(1)
				}
				joinAPIVIP = host
			}
		}
		network, err = cni.Detect(ctx, discoveryClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine the container network: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "cluster: api=%s network=%s/%s (%s)\n",
			joinAPIAddress, network.Name, network.Encapsulation, network.Detail)
	}

	// The mesh's interface name is derived from the peer Secret's
	// identity: deterministic on every member, unique per mesh, and
	// never colliding with a node's existing wg0/tailscale devices.
	ifaceName := tunnel.InterfaceName(secretNamespace + "/" + secretName)
	tunnelSubnet := subnetOf(localAddressBase)

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)

	err = ctrl.NewControllerManagedBy(mgr).
		Named("mesh").
		For(machine, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return selector.Matches(labels.Set(obj.GetLabels()))
		}))).
		// Node changes (a new worker joining, a label added) change the
		// tunnel-endpoint set, so they trigger allocation. Otherwise a
		// newly-selected node waits for an unrelated Machine event
		// before it gets a tunnel address.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			return []reconcile.Request{{}}
		})).
		Complete(&meshReconciler{
			Client:                 mgr.GetClient(),
			reader:                 mgr.GetAPIReader(),
			secretNamespace:        secretNamespace,
			secretName:             secretName,
			secretKey:              secretKey,
			port:                   port,
			gatewayNamespace:       gatewayNamespace,
			gatewayName:            gatewayName,
			tunnelEndpointSelector: endpointSelector,
			tunnelEndpointsRaw:     rawTunnelEndpoints,
			tunnelSubnet:           tunnelSubnet,
			localAddressBase:       localAddressBase,
			dialerDaemonSetName:    dialerDaemonSetName,
			dialerServiceAccount:   dialerServiceAccount,
			dialerImage:            dialerImage,
			dialerImagePullSecret:  dialerImagePullSecret,
			dialerPrivateKeyDir:    dialerPrivateKeyDir,
			ifaceName:              ifaceName,
			apiVIP:                 joinAPIVIP,

			ownerRef:                 runtimeOwner,
			network:                  network,
			transitBGPPort:           transitBGPPort,
			transitBGPASN:            transitBGPASN,
			dialerCloudDaemonSetName: dialerCloudDaemonSetName,
			dialerCloudListenPort:    dialerListenPort,
			dialerCloudImage:         dialerCloudImage,
			dialerCloudHostBinary:    dialerCloudHostBinary,
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to create mesh controller: %v\n", err)
		os.Exit(1)
	}

	if joinEnabled {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create clientset for join reconciler: %v\n", err)
			os.Exit(1)
		}
		var sshKeys []string
		for _, k := range strings.Split(joinSSHAuthorizedKeys, ",") {
			if k = strings.TrimSpace(k); k != "" {
				sshKeys = append(sshKeys, k)
			}
		}
		awsProvider := joinaws.Provider{ConfigNamespace: awsConfigNamespace, ConfigName: awsConfigName}
		dockerProvider := joindocker.Provider{ConfigNamespace: secretNamespace, ConfigName: "docker-provider-config"}

		// Each cluster technology is one implementation of
		// join.ClusterJoinProvider behind the same seam. Selection is by
		// name, and each implementation's own knobs live in its own
		// provider-config Secret (the aws-provider-config pattern),
		// not in this binary's generic flags.
		var joinProvider join.ClusterJoinProvider
		switch joinProviderName {
		case "k0s":
			joinProvider = &joink0s.Provider{
				Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL,
				ConfigNamespace: secretNamespace, ConfigName: "k0s-provider-config",
			}
		case "kubeadm":
			joinProvider = &joinkubeadm.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL}
		default:
			fmt.Fprintf(os.Stderr, "unknown --join-provider %q (registered specializations: k0s, kubeadm)\n", joinProviderName)
			os.Exit(1)
		}

		joinReconciler := &join.Reconciler{
			Client:         mgr.GetClient(),
			Reader:         mgr.GetAPIReader(),
			Join:           joinProvider,
			InfraProviders: []join.InfraProvider{awsProvider, dockerProvider},

			TemplatePath:      joinTemplatePath,
			APIVIP:            joinAPIVIP,
			KubeletExtraArgs:  joinKubeletExtraArgs,
			SSHAuthorizedKeys: sshKeys,

			WireGuardAddress:    wireGuardAddress,
			WireGuardListenPort: wireGuardListenPort,

			DialerPeerSecretNamespace: secretNamespace,
			DialerPeerSecretName:      secretName,
			DialerListenPort:          dialerListenPort,

			InterfaceName: ifaceName,

			DialerBinaryURLARM64:    dialerBinaryURLARM64,
			DialerBinarySHA256ARM64: dialerBinarySHA256ARM64,
			DialerBinaryURLAMD64:    dialerBinaryURLAMD64,
			DialerBinarySHA256AMD64: dialerBinarySHA256AMD64,

			CNIPluginsURLARM64:    cniPluginsURLARM64,
			CNIPluginsSHA256ARM64: cniPluginsSHA256ARM64,
			CNIPluginsURLAMD64:    cniPluginsURLAMD64,
			CNIPluginsSHA256AMD64: cniPluginsSHA256AMD64,

			BootstrapSecretNameFormat: bootstrapSecretNameFormat,
		}

		joinMachine := &unstructured.Unstructured{}
		joinMachine.SetGroupVersionKind(machineGVK)
		err = ctrl.NewControllerManagedBy(mgr).
			Named("join").
			For(joinMachine, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return selector.Matches(labels.Set(obj.GetLabels()))
			}))).
			Complete(joinReconciler)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create join controller: %v\n", err)
			os.Exit(1)
		}

		err = ctrl.NewControllerManagedBy(mgr).
			Named("claim").
			For(&v1alpha1.ProvisionedNodeClaim{}).
			Complete(&claim.Reconciler{
				Client:                    mgr.GetClient(),
				Reader:                    mgr.GetAPIReader(),
				Provisioners:              []join.MachineProvisioner{awsProvider, dockerProvider},
				RoleLabel:                 cloudWorkerRoleLabel,
				RoleValue:                 cloudWorkerRoleValue,
				BootstrapSecretNameFormat: bootstrapSecretNameFormat,
				TunnelInterface:           ifaceName,
			})
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create claim controller: %v\n", err)
			os.Exit(1)
		}
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "problem running manager: %v\n", err)
		os.Exit(1)
	}
}

// subnetOf turns "10.100.0.1/24" into "10.100.0.0/24", the tunnel
// subnet the transit masquerade rule is scoped to.
func subnetOf(base string) string {
	_, ipNet, err := net.ParseCIDR(base)
	if err != nil {
		return ""
	}
	return ipNet.String()
}
