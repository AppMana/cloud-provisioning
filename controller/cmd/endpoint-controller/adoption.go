package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	claimpkg "github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
			// Existing published intent still needs expiry recovery when normal
			// CNI work fails. Never start preparation through this error path.
			if recoveryErr := r.recoverPublishedReceiveTransition(ctx, &machines.Items[i], time.Now()); recoveryErr != nil {
				ctrl.LoggerFrom(ctx).Error(recoveryErr, "could not recover a remote's published handover", "machine", machines.Items[i].GetName())
			}
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
	var tunnelAddrs []string
	for _, key := range []string{join.WireGuardAddrAnnotation, join.WireGuardAddr6Annotation} {
		if addr := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()[key]), "/", 2)[0]; addr != "" {
			tunnelAddrs = append(tunnelAddrs, addr)
		}
	}
	claimRef := ""
	for _, owner := range machine.GetOwnerReferences() {
		if owner.Kind == "ProvisionedNodeClaim" {
			claimRef = machine.GetNamespace() + "/" + owner.Name
		}
	}
	return r.ensureCNINodeAddress(ctx, nodeName, tunnelAddrs, claimRef)
}

// siteExcluder keeps the site pod network off nodes of every native network
// the infrastructure providers run.
func siteExcluder(c client.Client, reader client.Reader, providers []join.InfraProvider, extra string) (*site.Excluder, error) {
	e := &site.Excluder{Client: c, Reader: reader, Now: time.Now}
	for name := range nativeNetworksOf(providers) {
		e.Native = append(e.Native, name)
	}
	for _, ref := range strings.Split(extra, ",") {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		namespace, name, ok := strings.Cut(ref, "/")
		if !ok || namespace == "" || name == "" {
			return nil, fmt.Errorf("%q is not namespace/name", ref)
		}
		e.Extra = append(e.Extra, client.ObjectKey{Namespace: namespace, Name: name})
	}
	return e, nil
}

// nativeNetworksOf indexes the pod networks the infrastructure providers run
// natively.
func nativeNetworksOf(providers []join.InfraProvider) map[string]platform.Network {
	var networks []platform.Network
	for _, p := range providers {
		if n, ok := p.(join.NetworkProvider); ok {
			networks = append(networks, n.NativeNetworks()...)
		}
	}
	return platform.Index(networks...)
}

// nativeObserver is the allocation observer for a provider-native network.
// A node registered on a network nobody observes cannot be routed: its pod
// addresses are unknown.
func (r *meshReconciler) nativeObserver(network string) (platform.Observer, error) {
	observer := r.nativeObservers[network]
	if observer == nil {
		return nil, fmt.Errorf("pod network %q has no allocation observer configured", network)
	}
	return observer, nil
}

// ensureCNINodeAddress states the address the CNI peers on for each
// family named in addrs. A family with no address here is left to the
// CNI.
func (r *meshReconciler) ensureCNINodeAddress(ctx context.Context, nodeName string, addrs []string, claim string) error {
	var stated []string
	for _, addr := range addrs {
		if addr = strings.TrimSpace(addr); addr != "" {
			stated = append(stated, addr)
		}
	}
	if nodeName == "" || len(stated) == 0 {
		return nil
	}
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting node %s: %w", nodeName, err)
	}
	// A provider-native network owns this node; never pin a site CNI
	// address on it.
	if name := node.Labels[platform.CNILabel]; name != "" {
		network := r.nativeNetworks[name]
		if network == nil {
			return fmt.Errorf("node %s registered on pod network %q, which no provider runs", node.Name, name)
		}
		return network.VerifyNode(node)
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
	want := map[string]string{}
	for _, addr := range stated {
		key, value := calicoIPv4Annotation, addr+"/32"
		if strings.Contains(addr, ":") {
			key, value = calicoIPv6Annotation, addr+"/128"
		}
		// Calico rewrites this with the prefix length the address
		// actually carries on the interface, so only the address is
		// compared. Fixing the mask back every pass would be a fight
		// with the thing being configured.
		if existing := node.Annotations[key]; existing != "" && strings.SplitN(existing, "/", 2)[0] == addr {
			continue
		}
		want[key] = value
	}
	// The claim is recorded here too, so its teardown can find the
	// Node it produced. A Node is cluster-scoped and a claim is not, so
	// an ownerReference cannot express this.
	if len(want) == 0 && (claim == "" || node.Annotations[claimpkg.ClaimAnnotation] == claim) {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	for key, value := range want {
		node.Annotations[key] = value
	}
	if claim != "" {
		node.Annotations[claimpkg.ClaimAnnotation] = claim
	}
	if err := r.Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("annotating node %s with its tunnel address: %w", nodeName, err)
	}
	return nil
}

// syncIPv6Endpoint keeps an endpoint's IPv6 entries matching the mesh:
// its IPv6 tunnel address, paired with the IPv4 one already allocated,
// and the IPv6 address the rest of the site forwards to when it relays.
// A single-stack mesh carries neither. Reports whether anything changed.
func (r *meshReconciler) syncIPv6Endpoint(data map[string][]byte, node *corev1.Node) bool {
	changed := false
	set := func(key, value string) {
		if value == "" {
			if _, ok := data[key]; ok {
				delete(data, key)
				changed = true
			}
			return
		}
		if string(data[key]) != value {
			data[key] = []byte(value)
			changed = true
		}
	}
	addr6, transit6 := "", ""
	if r.tunnelIPv6Prefix != "" {
		var err error
		addr6, err = tunnel.TunnelAddress6(r.tunnelIPv6Prefix, string(data[tunnel.NodeTunnelAddressPrefix+node.Name]))
		if err != nil {
			addr6 = ""
		}
		transit6 = r.transitAddress6(node)
	}
	set(tunnel.NodeTunnelAddress6Prefix+node.Name, addr6)
	set(tunnel.NodeTransitAddress6Prefix+node.Name, transit6)
	return changed
}

// transitAddress6 is the IPv6 address the rest of the site reaches this
// node at. Where the network is Calico, that is the address Calico
// peers on: its own IPv6 routes already resolve through it, while a
// Kubernetes address held on a dummy device answers no neighbour
// solicitation on the LAN. Otherwise the node's IPv6 Kubernetes
// address.
func (r *meshReconciler) transitAddress6(node *corev1.Node) string {
	if r.network.Name == cni.Calico {
		addr := strings.SplitN(strings.TrimSpace(node.Annotations[calicoIPv6Annotation]), "/", 2)[0]
		if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
			return ip.String()
		}
	}
	for _, a := range node.Status.Addresses {
		if ip := net.ParseIP(a.Address); a.Type == corev1.NodeInternalIP && ip != nil && ip.To4() == nil {
			return ip.String()
		}
	}
	return ""
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
func (r *meshReconciler) publishRemotePodCIDRs(ctx context.Context, machine *unstructured.Unstructured, nodeName string) (bool, error) {
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false, err
	}
	nativeName := node.Labels[platform.CNILabel]
	nativeNode := nativeName != ""
	var prefixes []netip.Prefix
	var nativeNetwork []byte
	var nativeScope *tunnel.NativeNetwork
	var err error
	if nativeNode {
		observer, observerErr := r.nativeObserver(nativeName)
		if observerErr != nil {
			return false, observerErr
		}
		allocation, observeErr := observer.Allocations(ctx, machine, nodeName)
		if observeErr != nil {
			return false, fmt.Errorf("observing %s pod allocation: %w", nativeName, observeErr)
		}
		prefixes = allocation.PodPrefixes
		network := tunnel.NativeNetwork{ID: allocation.NetworkID}
		for _, prefix := range allocation.NativePrefixes {
			network.Prefixes = append(network.Prefixes, prefix.String())
		}
		sort.Strings(network.Prefixes)
		nativeScope = &network
	} else {
		prefixes, err = r.network.PrefixesFor(ctx, r.reader, nodeName)
	}
	if err != nil {
		// No block yet, which is not a failure: the caller comes back.
		return false, nil
	}
	if len(prefixes) == 0 && !nativeNode {
		// An encapsulated network needs none, so there is nothing
		// pending and nothing to publish.
		return r.network.Encapsulation == cni.Encapsulated, nil
	}
	// A block the network would masquerade toward is still published:
	// it is the operator's to resolve, and refusing here would leave
	// the node with no reachability at all rather than reachability
	// that fails in one direction. Saying so is what was missing.
	if err := r.network.CheckMasquerade(ctx, r.reader, prefixes); err != nil {
		if nativeNode {
			return false, err
		}
		ctrl.LoggerFrom(ctx).Error(err, "the remote's pod block will not survive the network's outgoing NAT", "node", nodeName)
	}
	secret := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, secret); err != nil {
		return false, fmt.Errorf("getting peer secret: %w", err)
	}
	if nativeScope != nil {
		nativeScope.PublicKey = string(secret.Data[tunnel.PeerPublicKeyPrefix+machine.GetName()])
		if nativeScope.PublicKey == "" {
			return false, fmt.Errorf("native network has no peer identity")
		}
		nativeNetwork, err = json.Marshal(nativeScope)
		if err != nil {
			return false, err
		}
	}
	key := tunnel.PeerAllowedIPsPrefix + machine.GetName()
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
	routeHostsKey := tunnel.PeerRouteHostsPrefix + machine.GetName()
	wantRouteHosts := string(secret.Data[routeHostsKey])
	if nativeNode {
		want, err = nativeAllowedIPs(machine, secret, prefixes)
		if err != nil {
			return false, err
		}
		addresses, _, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
		if err != nil {
			return false, err
		}
		_, machineAddresses := machineHosts(addresses)
		routeHosts := tunnel.SplitList(wantRouteHosts)
		for _, addr := range machineAddresses {
			routeHosts = appendMissing(routeHosts, addr)
		}
		wantRouteHosts = strings.Join(routeHosts, ",")
	}
	nativeKey := tunnel.PeerNativeNetworkPrefix + machine.GetName()
	if want == string(secret.Data[key]) &&
		string(nativeNetwork) == string(secret.Data[nativeKey]) &&
		(!nativeNode || wantRouteHosts == string(secret.Data[routeHostsKey])) {
		return true, nil
	}
	if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
		return false, err
	}
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = []byte(want)
	if nativeNode {
		secret.Data[nativeKey] = nativeNetwork
		if wantRouteHosts != "" {
			secret.Data[routeHostsKey] = []byte(wantRouteHosts)
		}
	} else {
		delete(secret.Data, nativeKey)
	}
	if err := r.Patch(ctx, secret, patch); err != nil {
		return false, fmt.Errorf("publishing the remote node's pod blocks: %w", err)
	}
	return true, nil
}

func (r *meshReconciler) hasReceiveJournal(ctx context.Context, machine *unstructured.Unstructured) (bool, error) {
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receiveJournal.ConfigMaps == nil {
		return false, nil
	}
	uid := string(machine.GetUID())
	if uid == "" {
		return false, nil
	}
	_, err := r.receiveJournal.ConfigMaps.Get(ctx, tunnel.ReceiveTransitionName(uid), metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (r *meshReconciler) ensureAdoptionConfig(ctx context.Context, machine *unstructured.Unstructured) error {
	peerSecret := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, peerSecret); err != nil {
		return fmt.Errorf("getting peer secret: %w", err)
	}
	selfTunnelAddr := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()[join.WireGuardAddrAnnotation]), "/", 2)[0]
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
	if r.receivePreparation != nil {
		if !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != "" {
			return nil
		}
		_, preparing := existing.Data[tunnel.ReceivePrepareKey]
		if !preparing && remoteNeighborSourcesWithdrawn(existing.Data[tunnel.CloudPeersKey], doc) {
			hasJournal, err := r.hasReceiveJournal(ctx, machine)
			if err != nil {
				return err
			}
			if !hasJournal {
				patch := client.MergeFromWithOptions(existing.DeepCopy(), client.MergeFromWithOptimisticLock{})
				if existing.Data == nil {
					existing.Data = map[string][]byte{}
				}
				existing.Data[tunnel.CloudPeersKey] = doc
				return r.Patch(ctx, existing, patch)
			}
		}
		return r.reconcileReceivePreparation(ctx, machine)
	}
	if string(existing.Data[tunnel.CloudPeersKey]) == string(doc) {
		return nil
	}
	// Receiver preparation owns the forwarding base until the transition
	// explicitly releases it. Even an invalid proposal must not allow ordinary
	// reconciliation to move prefix ownership underneath prepared transports.
	if _, preparing := existing.Data[tunnel.ReceivePrepareKey]; preparing {
		return nil
	}
	// Preparation can start after this read. A resource-version precondition
	// prevents this writer from overwriting its newly reserved forwarding base.
	patch := client.MergeFromWithOptions(existing.DeepCopy(), client.MergeFromWithOptimisticLock{})
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
// adoption Secrets themselves. Online chart upgrades preserve the current
// named grants; this reconciliation removes grants for departed machines,
// except journals still reserved by the controller-owned handover inventory.
func (r *meshReconciler) ensureDialerRole(ctx context.Context) error {
	if r.dialerServiceAccount == "" {
		return nil
	}
	// Capture the Role revision before deriving grants. A concurrent pass that
	// grants a newly archived predecessor must make this older update conflict.
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
	journals := make([]string, 0, len(machines.Items))
	for i := range machines.Items {
		adoption = append(adoption, tunnel.AdoptionSecretName(machines.Items[i].GetName()))
		if r.receivePreparation != nil && r.receivePreparation.Journal != nil && machines.Items[i].GetNamespace() == r.secretNamespace {
			if machines.Items[i].GetUID() == "" {
				return fmt.Errorf("journal read access requires a machine UID")
			}
			journals = append(journals, tunnel.ReceiveTransitionName(string(machines.Items[i].GetUID())))
		}
	}
	if r.receivePreparation != nil && r.receivePreparation.Journal != nil {
		mesh := &corev1.Secret{}
		if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
			return err
		}
		if mesh.UID == "" || mesh.DeletionTimestamp != nil {
			return fmt.Errorf("journal read access requires a live mesh identity")
		}
		inventory, _, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		journals = append(journals, tunnel.ReceiveInventoryName(string(mesh.UID)))
		// Released histories still fence stale boot recovery. Keep their exact
		// journal and ancestry reads without treating them as active owners.
		retained := append([]string{}, inventory.MachineUIDs...)
		for uid := range inventory.Released {
			retained = append(retained, uid)
		}
		sort.Strings(retained)
		for _, uid := range retained {
			name := tunnel.ReceiveTransitionName(uid)
			publication, released := inventory.Released[uid]
			if !released {
				publication = inventory.Publications[uid]
			}
			if publication.JournalUID != "" {
				store := r.receiveJournal
				store.Name, store.ExpectedUID = name, publication.JournalUID
				snapshot, err := store.Load(ctx)
				if err != nil {
					return err
				}
				archives, err := store.ArchiveNames(ctx, snapshot)
				if err != nil {
					return err
				}
				journals = append(journals, archives...)
				if !released {
					pending, err := r.receivePreparationArchive(ctx, store, snapshot)
					if err != nil {
						return err
					}
					if pending != "" {
						journals = append(journals, pending)
					}
				}
			}
			found := false
			for _, current := range journals {
				found = found || current == name
			}
			if !found {
				journals = append(journals, name)
			}
		}
	}
	sort.Strings(adoption)
	sort.Strings(journals)
	desired := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		Verbs:         []string{"get", "patch"},
		ResourceNames: append([]string{r.secretName}, adoption...),
	}}
	if len(journals) != 0 {
		// Preparation receipts are writable by agents. Phase authority lives in
		// separate controller-owned journals that agents may only read by name.
		desired = append(desired, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}, ResourceNames: journals})
	}
	if r.receivePreparation != nil && r.receivePreparation.Journal != nil && r.receivePreparation.Journal.Receipts != nil && r.receivePreparation.Journal.Receipts.Endpoint != "" {
		desired = append(desired, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}})
	}

	if reflect.DeepEqual(role.Rules, desired) {
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
