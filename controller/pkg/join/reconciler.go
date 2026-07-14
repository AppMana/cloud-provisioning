// Reconciler watches Machine objects and, for any whose bootstrap
// Secret doesn't exist yet, provisions it: generates a WireGuard
// keypair, asks a ClusterJoinProvider for join credentials, asks an
// InfraProvider whether the underlying infrastructure is ready,
// renders the join-pattern template, creates the bootstrap Secret, and
// updates wg-dialer-peer so the new node is accepted into the
// full-mesh tunnel. Adding a new cluster technology or infrastructure
// provider means a new ClusterJoinProvider/InfraProvider
// implementation, never touching this reconciler.
package join

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// isMissingCRD reports whether err indicates the requested Kind isn't
// registered with the API server at all -- i.e. the CRD that defines
// it (owned by another operator, e.g. CAPA's AWSMachine) isn't
// installed yet. This is expected during bootstrap ordering (this
// reconciler can start before CAPA does) and should be treated as
// "not ready yet, requeue quietly", never as a hard error.
func isMissingCRD(err error) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	// The dynamic/unstructured client can surface a missing CRD as a
	// plain "no matches for kind" / "could not find the requested
	// resource" error message rather than a typed meta.NoKindMatchError,
	// depending on server version -- checking the message is the only
	// reliable cross-version signal for that case.
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "the server could not find the requested resource")
}

// crdRecheckInterval paces retries while waiting for another operator's
// CRD (e.g. CAPA's) to appear -- long enough not to hot-loop against a
// slow-starting dependency, short enough that provisioning starts
// promptly once it does.
const crdRecheckInterval = 30 * time.Second

var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

// NodeVIPAnnotation records the vip0 address allocated to a cloud
// worker Machine, so a later reconcile (or a future Machine) can find
// the next free one without needing separate state storage.
const NodeVIPAnnotation = "cloud-provisioning.appmana.com/node-vip4"

// Reconciler provisions bootstrap Secrets for cloud-worker Machines.
type Reconciler struct {
	client.Client
	Reader client.Reader

	Join ClusterJoinProvider

	// InfraProviders is every registered infrastructure provider (AWS,
	// a containernet-backed test double, ...). Which one applies to a
	// given Machine is inferred from its spec.infrastructureRef.kind,
	// matched against each provider's own GVK() -- never hardcoded
	// here, so adding a new cloud/test provider means registering it,
	// not branching this reconciler.
	InfraProviders []InfraProvider

	// TemplatePath is the join-pattern template to render (e.g.
	// join-patterns/k0s-worker.cloud-config.tmpl).
	TemplatePath string

	// Static, cluster-topology values this reconciler contributes
	// directly (not provider-specific): the API VIP reachable once the
	// tunnel is up, kubelet taint/label args, and the SSH keys to
	// authorize on every new node.
	APIVIP            string
	KubeletExtraArgs  string
	SSHAuthorizedKeys []string

	// WireGuard tunnel config for the cloud side's own interface.
	WireGuardAddress    string // e.g. "10.100.0.2/24"
	WireGuardListenPort string // e.g. "51820"

	// Node VIP range for Calico autodetection (vip0): allocated as
	// <NodeVIP4Base + n>, avoiding the on-prem nodes' own fixed
	// addresses (.1/.2/.3 in this cluster).
	NodeVIP4Prefix string // e.g. "10.101.0."
	NodeVIP6Prefix string // e.g. "fd8f:cf26:522a::"
	NodeVIPStart   int    // e.g. 4

	// wg-dialer-peer Secret (namespace/name) holding every on-prem
	// node's dialer identity -- this is where the peer list for the
	// cloud side's own wg0.conf comes from, and where the new node's
	// public key gets recorded so on-prem dialers accept it.
	DialerPeerSecretNamespace string
	DialerPeerSecretName      string
	DialerListenPort          string

	// BootstrapSecretName is templated with the Machine's name, e.g.
	// "%s-bootstrap".
	BootstrapSecretNameFormat string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	bootstrapSecretName := fmt.Sprintf(r.BootstrapSecretNameFormat, machine.GetName())

	existing := &corev1.Secret{}
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: machine.GetNamespace(), Name: bootstrapSecretName}, existing)
	if err == nil {
		// Already provisioned -- nothing to do. (Re-provisioning after
		// a spec change is a delete-and-let-us-recreate operation, same
		// as every other CAPA spec-immutability case this project has
		// already hit -- not something this reconciler second-guesses.)
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("checking for existing bootstrap secret: %w", err)
	}

	infraRefName, found, err := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "name")
	if err != nil || !found {
		log.V(1).Info("machine has no infrastructureRef yet, waiting")
		return ctrl.Result{}, nil
	}
	infraRefKind, found, err := unstructured.NestedString(machine.Object, "spec", "infrastructureRef", "kind")
	if err != nil || !found {
		log.V(1).Info("machine's infrastructureRef has no kind yet, waiting")
		return ctrl.Result{}, nil
	}
	infra := r.infraProviderFor(infraRefKind)
	if infra == nil {
		return ctrl.Result{}, fmt.Errorf("no InfraProvider registered for infrastructureRef kind %q", infraRefKind)
	}

	infraMachine := &unstructured.Unstructured{}
	infraMachine.SetGroupVersionKind(infra.GVK())
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: machine.GetNamespace(), Name: infraRefName}, infraMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if isMissingCRD(err) {
			log.Info("infrastructure provider's CRD isn't installed yet, waiting", "gvk", infra.GVK())
			return ctrl.Result{RequeueAfter: crdRecheckInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting infrastructure resource %s: %w", infraRefName, err)
	}

	ready, err := infra.Ready(ctx, infraMachine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking infra readiness: %w", err)
	}
	if !ready {
		log.V(1).Info("infrastructure not ready yet, waiting")
		return ctrl.Result{}, nil
	}

	log.Info("provisioning bootstrap secret", "machine", req.NamespacedName)

	cloudPriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating wireguard keypair: %w", err)
	}
	cloudPub := cloudPriv.PublicKey()

	dialerSecret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.DialerPeerSecretNamespace, Name: r.DialerPeerSecretName}, dialerSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting dialer peer secret: %w", err)
	}
	peers, err := onPremPeers(dialerSecret)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("deriving on-prem peer list: %w", err)
	}

	nodeVIPIndex, err := r.allocateNodeVIPIndex(ctx, machine.GetLabels())
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("allocating node VIP: %w", err)
	}
	nodeVIP4 := fmt.Sprintf("%s%d", r.NodeVIP4Prefix, nodeVIPIndex)
	nodeVIP6 := fmt.Sprintf("%s%d", r.NodeVIP6Prefix, nodeVIPIndex)

	joinValues, err := r.Join.JoinValues(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting cluster join values: %w", err)
	}
	infraValues, err := infra.InfraValues(ctx, infraMachine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infra values: %w", err)
	}

	values := map[string]any{
		"sshAuthorizedKeys": r.SSHAuthorizedKeys,
		"apiVIP":            r.APIVIP,
		"nodeVIP4":          nodeVIP4,
		"nodeVIP6":          nodeVIP6,
		"kubeletExtraArgs":  r.KubeletExtraArgs,
		"wireguard": map[string]any{
			"address":    r.WireGuardAddress,
			"listenPort": r.WireGuardListenPort,
			"privateKey": cloudPriv.String(),
			"peers":      peers,
		},
	}
	for k, v := range joinValues {
		values[k] = v
	}
	for k, v := range infraValues {
		values[k] = v
	}

	rendered, err := render.Pattern(r.TemplatePath, values)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rendering join pattern: %w", err)
	}

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapSecretName,
			Namespace: machine.GetNamespace(),
		},
		Type: "cluster.x-k8s.io/secret",
		StringData: map[string]string{
			"value":  rendered,
			"format": "cloud-config",
		},
	}
	if err := r.Create(ctx, bootstrapSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating bootstrap secret: %w", err)
	}

	// Record the new node's public key so on-prem dialers accept it,
	// and stamp the allocated VIP onto the Machine so future
	// allocations don't reuse it.
	patch := client.MergeFrom(dialerSecret.DeepCopy())
	if dialerSecret.Data == nil {
		dialerSecret.Data = map[string][]byte{}
	}
	dialerSecret.Data["peer-public-key"] = []byte(cloudPub.String())
	dialerSecret.Data["peer-endpoint"] = []byte("pending")
	if err := r.Patch(ctx, dialerSecret, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating dialer peer secret: %w", err)
	}

	machinePatch := client.MergeFrom(machine.DeepCopy())
	annotations := machine.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[NodeVIPAnnotation] = strconv.Itoa(nodeVIPIndex)
	machine.SetAnnotations(annotations)
	if err := r.Patch(ctx, machine, machinePatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("annotating machine with allocated VIP: %w", err)
	}

	log.Info("bootstrap secret provisioned", "machine", req.NamespacedName, "nodeVIP4", nodeVIP4)
	return ctrl.Result{}, nil
}

// infraProviderFor finds the registered InfraProvider whose GVK.Kind
// matches a Machine's spec.infrastructureRef.kind (e.g. "AWSMachine",
// "ContainernetMachine"), or nil if none is registered for it. This
// is the whole of the reconciler's provider-selection logic -- it is
// deliberately never anything more specific than a Kind comparison,
// so registering a new InfraProvider is the only thing needed to
// support a new infrastructure.
func (r *Reconciler) infraProviderFor(kind string) InfraProvider {
	for _, p := range r.InfraProviders {
		if p.GVK().Kind == kind {
			return p
		}
	}
	return nil
}

// onPremPeers derives the cloud side's wg0.conf peer list from
// wg-dialer-peer's per-node private keys (public keys are derived, not
// stored separately -- the Secret only ever needed to hold what each
// dialer needs for itself), local tunnel addresses, and each node's
// real cluster VIP(s) (a "cluster-vip-<node>" key, comma-separated,
// e.g. "10.101.0.1,fd8f:cf26:522a::1" for jarvis -- every node here is
// genuinely dual-stack, confirmed against the live cluster's own
// Node.status.addresses, so this is not a hypothetical). All of the
// tunnel address plus every cluster VIP must be in AllowedIPs, not
// just the tunnel address, confirmed necessary in practice: BGP
// traffic uses the real cluster VIP(s), not the tunnel address, and
// WireGuard only decrypts/routes traffic matching a peer's
// AllowedIPs -- an on-prem node's IPv6 cluster traffic would be
// silently undeliverable through the tunnel if only its IPv4 VIP were
// listed.
func onPremPeers(secret *corev1.Secret) ([]map[string]string, error) {
	var peers []map[string]string
	for key, val := range secret.Data {
		const prefix = "dialer-private-key-"
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		nodeName := strings.TrimPrefix(key, prefix)
		priv, err := wgtypes.ParseKey(strings.TrimSpace(string(val)))
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", key, err)
		}
		localAddr, ok := secret.Data["local-address-"+nodeName]
		if !ok {
			return nil, fmt.Errorf("secret has %s but no matching local-address-%s", key, nodeName)
		}
		clusterVIPs, ok := secret.Data["cluster-vip-"+nodeName]
		if !ok {
			return nil, fmt.Errorf("secret has %s but no matching cluster-vip-%s", key, nodeName)
		}
		tunnelAddr := strings.SplitN(strings.TrimSpace(string(localAddr)), "/", 2)[0]
		allowedIPs := []string{hostCIDR(tunnelAddr)}
		for _, vip := range strings.Split(string(clusterVIPs), ",") {
			vip = strings.TrimSpace(vip)
			if vip == "" {
				continue
			}
			allowedIPs = append(allowedIPs, hostCIDR(vip))
		}
		pub := priv.PublicKey()
		peers = append(peers, map[string]string{
			"publicKey":  pub.String(),
			"allowedIPs": strings.Join(allowedIPs, ", "),
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i]["publicKey"] < peers[j]["publicKey"] })
	return peers, nil
}

// hostCIDR appends the correct single-host prefix length for addr's
// family -- /32 for IPv4, /128 for IPv6. The cluster is dual-stack
// (Calico vip0 carries both an IPv4 and an IPv6 cluster VIP per node),
// so any address flowing through here, tunnel or cluster VIP alike,
// may genuinely be IPv6, not just IPv4 with a differently-shaped
// string. Hardcoding /32 against an IPv6 literal produces an address
// that either fails to parse as WireGuard AllowedIPs or, worse,
// silently matches the wrong host count.
func hostCIDR(addr string) string {
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return addr + "/128"
	}
	return addr + "/32"
}

// allocateNodeVIPIndex finds the next free node-VIP index by scanning
// existing cloud-worker Machines' NodeVIPAnnotation, starting from
// NodeVIPStart. No separate allocator state needed.
func (r *Reconciler) allocateNodeVIPIndex(ctx context.Context, labels map[string]string) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err := r.List(ctx, list); err != nil {
		return 0, err
	}
	maxIndex := r.NodeVIPStart - 1
	for _, m := range list.Items {
		v, ok := m.GetAnnotations()[NodeVIPAnnotation]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		if n > maxIndex {
			maxIndex = n
		}
	}
	return maxIndex + 1, nil
}
