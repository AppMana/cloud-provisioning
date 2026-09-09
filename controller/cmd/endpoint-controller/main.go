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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/claim"
	claimpkg "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/discover"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joincontainernet "github.com/appmana/cloud-provisioning/controller/pkg/join/containernet"
	joindocker "github.com/appmana/cloud-provisioning/controller/pkg/join/docker"
	joink0s "github.com/appmana/cloud-provisioning/controller/pkg/join/k0s"
	joink3s "github.com/appmana/cloud-provisioning/controller/pkg/join/k3s"
	joinkubeadm "github.com/appmana/cloud-provisioning/controller/pkg/join/kubeadm"
	joinmicrok8s "github.com/appmana/cloud-provisioning/controller/pkg/join/microk8s"
	joinrke2 "github.com/appmana/cloud-provisioning/controller/pkg/join/rke2"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	reader client.Reader
	// machineSelector is which Machines this operator owns, so a
	// mesh-wide pass refreshes those and no others.
	machineSelector  labels.Selector
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
	// endpointRetention is how long a node that has left the selector
	// goes on being an endpoint: dialer scheduled, entries published,
	// tunnel carrying traffic. It is the time a remote is given to read
	// a peer list naming its replacement, over the tunnel it is about to
	// lose, so it is measured in the dialer's poll interval and never in
	// anything about the departing node's health.
	endpointRetention time.Duration

	// Dialer DaemonSets: this operator owns both specs directly. There
	// is no CRD and they are not hand-authored in gitops.
	dialerDaemonSetName   string
	dialerServiceAccount  string
	dialerImage           string
	dialerImagePullSecret string
	dialerImagePullPolicy corev1.PullPolicy
	dialerPrivateKeyDir   string
	ifaceName             string
	apiVIP                string
	// apiServerPort makes the api-servers record dialable for the
	// remote's loopback balancer; read from the join API address, not
	// restated as configuration.
	apiServerPort string

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
	dialerCloudImage          string
	dialerCloudHostBinary     string
	windowsPublisherImage     string
	windowsPublisherAPIServer string
}

// owners returns the ownerReference list to stamp on everything this
// controller creates, so an uninstall garbage-collects it.
// apiServerPortOf reads the API port out of the address the join
// dials, defaulting to Kubernetes' own 6443: one fact, one source.
func apiServerPortOf(apiAddress string) string {
	if u, err := url.Parse(apiAddress); err == nil && u.Port() != "" {
		return u.Port()
	}
	return "6443"
}

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

func (r *meshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := ctrl.LoggerFrom(ctx)

	// A retained endpoint is released on a deadline, and a deadline is
	// not an event: nothing about any node or Machine changes when the
	// retention window runs out, so without a requeue the departed
	// node's entries would sit published until the next full resync,
	// hours away. Whichever deadline is nearer wins.
	releaseIn := time.Duration(0)
	defer func() {
		if err != nil || releaseIn <= 0 {
			return
		}
		if result.RequeueAfter == 0 || releaseIn < result.RequeueAfter {
			result.RequeueAfter = releaseIn
		}
	}()

	// Allocate tunnel addresses and cluster VIPs for every selected
	// endpoint node first: the peer graph the dialers and the join
	// reconciler read is derived from these, and a node that hasn't
	// been allocated one is not a mesh member.
	releaseIn, err = r.reconcileTunnelEndpoints(ctx)
	if err != nil {
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
	if err := r.ensureWindowsPublisherDaemonSet(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Windows peer publisher: %w", err)
	}

	// Node events enqueue a nameless request: they change the endpoint
	// set rather than any one Machine. An empty-name Get would be a
	// non-NotFound error, an infinite error requeue rather than a
	// no-op, so the Machine-scoped work below is skipped.
	//
	// But every remote reads its own peer list, and that list names the
	// endpoints. Changing which nodes hold tunnels therefore changes
	// what every remote must be told, and nothing else will tell them:
	// no Machine has changed, so no Machine event follows. Measured, a
	// tunnel moved from one site node to another and the site converged
	// while the remote went on naming a node that no longer ran a
	// dialer, its handshake ten minutes stale, until the next resync
	// hours later.
	if req.Name == "" {
		return ctrl.Result{}, r.refreshAdoptionConfigs(ctx)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.refreshAdoptionConfigs(ctx)
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

	// The Role admitting this machine's adoption Secret, on the same
	// event: a machine created moments ago must be readable by name
	// before its dialer's first adoption read, and the nameless pass
	// that would otherwise add it has no reason to run. After the
	// adoption config, not before: a Role write refused (an upgrade
	// window where this controller outruns its own RBAC) must not
	// keep the peer list stale too.
	if err := r.ensureDialerRole(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("scoping the dialer Role: %w", err)
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
	// Pending until proven otherwise, which includes the case where
	// this machine has no node yet.
	//
	// A node has no blocks until something is scheduled on it, and it
	// has no node at all until it joins — both after the Machine
	// stopped changing. Gating the requeue on the node already being
	// known therefore never comes back for the one case that needs it:
	// the machine is reconciled while the node is still absent, sets
	// nothing pending, and is never reconciled again. The blocks are
	// never published, the site cannot reach a pod on the remote, and
	// every component reports healthy.
	//
	// This was hidden for as long as the node was found through
	// status.nodeRef, because Cluster API writing that field was
	// itself a Machine event: the data source doubled as the trigger.
	remoteBlocksPending := true
	if nodeName := r.nodeNameForMachine(ctx, machine); nodeName != "" {
		published, err := r.publishRemotePodCIDRs(ctx, machine.GetName(), nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		remoteBlocksPending = !published
	}

	// Tell the CNI which address to peer on, once the node exists.
	if err := r.ensureCNINodeAddressForMachine(ctx, machine); err != nil {
		return ctrl.Result{}, err
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
	//
	// The same address joins the machine's accept list and route
	// hosts. It is one fact, learned here: the machine's node address.
	// An encapsulating network addresses its packets to exactly it
	// (flannel's vxlan outers, cilium's tunnel outers), so the site's
	// dialers must both accept it through cryptokey routing and route
	// it into the tunnel; the dialer's fwmark is what makes that route
	// safe for the address the tunnel also dials. The join reconciler
	// cannot write these at render time because the address does not
	// exist yet; this mirror is where it first becomes known.
	machineKey := r.secretKey + machine.GetName()
	allowedKey := tunnel.PeerAllowedIPsPrefix + machine.GetName()
	routeHostsKey := tunnel.PeerRouteHostsPrefix + machine.GetName()
	allowed := tunnel.SplitList(string(secret.Data[allowedKey]))
	routeHosts := tunnel.SplitList(string(secret.Data[routeHostsKey]))
	wantAllowed := appendMissing(allowed, tunnel.HostCIDR(externalIP))
	wantRouteHosts := appendMissing(routeHosts, externalIP)
	if string(secret.Data[machineKey]) != endpoint ||
		len(wantAllowed) != len(allowed) || len(wantRouteHosts) != len(routeHosts) {
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[machineKey] = []byte(endpoint)
		secret.Data[allowedKey] = []byte(strings.Join(wantAllowed, ","))
		secret.Data[routeHostsKey] = []byte(strings.Join(wantRouteHosts, ","))
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

// appendMissing appends value to list unless an equal entry is
// already there, preserving whatever order the list arrived in: the
// mirror only ever adds the one fact it owns, never rewrites the
// render's.
func appendMissing(list []string, value string) []string {
	for _, have := range list {
		if strings.TrimSpace(have) == value {
			return list
		}
	}
	return append(append([]string{}, list...), value)
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
func (r *meshReconciler) reconcileTunnelEndpoints(ctx context.Context) (time.Duration, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return 0, fmt.Errorf("listing nodes: %w", err)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, secretKey, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("getting secret %s: %w", secretKey, err)
		}
		// The peer Secret is controller-managed state; nothing else has
		// to create it (no manual steps, no gitops-authored Secret for a
		// controller-owned object).
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.secretNamespace, Name: r.secretName, OwnerReferences: r.owners()}}
		if err := r.Create(ctx, secret); err != nil {
			return 0, fmt.Errorf("creating peer secret %s: %w", secretKey, err)
		}
	}
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	// What the nodes no longer account for, before anything is
	// allocated: a departed endpoint's address is retired in the same
	// pass, so the loop below cannot hand it straight to another node.
	now := time.Now()
	remoteChanged, err := r.pruneDeletedMachines(ctx, secret)
	if err != nil {
		return 0, err
	}
	changed := pruneDeparted(secret.Data, r.membership(nodes.Items), now, r.endpointRetention, r.convergedMachines(ctx, secret.Data)) || remoteChanged

	// Existing allocations stay put; new nodes take the next free host
	// in the tunnel subnet. A retired address is not free, and neither
	// is one reserved to a node that is not currently an endpoint: that
	// node keeps it, so that being selected again makes it the same peer
	// rather than a new one.
	used := map[string]bool{}
	for key, val := range secret.Data {
		if strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) ||
			strings.HasPrefix(key, tunnel.TunnelAddressReservationPrefix) {
			used[strings.SplitN(strings.TrimSpace(string(val)), "/", 2)[0]] = true
		}
	}
	for _, addr := range tunnel.SplitList(string(secret.Data[tunnel.RetiredTunnelAddressesKey])) {
		used[addr] = true
	}

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
			// A node the selector has let go of, still inside its
			// retention window, is still an endpoint: it holds a
			// published key and address, its dialer still runs, and its
			// tunnel still carries traffic.
			//
			// But it stops being the way to reach anything at once. The
			// window exists so a remote can move off this node before
			// its tunnel goes, and a remote can only move if something
			// else already owns the prefixes it needs. Leaving them here
			// meant the handover happened at the same instant as the
			// teardown: the node was unpublished, its interface swept,
			// and only then did its addresses appear on a surviving
			// endpoint, by which time the remote had no path left to
			// learn that. Measured on cp, which owned the API server's
			// address: at 14:36:11 the remote permitted 10.10.0.10 on
			// cp's peer entry and reached the API; at 14:36:17 cp's
			// interface was gone, the remote still named cp for
			// 10.10.0.10, and it could not read the list that would have
			// told it otherwise. It stayed that way for ten minutes.
			//
			// So a departing endpoint keeps its key and its tunnel
			// address, which is what keeps its tunnel usable, and its
			// node addresses and pod blocks move to the site entries
			// now, where a surviving endpoint relays them. One owner per
			// prefix throughout: they are published here or there, never
			// both.
			if publishedEndpoint(secret.Data, node.Name) {
				if len(secret.Data[tunnel.NodeDepartedAtPrefix+node.Name]) == 0 {
					continue
				}
				if r.publishSiteNode(ctx, secret, node) {
					changed = true
				}
				if stopOwningAsEndpoint(secret.Data, node.Name) {
					changed = true
				}
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
		// This node owns its own prefixes now, so nothing relays them.
		// A node that returns to the selector takes back what it handed
		// over when it left.
		//
		// Only once it is a peer as published, though. Selection makes
		// this operator allocate a tunnel address; it is the node's own
		// dialer that publishes the key, and RemotePeers renders a node
		// only when both are there. Taking the relayed entry away at
		// selection meant the node's address and pod block belonged to
		// nobody in the render until that key landed, which is however
		// long the dialer takes to schedule and come up. A remote
		// reading the list in that window prunes the route and cannot
		// read the correction, because the site is only reachable over
		// what it just pruned. Measured on remote2: the host routes for
		// all three site nodes were removed, and the API server was
		// unreadable for 26 minutes.
		if takeBackFromRelay(secret.Data, node.Name) {
			changed = true
		}
		addrKey := tunnel.NodeTunnelAddressPrefix + node.Name
		reservationKey := tunnel.TunnelAddressReservationPrefix + node.Name
		if len(secret.Data[addrKey]) == 0 {
			// The address this node already holds, if it has ever held
			// one. A node returning to the selector is the same peer it
			// was, so every remote's configuration for it is still
			// correct and nothing has to converge.
			addr := strings.TrimSpace(string(secret.Data[reservationKey]))
			if addr == "" {
				var err error
				addr, err = nextFreeAddress(r.localAddressBase, used)
				if err != nil {
					return 0, err
				}
			}
			used[strings.SplitN(addr, "/", 2)[0]] = true
			secret.Data[addrKey] = []byte(addr)
			changed = true
		}
		if !bytes.Equal(secret.Data[reservationKey], secret.Data[addrKey]) {
			secret.Data[reservationKey] = secret.Data[addrKey]
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
	if joined := strings.Join(canonicalAPIHosts(apiServers...), ","); joined != "" && string(secret.Data[tunnel.APIServersKey]) != joined {
		secret.Data[tunnel.APIServersKey] = []byte(joined)
		changed = true
	}

	// When a retained endpoint is due to be released, come back for it:
	// see Reconcile, where a deadline is turned into a requeue.
	releaseIn := soonestRelease(secret.Data, now, r.endpointRetention)
	if !changed {
		return releaseIn, nil
	}
	return releaseIn, r.Patch(ctx, secret, patch)
}

// soonestRelease is how long until the nearest retained endpoint may be
// pruned, or zero when none is retained.
//
// A second past the deadline rather than exactly on it, because a timer
// that fires a hair early reads as not yet expired and would cost
// another whole window. A record whose window has already run out and
// which this pass did not prune is waiting on something else entirely
// (no surviving endpoint is published yet), so it comes back on the
// dialer's own cadence rather than as fast as the queue will run.
func soonestRelease(data map[string][]byte, now time.Time, retention time.Duration) time.Duration {
	const stillWaiting = 30 * time.Second
	soonest := time.Duration(0)
	for key, val := range data {
		if !strings.HasPrefix(key, tunnel.NodeDepartedAtPrefix) {
			continue
		}
		since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(val)))
		if err != nil {
			continue
		}
		remaining := since.Add(retention).Sub(now) + time.Second
		if remaining <= 0 {
			remaining = stillWaiting
		}
		if soonest == 0 || remaining < soonest {
			soonest = remaining
		}
	}
	return soonest
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

// meshMembership is what the nodes say the mesh contains: which of
// them terminate a tunnel, and which are at this site with no tunnel
// of their own. A node this operator provisioned is in neither: it is
// not at this site at all, and reaches the mesh as a peer entry keyed
// by its Machine name, which none of this owns.
type meshMembership struct {
	endpoints map[string]bool
	siteNodes map[string]bool
}

// membership reads that off the node list. Existence and the endpoint
// selector are its only inputs, and neither is a health signal: a
// NotReady node, a node whose dialer pod is not running, and a node
// whose handshake has gone stale are all members here and keep
// everything they published. See pruneDeparted for why.
func (r *meshReconciler) membership(nodes []corev1.Node) meshMembership {
	want := meshMembership{endpoints: map[string]bool{}, siteNodes: map[string]bool{}}
	for i := range nodes {
		node := &nodes[i]
		switch {
		case r.isTunnelEndpoint(node):
			want.endpoints[node.Name] = true
		case node.Labels[cloudWorkerRoleLabel] == cloudWorkerRoleValue:
		default:
			want.siteNodes[node.Name] = true
		}
	}
	return want
}

// pruneDeparted removes what a node published once it is no longer the
// thing it published as, and reports whether it changed anything.
//
// Nothing did this, and the cost was measured. Moving the tunnel from
// one site node to another left the departed endpoint's key and tunnel
// address in the Secret, so every remote's derived peer list still
// named a node running no dialer and never learned the one that was.
// A remote whose only path to the API server is that peer goes
// NotReady, and on a real cloud it cannot be recovered without
// out-of-band access.
//
// What counts as departed is intent, never health. An operator who
// changes the selector, deletes a node, or drains it out of the
// cluster has said so declaratively, and acting on it promptly is
// doing as asked. NotReady, a dialer pod that is not running and a
// handshake that has gone stale all mean the node is expected back:
// removing its entry churns every remote's configuration, and forces
// the mesh to reconverge, while the fault is probably elsewhere. This
// is transitSpeaker.reconcile's judgement applied to the other half of
// the mesh. There is deliberately no horizon after which a sick node
// is evicted, because no number tells a long outage from a long
// maintenance, and the operator already has a way to say which it is.
//
// Two orderings within one pass are load bearing:
//
//   - No node-* entry goes until some surviving endpoint is fully
//     published, meaning it has both its own key and an allocated
//     address. A remote left with a peer it cannot reach is stale; a
//     remote left with no peer at all is stranded, and only the second
//     is unrecoverable from the far side.
//   - No node-* entry goes in the pass that first finds the node
//     departed. Publishing the replacement and withdrawing the current
//     endpoint together is what stranded a remote for eighteen minutes:
//     the sentence naming the replacement has to travel over the
//     endpoint being withdrawn. The instant of departure is recorded
//     instead, and the entries go once retention has elapsed, which is
//     time measured in the dialer's poll interval rather than in
//     anything about the node's health. A node the selector takes back
//     before then has the record cleared and never notices. A node that
//     has left the cluster is not retained at all: nothing can schedule
//     a dialer on it, so keeping its entries offers a remote a peer
//     that no longer answers.
//   - A site-* entry goes only once that same node's endpoint
//     publication is complete, or the node is gone from the cluster. A
//     node in the middle of becoming an endpoint is still reached by
//     relaying through the current one, and dropping its site entry
//     first would take away reachability it already had. It also must
//     go then rather than later: the accept list has one owner per
//     prefix, and the node's addresses would otherwise be permitted
//     both on its own peer and on the relaying one.
func pruneDeparted(data map[string][]byte, want meshMembership, now time.Time, retention time.Duration, converged map[string]map[string]bool) bool {
	if len(want.endpoints) == 0 && len(want.siteNodes) == 0 {
		// A membership that reads as empty is a failed read until
		// proven otherwise. Nothing at a site departs all at once, and
		// believing this reading empties the mesh.
		return false
	}
	survivors := 0
	for name := range want.endpoints {
		if publishedEndpoint(data, name) {
			survivors++
		}
	}
	changed := false
	drop := func(key string) {
		if _, ok := data[key]; ok {
			delete(data, key)
			changed = true
		}
	}
	for _, name := range publishedNames(data,
		tunnel.NodePublicKeyPrefix, tunnel.NodeTunnelAddressPrefix,
		tunnel.NodeAddressesPrefix, tunnel.NodePodCIDRsPrefix,
		tunnel.NodeDepartedAtPrefix) {
		if want.endpoints[name] {
			// Back in the selector. Whatever it was part way through is
			// abandoned, and it is an endpoint like any other.
			drop(tunnel.NodeDepartedAtPrefix + name)
			continue
		}
		if survivors == 0 {
			// Nowhere for a remote to go, so there is nothing for it to
			// be given time to learn. The clock starts when a
			// replacement exists, not when the selector changed.
			continue
		}
		if want.siteNodes[name] {
			// Still a node of this site, merely no longer selected. It
			// keeps its dialer and its entries for the retention window,
			// which is what gives a remote two working paths across the
			// change instead of none.
			expired, recorded := departedLongEnough(data, name, now, retention)
			if recorded {
				changed = true
			}
			if !expired {
				continue
			}
			// The window is a floor, not a ceiling. A clock cannot know
			// whether a remote read the list naming the replacement:
			// a controller outage or a slow render can eat the whole
			// window, and releasing on schedule then strands every
			// remote still holding only the departed node. Held until
			// each remote shows a handshake with a current endpoint
			// after the departure, which is the read made observable.
			if !everyRemoteAcknowledged(data, converged, name) {
				continue
			}
		}
		// A node that is not at this site at all has been deleted or
		// drained out of the cluster. Nothing can schedule a dialer on
		// it, so retaining its entries would buy a remote only a peer
		// that no longer answers.
		//
		// Only such a node loses its address. One that is merely no
		// longer selected is still a node of this site and keeps its
		// reservation, so being selected again makes it the peer it
		// already was rather than a new one. Retiring on deselection is
		// what turned every placement change into a new identity for
		// every node it touched.
		if !want.siteNodes[name] {
			if retireTunnelAddress(data, string(data[tunnel.NodeTunnelAddressPrefix+name])) {
				changed = true
			}
			drop(tunnel.TunnelAddressReservationPrefix + name)
		}
		drop(tunnel.NodePublicKeyPrefix + name)
		drop(tunnel.NodeTunnelAddressPrefix + name)
		drop(tunnel.NodeAddressesPrefix + name)
		drop(tunnel.NodePodCIDRsPrefix + name)
		drop(tunnel.NodeDepartedAtPrefix + name)
	}
	for _, name := range publishedNames(data, tunnel.SiteAddressesPrefix, tunnel.SitePodCIDRsPrefix) {
		// A retained endpoint is still an endpoint: it carries its own
		// addresses on its own peer, and the same addresses on a site
		// entry would be a second owner for those prefixes.
		if want.siteNodes[name] && !publishedEndpoint(data, name) {
			continue
		}
		if want.endpoints[name] && !publishedEndpoint(data, name) {
			continue
		}
		drop(tunnel.SiteAddressesPrefix + name)
		drop(tunnel.SitePodCIDRsPrefix + name)
	}
	return changed
}

// departedLongEnough reports whether a node left the selector far
// enough back that a remote has had a real chance to read the peer list
// naming its replacement, and whether this call had to write the
// departure record. The first pass that finds a node departed records
// the instant and reports not yet.
//
// A record this cannot parse is rewritten as now rather than read as
// expired: the recoverable reading of unreadable state is the one that
// keeps the endpoint, and an endpoint retained one window too long
// costs a remote nothing but a second working tunnel.
// convergedMachines reads each remote's acknowledgment: the hash it
// stamped on its adoption Secret after applying a list, compared with
// the hash of what that Secret carries now. Content against content;
// an unreadable Secret is no acknowledgment, never a failure of the
// pass that asks.
func (r *meshReconciler) convergedMachines(ctx context.Context, data map[string][]byte) map[string]map[string]bool {
	converged := map[string]map[string]bool{}
	for key := range data {
		if !strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) {
			continue
		}
		machine := strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix)
		adoption := &corev1.Secret{}
		if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(machine)}, adoption); err != nil {
			continue
		}
		raw, ok := adoption.Data[tunnel.CloudPeersKey]
		if !ok || len(raw) == 0 {
			continue
		}
		// A stale stamp acknowledges nothing: the machine is still
		// holding some older list, whatever it says.
		if adoption.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(raw) {
			continue
		}
		var doc tunnel.PeerListDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		converged[machine] = relaysFromApplied(doc, data)
	}
	return converged
}

// relaysFromApplied reports, per site node, whether an applied peer
// list carries that node's addresses on a relay rather than on the
// node's own entry.
//
// This is the content behind an acknowledgment. A departing endpoint
// is held until every remote has moved, and "moved" cannot mean "has
// applied its current list": right after a placement change the
// current list is the one the remote applied long ago, still routing
// the departing node directly, so a freshness test releases the node
// the moment before the remote learns where it went. The node then
// loses its own tunnel while the remotes still accept its sources
// only there, and everything it sends is dropped by cryptokey
// routing until the next render reaches them. Measured as a
// sub-minute flap on exactly the transit pairs after a placement
// shrink; the dialer's own egress decision had the same defect and
// the same fix.
func relaysFromApplied(doc tunnel.PeerListDoc, data map[string][]byte) map[string]bool {
	relays := map[string]bool{}
	for key := range data {
		if !strings.HasPrefix(key, tunnel.SiteAddressesPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, tunnel.SiteAddressesPrefix)
		addrs := tunnel.SplitList(string(data[key]))
		pub := strings.TrimSpace(string(data[tunnel.NodePublicKeyPrefix+name]))
		relays[name] = tunnel.DocRelaysNode(doc, pub, addrs)
	}
	return relays
}

// everyRemoteAcknowledged reports whether each remote machine has
// applied the current render, per the hash it stamped on its adoption
// Secret (tunnel.AppliedListAnnotation). Only its own acknowledgment
// counts: a handshake proves a tunnel, not a read, and a remote can
// handshake a current endpoint on keepalive alone while routing every
// packet by a list two placements old.
//
// No machines means nothing to protect. A machine without an
// acknowledgment holds the release indefinitely, and that is the
// right trade: the cost of holding is one unused peer entry, and the
// cost of releasing early was measured as both clouds dark, refusing
// the replacement's handshakes because nothing had ever told them its
// key.
func everyRemoteAcknowledged(data map[string][]byte, converged map[string]map[string]bool, name string) bool {
	for key := range data {
		if !strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) {
			continue
		}
		// Not "has this machine applied something recent" but "does
		// what it applied carry THIS node on a relay": the question
		// the departing node's traffic actually turns on.
		if !converged[strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix)][name] {
			return false
		}
	}
	return true
}

func departedLongEnough(data map[string][]byte, name string, now time.Time, retention time.Duration) (expired, recorded bool) {
	key := tunnel.NodeDepartedAtPrefix + name
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data[key])))
	if err != nil {
		data[key] = []byte(now.UTC().Format(time.RFC3339))
		return false, true
	}
	return !now.Before(since.Add(retention)), false
}

// publishedEndpoint reports whether a node is a usable peer as
// published: an address this operator allocated, and the public key
// the node's own dialer put there. Either alone is half a peer, which
// RemotePeers skips.
func publishedEndpoint(data map[string][]byte, name string) bool {
	return strings.TrimSpace(string(data[tunnel.NodePublicKeyPrefix+name])) != "" &&
		strings.TrimSpace(string(data[tunnel.NodeTunnelAddressPrefix+name])) != ""
}

// publishedNames lists, once each and in a stable order, the node
// names appearing under any of the given key prefixes.
func publishedNames(data map[string][]byte, prefixes ...string) []string {
	seen := map[string]bool{}
	var names []string
	for key := range data {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if name := strings.TrimPrefix(key, prefix); !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// retireTunnelAddress records an address as spent, so no later node is
// given it. See tunnel.RetiredTunnelAddressesKey for why it is never
// handed back.
func retireTunnelAddress(data map[string][]byte, addr string) bool {
	addr = strings.SplitN(strings.TrimSpace(addr), "/", 2)[0]
	if addr == "" {
		return false
	}
	retired := tunnel.SplitList(string(data[tunnel.RetiredTunnelAddressesKey]))
	for _, existing := range retired {
		if existing == addr {
			return false
		}
	}
	retired = append(retired, addr)
	sort.Slice(retired, func(i, j int) bool { return tunnel.LessIP(retired[i], retired[j]) })
	data[tunnel.RetiredTunnelAddressesKey] = []byte(strings.Join(retired, ","))
	return true
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

// Refreshing a remote's peer list is necessary and not sufficient, and
// the other half is not here.
//
// A remote learns its peer list from the API server and reaches the API
// server over the tunnel, so a pass that names the new endpoint and
// withdraws the old one at once removes the only path the new name
// could have travelled by. Measured: the site published only the new
// endpoint, the remote's own peer list was correctly re-rendered to
// name it, and the remote's WireGuard still held the old peer eighteen
// minutes later, NotReady throughout.
//
// The other half is two phase, and it is pruneDeparted's retention
// window together with dialerNodeAffinity's second term. A node the
// selector has let go of stays a full endpoint, dialer and published
// entries both, for as long as it takes a remote to poll and read the
// list naming the replacement. For that window a remote has two working
// paths rather than none, which is the whole trick.

// refreshAdoptionConfigs re-renders every remote's peer list, for the
// passes that were not about any one machine.
//
// A failure for one machine does not stop the others: they are separate
// remotes, and leaving the rest stale because one is unreadable is the
// blast radius this codebase keeps having to narrow.
func (r *meshReconciler) refreshAdoptionConfigs(ctx context.Context) error {
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind(machineGVK.Kind + "List"))
	// Every machine this operator is responsible for. Listing without
	// the selector would re-render peer lists for machines belonging to
	// something else.
	opts := []client.ListOption{}
	if r.machineSelector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: r.machineSelector})
	}
	if err := r.List(ctx, machines, opts...); err != nil {
		return fmt.Errorf("listing machines to refresh their peer lists: %w", err)
	}
	// The Role admitting these machines' adoption Secrets is a
	// derivation of the same list, so it is refreshed on the same
	// passes. Its failure does not stop the per-machine work below.
	roleErr := r.ensureDialerRole(ctx)
	if roleErr != nil {
		ctrl.LoggerFrom(ctx).Error(roleErr, "could not scope the dialer Role")
	}
	var failed []string
	for i := range machines.Items {
		// The address the CNI peers on is re-asserted here, not only on
		// the machine's own reconcile. The CNI's address monitor
		// restates its autodetected address whenever an interface
		// changes underneath it, which is exactly when tunnels move,
		// and the only signal that overwrite produces is a Node event:
		// the Machine holding the right value has not changed, so
		// waiting for a Machine event is waiting for nothing.
		if err := r.ensureCNINodeAddressForMachine(ctx, &machines.Items[i]); err != nil {
			failed = append(failed, machines.Items[i].GetName())
			ctrl.LoggerFrom(ctx).Error(err, "could not re-assert a remote's CNI address", "machine", machines.Items[i].GetName())
			continue
		}
		if err := r.ensureAdoptionConfig(ctx, &machines.Items[i]); err != nil {
			failed = append(failed, machines.Items[i].GetName())
			ctrl.LoggerFrom(ctx).Error(err, "could not refresh a remote's peer list", "machine", machines.Items[i].GetName())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not refresh the peer list for %v", failed)
	}
	return roleErr
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

// nodeNameForMachine is which node this machine turned out to be.
//
// Cluster API's Machine controller writes status.nodeRef, and when it
// is there it is the answer. But it only gets there if Cluster API
// holds a connection to the workload cluster, which it takes from a
// <cluster>-kubeconfig Secret — and this product's model is a
// pre-existing, self-managed cluster with no control plane provider
// to write one (see examples/aws.yaml, which an operator applies in
// full and which contains no such Secret). Depending on nodeRef alone
// therefore half-works in exactly the documented setup: the node
// joins and goes Ready, its pod blocks are never published, the CNI
// is never told which address to peer on, and nothing reports a
// problem.
//
// So failing that, the same match Cluster API itself makes: the node
// whose spec.providerID equals this machine's. In a cloud the
// provider sets one side and the cloud controller manager the other,
// and both are local reads that need no workload connection at all.
func (r *meshReconciler) nodeNameForMachine(ctx context.Context, machine *unstructured.Unstructured) string {
	if name, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "name"); name != "" {
		return name
	}
	providerID, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
	if providerID == "" {
		return ""
	}
	nodes := &corev1.NodeList{}
	if err := r.reader.List(ctx, nodes); err != nil {
		return ""
	}
	for i := range nodes.Items {
		if nodes.Items[i].Spec.ProviderID == providerID {
			return nodes.Items[i].Name
		}
	}
	return ""
}

func (r *meshReconciler) ensureCNINodeAddressForMachine(ctx context.Context, machine *unstructured.Unstructured) error {
	nodeName := r.nodeNameForMachine(ctx, machine)
	tunnelAddr := strings.SplitN(strings.TrimSpace(
		machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"]), "/", 2)[0]
	claimRef := ""
	for _, owner := range machine.GetOwnerReferences() {
		if owner.Kind == "ProvisionedNodeClaim" {
			claimRef = machine.GetNamespace() + "/" + owner.Name
		}
	}
	return r.ensureCNINodeAddress(ctx, nodeName, tunnelAddr, claimRef)
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
	owned, err := attachment.NativeTransportOwned(node)
	if err != nil {
		return err
	}
	if owned {
		if claim != "" && node.Annotations[claimpkg.ClaimAnnotation] != claim {
			patch := client.MergeFrom(node.DeepCopy())
			node.Annotations[claimpkg.ClaimAnnotation] = claim
			return r.Patch(ctx, node, patch)
		}
		return nil
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
	doc, err := tunnel.RemotePeerDocument(peerSecret.Data, selfTunnelAddr, r.apiVIP, r.apiServerPort)
	if err != nil {
		return err
	}
	if len(doc) == 0 {
		return nil
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

// ensureDialerRole keeps the dialer ServiceAccount's Role scoped to
// exactly the Secrets a dialer touches: the peer Secret (get, and
// merge-patch to self-publish a key) and each machine's adoption
// Secret (get everywhere -- a relay reads every remote's applied-list
// acknowledgment -- and patch to write its own). The chart cannot name
// the adoption Secrets, because machines come and go after install, so
// it renders the Role with only the peer Secret's name and this method
// carries the rest, derived from the same machine list that drives the
// adoption Secrets themselves. A helm upgrade may reset the Role to
// the chart's floor; the upgrade also restarts this controller, whose
// first pass restores it.
func (r *meshReconciler) ensureDialerRole(ctx context.Context) error {
	if r.dialerServiceAccount == "" {
		return nil
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind(machineGVK.Kind + "List"))
	opts := []client.ListOption{}
	if r.machineSelector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: r.machineSelector})
	}
	if err := r.List(ctx, machines, opts...); err != nil {
		return fmt.Errorf("listing machines to scope the dialer Role: %w", err)
	}
	adoption := make([]string, 0, len(machines.Items))
	for i := range machines.Items {
		adoption = append(adoption, tunnel.AdoptionSecretName(machines.Items[i].GetName()))
	}
	sort.Strings(adoption)
	desired := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		Verbs:         []string{"get", "patch"},
		ResourceNames: append([]string{r.secretName}, adoption...),
	}}

	role := &rbacv1.Role{}
	key := types.NamespacedName{Namespace: r.secretNamespace, Name: r.dialerServiceAccount}
	// The chart owns the Role's existence (its floor names the peer
	// Secret alone); this method only reshapes the one that is there.
	// A Role missing entirely is the install broken, not something to
	// paper over by creating one, and this identity holds no create on
	// roles anyway: the dialer's scope is maintained by a different
	// identity than the one it confines, and narrowing what this one
	// can do to the RBAC group is part of the point.
	if err := r.reader.Get(ctx, key, role); err != nil {
		return fmt.Errorf("reading the dialer Role: %w", err)
	}
	if len(role.Rules) == 1 && equalStrings(role.Rules[0].APIGroups, desired[0].APIGroups) &&
		equalStrings(role.Rules[0].Resources, desired[0].Resources) &&
		equalStrings(role.Rules[0].Verbs, desired[0].Verbs) &&
		equalStrings(role.Rules[0].ResourceNames, desired[0].ResourceNames) &&
		len(role.Rules[0].NonResourceURLs) == 0 {
		return nil
	}
	role.Rules = desired
	return r.Update(ctx, role)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	// A control plane carries a NoSchedule taint, so allowing it by
	// affinity is not enough: without a toleration a selected control
	// plane simply never gets a pod, and the mesh silently omits it.
	//
	// Unconditional, because a toleration decides nothing about where a
	// pod goes. It only removes an objection; the affinity above is what
	// chooses, and it admits a control plane exactly when the selector
	// names one or retention still holds one. Deriving this from the
	// selector alone tied the two together and got retention wrong: a
	// control plane that had just left the selector was still published,
	// still expected to carry its tunnel through the window, and could
	// no longer be scheduled at all. Measured with cp retained and
	// desiredNumberScheduled 1, so the node kept a tunnel that no dialer
	// maintained and, when its retention expired, no dialer was there to
	// take the interface down either.
	tolerations := dialerTolerations()

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
							RequiredDuringSchedulingIgnoredDuringExecution: dialerNodeAffinity(),
						},
					},
					ImagePullSecrets: imagePullSecrets(r.dialerImagePullSecret),
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           r.dialerImage,
							ImagePullPolicy: effectiveDialerPullPolicy(r.dialerImagePullPolicy),
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

// retainedEndpoints lists the nodes the peer Secret still publishes a
// tunnel address for. During a migration that is the departing node as
// well as the arriving one, which is the whole point: the departing
// node's dialer has to outlive the selector change that names its
// replacement, because the replacement's name travels over it.
//
// No Secret yet means no mesh yet, which is not an error: the first
// reconcile creates both the Secret and this DaemonSet.
func (r *meshReconciler) retainedEndpoints(ctx context.Context) ([]string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting secret %s to find retained endpoints: %w", key, err)
	}
	var names []string
	for k, v := range secret.Data {
		if !strings.HasPrefix(k, tunnel.NodeTunnelAddressPrefix) {
			continue
		}
		if strings.TrimSpace(string(v)) == "" {
			continue
		}
		names = append(names, strings.TrimPrefix(k, tunnel.NodeTunnelAddressPrefix))
	}
	sort.Strings(names)
	return names, nil
}

// dialerNodeAffinity is where the on-prem dialer is allowed to run:
// every Linux node at the site. Not only the selected endpoints,
// because the dialer has a job on every node. On an endpoint it
// terminates the tunnel; on a node with no tunnel it installs the
// transit that reaches the remotes through the relay, derived from the
// same data the render uses.
//
// Placement no longer appears here at all, and that is load bearing
// twice over. Which nodes hold tunnels is decided by the Secret the
// controller writes, and the dialer reads it every pass, so moving a
// tunnel changes data rather than pod scheduling: no dialer restarts,
// no speaker or session teardown, no window in which the node that
// must hand over is the node whose pod was just killed. And a node the
// selector let go of keeps its dialer for the retention window and
// after it, so the teardown of its interface is done by the same
// process that built it, rather than by a shutdown hook racing the
// scheduler.
//
// A node this operator provisioned is on the far side of a tunnel and
// never one of the site's ends of it, and Windows terminates nothing.
func dialerNodeAffinity() *corev1.NodeSelector {
	return &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
			{Key: cloudWorkerRoleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{cloudWorkerRoleValue}},
		},
	}}}
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
// stopBeingRelayed takes a node's addresses and blocks off the site
// entries, which is where they live while some other endpoint relays to
// it.
//
// The counterpart of stopOwningAsEndpoint, and required for the same
// reason: a node that leaves the selector hands its prefixes to the site
// entries, and a node that returns takes them back. Doing only the first
// half leaves a returning endpoint owning its block twice, once on its
// own peer entry and once on whichever endpoint relays the site, and the
// accept list resolves that by keeping whichever was written last.
// takeBackFromRelay hands a returning endpoint its own prefixes back,
// and only once it is a peer as published.
//
// Selection makes this operator allocate a tunnel address; it is the
// node's own dialer that publishes the key, and RemotePeers renders a
// node only when both are there. Dropping the relayed entry at
// selection left the node's address and pod block owned by nobody until
// that key landed, which is however long the dialer takes to schedule
// and come up. A remote reading the list in that window prunes the
// route and cannot read the correction, because the site is reachable
// only over what it just pruned.
func takeBackFromRelay(data map[string][]byte, name string) bool {
	if !publishedEndpoint(data, name) {
		return false
	}
	return stopBeingRelayed(data, name)
}

func stopBeingRelayed(data map[string][]byte, name string) bool {
	changed := false
	for _, key := range []string{
		tunnel.SiteAddressesPrefix + name,
		tunnel.SitePodCIDRsPrefix + name,
	} {
		if _, ok := data[key]; ok {
			delete(data, key)
			changed = true
		}
	}
	return changed
}

// stopOwningAsEndpoint takes a departing endpoint's node addresses and
// pod blocks off its own peer entry, leaving its key and tunnel address
// so the tunnel it still holds goes on working.
//
// Those prefixes are published as site entries in the same pass, and a
// prefix must have exactly one owner: WireGuard's accept list is a trie,
// so a range named on two peers belongs to whichever was written last
// and traffic for it follows whichever that happened to be.
func stopOwningAsEndpoint(data map[string][]byte, name string) bool {
	changed := false
	for _, key := range []string{
		tunnel.NodeAddressesPrefix + name,
		tunnel.NodePodCIDRsPrefix + name,
	} {
		if _, ok := data[key]; ok {
			delete(data, key)
			changed = true
		}
	}
	return changed
}

// imagePullSecrets is the pull secret list for a dialer pod, which is
// empty when no secret is configured rather than holding a reference to
// nothing.
//
// A LocalObjectReference with an empty name is accepted by the API and
// then breaks every strategic merge patch against the object, because
// name is the merge key for this list and the element has none:
// "does not contain declared merge key: name". kubectl rollout restart,
// kubectl set image and kubectl apply all fail against a DaemonSet
// carrying one, so an operator cannot restart their own dialers, on a
// cluster where nothing is visibly wrong.
// dialerTolerations lets a dialer run on a control plane. See the
// caller for why this is unconditional.
func dialerTolerations() []corev1.Toleration {
	var tolerations []corev1.Toleration
	for _, key := range []string{controlPlaneLabel, "node-role.kubernetes.io/master"} {
		tolerations = append(tolerations, corev1.Toleration{
			Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	return tolerations
}

func imagePullSecrets(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

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
					ImagePullSecrets: imagePullSecrets(r.dialerImagePullSecret),
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           cloudImage,
							ImagePullPolicy: effectiveDialerPullPolicy(r.dialerImagePullPolicy),
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
								// Writable for one file: the interface claim
								// that tells the node's cloud-init systemd
								// unit to stand off while this pod holds the
								// live peer list. The peers file and identity
								// here are still only ever read.
								{Name: "wg-dialer-config", MountPath: "/etc/wg-dialer"},
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
		endpointRetention   time.Duration
		dialerPrivateKeyDir string

		joinEnabled               bool
		joinTemplatePath          string
		joinAPIAddress            string
		joinAPIVIP                string
		joinAPIProxyPort          int
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
		dialerImagePullPolicy     string
		dialerCloudDaemonSetName  string
		windowsPublisherImage     string
		windowsPublisherAPIServer string
		windowsTunnelSHA256       string
		windowsWorkerVersion      string
		windowsWorkerSHA256       string
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
	flag.DurationVar(&endpointRetention, "tunnel-endpoint-retention", 3*time.Minute, "how long a node that has left --tunnel-endpoints goes on being one: dialer still scheduled, entries still published, tunnel still carrying traffic. A remote reads its peer list over the tunnel it is about to lose, so this is the time it is given to read the one naming the replacement, and it is measured in the dialer's 30s poll interval rather than in anything about the departing node's health")
	flag.StringVar(&dialerPrivateKeyDir, "dialer-private-key-dir", "/var/lib/cloud-provisioning", "host directory where each node's dialer keeps its own WireGuard private key (generated on first start; never leaves the node)")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "tunnel-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "cloud-provisioning-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "", "REQUIRED image for the dialer DaemonSets, pinned by digest (tag@sha256:...). Deliberately has no default: a stale built-in default once pointed at a pre-hardening build")
	flag.StringVar(&dialerImagePullPolicy, "dialer-image-pull-policy", "IfNotPresent", "pull policy for generated Linux dialer DaemonSets: Always, IfNotPresent, or Never for preloaded images")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "", "optional imagePullSecret for the dialer DaemonSets; leave empty when the images are publicly pullable. Naming a Secret that does not exist makes every pod on every endpoint node log a pull-secret warning, so this defaults to none")
	flag.IntVar(&transitBGPPort, "transit-bgp-port", 0, "port for the speaker each tunnel endpoint runs, telling the rest of the site which remote nodes are reachable through it. 0 leaves it off, which is right when every node that needs a remote terminates a tunnel of its own. Not 179: a node whose CNI speaks BGP is already there")
	flag.IntVar(&transitBGPASN, "transit-bgp-asn", 64512, "autonomous system for that speaker; match the cluster's own")
	flag.StringVar(&ownerDeployment, "owner-deployment", "", "this controller's own Deployment name; everything it creates at runtime (both DaemonSets, the peer and adoption Secrets) is owned by it, so an uninstall garbage-collects them instead of orphaning a tunnel interface on every endpoint node")
	flag.StringVar(&windowsTunnelSHA256, "windows-tunnel-sha256", "", "enable prepared-image k0s Windows bootstrap with this tunnel executable SHA256")
	flag.StringVar(&windowsWorkerVersion, "windows-worker-version", "", "exact baked k0s Windows worker build; must retain the cluster's base release")
	flag.StringVar(&windowsWorkerSHA256, "windows-worker-sha256", "", "baked k0s worker SHA256; required with windows-worker-version")
	flag.StringVar(&windowsPublisherImage, "windows-publisher-image", "", "Windows HostProcess peer-publisher image; empty leaves Windows publication disabled")
	flag.StringVar(&windowsPublisherAPIServer, "windows-publisher-api-server", "", "API URL reachable from Windows HostProcess, such as the distribution node-local balancer; empty uses the Kubernetes Service environment")
	flag.StringVar(&dialerCloudImage, "dialer-cloud-image", "", "optional PUBLIC base image for the REMOTE node's DaemonSet, which then executes --dialer-cloud-host-binary instead of carrying its own. Use when the project image is not pullable on a remote node (no preload, no registry credential): without it the adoption DaemonSet ImagePullBackOffs and the node stays on its frozen bootstrap peer list forever")
	flag.StringVar(&dialerCloudHostBinary, "dialer-cloud-host-binary", "/host-bin/wg-dialer", "path (inside the pod) of the host binary --dialer-cloud-image executes; the bootstrap already installed and sha-verified it")
	flag.StringVar(&dialerCloudDaemonSetName, "dialer-cloud-daemonset-name", "tunnel-dialer-remote", "name of the remote-side dialer DaemonSet this operator provisions directly")

	flag.BoolVar(&joinEnabled, "join-enabled", true, "enable bootstrap-secret provisioning (join.Reconciler) and claim expansion: the whole point of this operator; disable only for an endpoint-mirror-only deployment")
	flag.StringVar(&joinProviderName, "join-provider", "k0s", "which cluster technology's join specialization mints join credentials (k0s, kubeadm, k3s, rke2, microk8s); pair with the matching --join-template-path")
	flag.StringVar(&joinTemplatePath, "join-template-path", "/join-patterns/k0s-worker.cloud-config.tmpl", "path to the join-pattern template to render")
	flag.StringVar(&joinAPIAddress, "join-api-address", "", "REQUIRED cluster API server address used to mint join tokens (bracket IPv6 literals, e.g. https://[fd8f:cf26:522a::1]:6443)")
	flag.StringVar(&joinAPIVIP, "join-api-vip", "", "REQUIRED cluster API VIP the new node must reach through the tunnel before joining")
	flag.IntVar(&joinAPIProxyPort, "join-api-proxy-port", 7445, "port of the loopback API balancer the remote's dialer serves; the join gates on it and kubelet keeps dialing it")
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
	gatewayConfig := flag.String("aws-gateway-config", "", "optional path to AWS Windows Calico gateway scope JSON")
	flag.Parse()
	if _, err := parseDialerPullPolicy(dialerImagePullPolicy); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

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
		LeaderElection:          true,
		LeaderElectionNamespace: secretNamespace,
		LeaderElectionID:        meshLeaderElectionID(secretName),
		// Do not release early: an in-flight provider operation may still be ending
		// when cancellation starts. A successor waits for normal lease expiry.
		LeaderElectionReleaseOnCancel: false,

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
			// The cluster's own stated endpoint first: on an HA
			// cluster that is the VIP that outlives any one control
			// plane, and deriving from the member list instead pins
			// every remote to one specific member, whose death then
			// strands them with quorum intact. The member list is the
			// fallback for a cluster that never stated an endpoint.
			endpoint, err := discover.APIEndpoint(ctx, discoveryClient)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading the cluster's stated API endpoint: %v\n", err)
			}
			target := endpoint
			if target == "" {
				servers, err := discover.APIServers(ctx, discoveryClient)
				if err != nil {
					fmt.Fprintf(os.Stderr, "cannot determine the API server address (set --join-api-address): %v\n", err)
					os.Exit(1)
				}
				target = "https://" + servers[0]
			}
			if joinAPIAddress == "" {
				joinAPIAddress = target
			}
			if joinAPIVIP == "" {
				u, err := url.Parse(target)
				if err != nil || u.Hostname() == "" {
					fmt.Fprintf(os.Stderr, "cannot read a host from the API address %q: %v\n", target, err)
					os.Exit(1)
				}
				joinAPIVIP = u.Hostname()
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
			machineSelector:        selector,
			secretNamespace:        secretNamespace,
			secretName:             secretName,
			secretKey:              secretKey,
			port:                   port,
			gatewayNamespace:       gatewayNamespace,
			gatewayName:            gatewayName,
			tunnelEndpointSelector: endpointSelector,
			tunnelEndpointsRaw:     rawTunnelEndpoints,
			endpointRetention:      endpointRetention,
			tunnelSubnet:           tunnelSubnet,
			localAddressBase:       localAddressBase,
			dialerDaemonSetName:    dialerDaemonSetName,
			dialerServiceAccount:   dialerServiceAccount,
			dialerImage:            dialerImage,
			dialerImagePullSecret:  dialerImagePullSecret,
			dialerImagePullPolicy:  corev1.PullPolicy(dialerImagePullPolicy),
			dialerPrivateKeyDir:    dialerPrivateKeyDir,
			ifaceName:              ifaceName,
			apiVIP:                 joinAPIVIP,
			apiServerPort:          apiServerPortOf(joinAPIAddress),

			ownerRef:                  runtimeOwner,
			network:                   network,
			transitBGPPort:            transitBGPPort,
			transitBGPASN:             transitBGPASN,
			dialerCloudDaemonSetName:  dialerCloudDaemonSetName,
			dialerCloudListenPort:     dialerListenPort,
			dialerCloudImage:          dialerCloudImage,
			windowsPublisherImage:     windowsPublisherImage,
			windowsPublisherAPIServer: windowsPublisherAPIServer,
			dialerCloudHostBinary:     dialerCloudHostBinary,
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
		dockerProvider := joindocker.Provider{}
		// Adopts a container someone else owns, which is how a lab
		// topology presents a machine. Shipped like the docker provider
		// rather than hidden behind a build tag, so a harness exercises
		// the same binary that is released.
		containernetProvider := joincontainernet.Provider{}

		// Each cluster technology is one implementation of
		// join.ClusterJoinProvider behind the same seam. Selection is by
		// name, and each implementation's own knobs live in its own
		// provider-config Secret (the aws-provider-config pattern),
		// not in this binary's generic flags.
		var joinProvider join.ClusterJoinProvider
		switch joinProviderName {
		case "k0s":
			joinProvider = &joink0s.Provider{
				Reader: mgr.GetAPIReader(),
				Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL,
				ConfigNamespace: secretNamespace, ConfigName: "k0s-provider-config",
			}
		case "kubeadm":
			joinProvider = &joinkubeadm.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL}
		case "k3s":
			joinProvider = &joink3s.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL}
		case "microk8s":
			joinProvider = &joinmicrok8s.Provider{Reader: mgr.GetAPIReader(), Namespace: secretNamespace}
		case "rke2":
			joinProvider = &joinrke2.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL}
		default:
			fmt.Fprintf(os.Stderr, "unknown --join-provider %q (registered specializations: k0s, kubeadm, k3s, rke2, microk8s)\n", joinProviderName)
			os.Exit(1)
		}

		bootstrapRenderers := map[string]join.BootstrapRenderer{"linux": join.PatternBootstrap{Path: joinTemplatePath, Format: bootstrap.CloudConfig}}
		if (windowsWorkerVersion == "") != (windowsWorkerSHA256 == "") || (windowsWorkerVersion != "" && windowsTunnelSHA256 == "") {
			fmt.Fprintln(os.Stderr, "Windows worker override requires version, SHA256 and enabled Windows bootstrap")
			os.Exit(1)
		}
		if windowsTunnelSHA256 != "" {
			if joinProviderName != "k0s" || windowsPublisherImage == "" {
				fmt.Fprintln(os.Stderr, "Windows bootstrap requires k0s and a Windows publisher image")
				os.Exit(1)
			}
			bootstrapRenderers["windows"] = joink0s.WindowsBootstrap{ServiceSHA256: windowsTunnelSHA256, WorkerVersion: windowsWorkerVersion, WorkerSHA256: windowsWorkerSHA256}
		}
		joinReconciler := &join.Reconciler{
			Client:         mgr.GetClient(),
			Reader:         mgr.GetAPIReader(),
			Join:           joinProvider,
			InfraProviders: []join.InfraProvider{awsProvider, dockerProvider, containernetProvider},

			BootstrapRenderers: bootstrapRenderers,
			APIVIP:             joinAPIVIP,
			APIProxyPort:       joinAPIProxyPort,
			KubeletExtraArgs:   joinKubeletExtraArgs,
			SSHAuthorizedKeys:  sshKeys,

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
				Provisioners:              []join.MachineProvisioner{awsProvider, dockerProvider, containernetProvider},
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

	if *gatewayConfig != "" {
		if err := registerGateway(mgr, *gatewayConfig, secretNamespace, secretName, joinAPIVIP, apiServerPortOf(joinAPIAddress)); err != nil {
			fmt.Fprintf(os.Stderr, "unable to register AWS gateway controller: %v\n", err)
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
