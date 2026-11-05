package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
	if err := r.validateSiteDialerAdmission(nodes.Items); err != nil {
		return 0, err
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
	if progressed, err := r.receiveCoordinator().ReconcileOrphanWithdrawals(ctx, secret); err != nil {
		return 0, err
	} else if progressed {
		return time.Second, nil
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
	// Persist the current election before newly selected endpoints can enter it.
	changed := tunnel.PinRelay(secret.Data)
	changed = pruneDeparted(secret.Data, r.membership(nodes.Items), now, r.endpointRetention, r.convergedMachines(ctx, secret.Data)) || remoteChanged || changed

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
			// A departing endpoint retains its key, tunnel address, and dialer during
			// retention. Publish its node addresses and pod blocks as site entries so
			// surviving endpoints can relay them before the old tunnel is removed.
			// Keep each prefix under one owner in the published peer document.
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
		// A returning endpoint takes back its relayed prefixes only after both its
		// tunnel address and public key are published. Removing the relay entry
		// before RemotePeers can render the endpoint would leave a routing gap.
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
		if r.syncIPv6Endpoint(secret.Data, node) {
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
		if err := r.ensureCNINodeAddress(ctx, node.Name, []string{firstAddress(addresses)}, ""); err != nil {
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
	if tunnel.PinRelay(secret.Data) {
		changed = true
	}
	// Every control plane's address, for the remote's node-local load
	// balancer: a k0s worker fans its API traffic across all control
	// planes, so publishing only the one the join token points at would
	// leave the remote dependent on that single node staying up.
	var apiServers []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !platform.IsControlPlane(node) {
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

// TunnelEndpointSelector is a structured selector for choosing which site Linux
// nodes terminate WireGuard tunnels. It supports standard metav1.LabelSelector
// (matchLabels + matchExpressions), explicit nodeNames / excludeNodeNames,
// role filters ("worker", "control-plane", "all"), and node field requirements
// (metadata.name, spec.unschedulable), while remaining backward-compatible with
// plain label-selector strings and "all".
type TunnelEndpointSelector struct {
	MatchLabels         map[string]string                `json:"matchLabels,omitempty"`
	MatchExpressions    []metav1.LabelSelectorRequirement `json:"matchExpressions,omitempty"`
	NodeNames           []string                         `json:"nodeNames,omitempty"`
	ExcludeNodeNames    []string                         `json:"excludeNodeNames,omitempty"`
	Roles               []string                         `json:"roles,omitempty"`
	IncludeControlPlane bool                             `json:"includeControlPlane,omitempty"`
	MatchFields         []corev1.NodeSelectorRequirement `json:"matchFields,omitempty"`

	compiledSelector labels.Selector
}

// parseTunnelEndpointSelector parses either a legacy label-selector string
// ("", "all", "*", or "k=v,k2 in (a,b)") or a JSON-encoded TunnelEndpointSelector.
func parseTunnelEndpointSelector(raw string) (*TunnelEndpointSelector, labels.Selector, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		var spec TunnelEndpointSelector
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return nil, nil, fmt.Errorf("parsing structured tunnel.endpoints selector: %w", err)
		}
		ls := &metav1.LabelSelector{
			MatchLabels:      spec.MatchLabels,
			MatchExpressions: spec.MatchExpressions,
		}
		sel, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return nil, nil, fmt.Errorf("compiling tunnel.endpoints label selector: %w", err)
		}
		for _, r := range spec.Roles {
			switch strings.ToLower(strings.TrimSpace(r)) {
			case "worker", "workers", "control-plane", "controlplane", "master", "all", "*":
			default:
				return nil, nil, fmt.Errorf("unknown role %q in tunnel.endpoints.roles (supported: worker, control-plane, all)", r)
			}
		}
		for _, mf := range spec.MatchFields {
			switch mf.Key {
			case "metadata.name", "spec.unschedulable":
			default:
				return nil, nil, fmt.Errorf("unsupported matchFields key %q in tunnel.endpoints (supported: metadata.name, spec.unschedulable)", mf.Key)
			}
			switch mf.Operator {
			case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn:
				if len(mf.Values) == 0 {
					return nil, nil, fmt.Errorf("matchFields key %q with operator %s requires non-empty values", mf.Key, mf.Operator)
				}
			default:
				return nil, nil, fmt.Errorf("unsupported matchFields operator %q in tunnel.endpoints (supported: In, NotIn)", mf.Operator)
			}
		}
		spec.compiledSelector = sel
		return &spec, sel, nil
	}
	if isAllNodes(trimmed) {
		return nil, labels.Everything(), nil
	}
	sel, err := labels.Parse(trimmed)
	if err != nil {
		return nil, nil, err
	}
	return nil, sel, nil
}

func (s *TunnelEndpointSelector) namesControlPlane() bool {
	if s == nil {
		return false
	}
	if s.IncludeControlPlane {
		return true
	}
	for _, r := range s.Roles {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "control-plane", "controlplane", "master", "all", "*":
			return true
		}
	}
	for k := range s.MatchLabels {
		if _, ok := platform.ControlPlaneMarks[k]; ok {
			return true
		}
	}
	for _, expr := range s.MatchExpressions {
		if _, ok := platform.ControlPlaneMarks[expr.Key]; ok {
			return true
		}
	}
	return false
}

func (s *TunnelEndpointSelector) allowsWorkers() bool {
	if s == nil || len(s.Roles) == 0 {
		return true
	}
	for _, r := range s.Roles {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "worker", "workers", "all", "*":
			return true
		}
	}
	return false
}

func (s *TunnelEndpointSelector) matchesNode(node *corev1.Node) bool {
	for _, ex := range s.ExcludeNodeNames {
		if strings.TrimSpace(ex) == node.Name {
			return false
		}
	}
	if len(s.NodeNames) > 0 {
		matchedName := false
		for _, n := range s.NodeNames {
			if strings.TrimSpace(n) == node.Name {
				matchedName = true
				break
			}
		}
		if !matchedName {
			return false
		}
	}
	isCP := platform.IsControlPlane(node)
	// Explicit nodeNames select that node even if it is a control-plane node,
	// unless roles explicitly exclude control-plane.
	namedExplicitly := len(s.NodeNames) > 0
	if isCP && !s.namesControlPlane() && !(namedExplicitly && len(s.Roles) == 0) {
		return false
	}
	if !isCP && !s.allowsWorkers() {
		return false
	}
	for _, mf := range s.MatchFields {
		var actual string
		switch mf.Key {
		case "metadata.name":
			actual = node.Name
		case "spec.unschedulable":
			if node.Spec.Unschedulable {
				actual = "true"
			} else {
				actual = "false"
			}
		}
		inValues := false
		for _, v := range mf.Values {
			if v == actual {
				inValues = true
				break
			}
		}
		if mf.Operator == corev1.NodeSelectorOpIn && !inValues {
			return false
		}
		if mf.Operator == corev1.NodeSelectorOpNotIn && inValues {
			return false
		}
	}
	if s.compiledSelector != nil && !s.compiledSelector.Empty() {
		if !s.compiledSelector.Matches(labels.Set(node.Labels)) {
			return false
		}
	}
	return true
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
	if r.tunnelEndpointFilter != nil {
		return r.tunnelEndpointFilter.matchesNode(node)
	}
	if strings.HasPrefix(strings.TrimSpace(r.tunnelEndpointsRaw), "{") {
		if filter, _, err := parseTunnelEndpointSelector(r.tunnelEndpointsRaw); err == nil && filter != nil {
			return filter.matchesNode(node)
		}
	}
	if platform.IsControlPlane(node) && !selectorNamesControlPlane(r.tunnelEndpointsRaw) {
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
		tunnel.NodeTunnelAddress6Prefix, tunnel.NodeTransitAddress6Prefix,
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
		drop(tunnel.NodeTunnelAddress6Prefix + name)
		drop(tunnel.NodeTransitAddress6Prefix + name)
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
		// Preparation is not a forwarding acknowledgement. Only explicit
		// transition completion may release the retained legacy endpoints.
		if _, preparing := adoption.Data[tunnel.ReceivePrepareKey]; preparing {
			continue
		}
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

// relaysFromApplied reports whether each site's addresses are carried by
// a relay in the remote's applied document. Hash freshness alone cannot
// release retention: the current document may still route a node directly.
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
	for key := range platform.ControlPlaneMarks {
		if strings.Contains(raw, key) {
			return true
		}
	}
	return isAllNodes(raw)
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

// Remote nodes fetch peer lists through the tunnel. Publishing a replacement
// and withdrawing its predecessor in one pass can remove the path needed to
// learn the replacement. pruneDeparted and dialerNodeAffinity retain the old
// endpoint until the remote can acknowledge the replacement configuration.

// machinesForBlock maps a pod block to the Machines this operator owns
// whose node holds it.
func (r *meshReconciler) machinesForBlock(ctx context.Context, block client.Object) []reconcile.Request {
	u, ok := block.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	node, _, _ := unstructured.NestedString(u.Object, "spec", "node")
	if node == "" {
		return nil
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind(machineGVK.Kind + "List"))
	opts := []client.ListOption{}
	if r.machineSelector != nil {
		opts = append(opts, client.MatchingLabelsSelector{Selector: r.machineSelector})
	}
	if err := r.List(ctx, machines, opts...); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing machines for a pod block", "node", node)
		return nil
	}
	var out []reconcile.Request
	for i := range machines.Items {
		if r.nodeNameForMachine(ctx, &machines.Items[i]) == node {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: machines.Items[i].GetNamespace(), Name: machines.Items[i].GetName()}})
		}
	}
	return out
}
