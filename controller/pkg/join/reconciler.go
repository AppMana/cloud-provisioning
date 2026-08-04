// Reconciler watches Machine objects and, for any whose bootstrap
// Secret doesn't exist yet, provisions it: generates the cloud node's
// WireGuard keypair, asks a ClusterJoinProvider for join credentials,
// renders the join-pattern template, creates the bootstrap Secret
// (owned by the Machine, so deletion cascades), and updates the peer
// Secret so the new node is accepted into the mesh. Adding a new
// cluster technology or infrastructure provider means a new
// ClusterJoinProvider/InfraProvider implementation, never touching
// this reconciler.
//
// The mesh this reconciler emits is fully connected between the
// topologically local side and every topologically isolated remote:
// each remote peers with every selected local tunnel-endpoint node and
// with every other remote. Isolated remotes share no LAN, so without
// remote-to-remote peers they could not reach each other.
package join

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
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
// registered with the API server at all, meaning the CRD that defines
// it (owned by another operator, e.g. CAPA's AWSMachine) isn't
// installed yet. This is expected during bootstrap ordering, since
// this reconciler can start before CAPA does, and is treated as "not
// ready yet, requeue quietly" rather than a hard error.
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
	// depending on server version. Checking the message is the only
	// reliable cross-version signal for that case.
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "the server could not find the requested resource")
}

// crdRecheckInterval paces retries while waiting for another operator's
// CRD (e.g. CAPA's) to appear: long enough not to hot-loop against a
// slow-starting dependency, short enough that provisioning starts
// promptly once it does.
const crdRecheckInterval = 30 * time.Second

var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

// WireGuardAddrAnnotation records the WireGuard tunnel address
// allocated to a cloud worker Machine, mirroring NodeVIPAnnotation.
//
// Each cloud Machine gets its own distinct address, allocated the same
// way node VIPs are. A shared literal would give a second cloud
// Machine an identical WireGuard AllowedIPs entry and kernel
// RouteHost, which is undefined for WireGuard cryptokey routing (two
// peers cannot both claim the same AllowedIPs destination) and
// ambiguous for the on-prem dialer's kernel route.
const WireGuardAddrAnnotation = "cloud-provisioning.appmana.com/wireguard-addr4"

// Reconciler provisions bootstrap Secrets for cloud-worker Machines.
type Reconciler struct {
	client.Client
	Reader client.Reader

	Join ClusterJoinProvider

	// InfraProviders is every registered infrastructure provider (AWS,
	// a Docker-backed test double, ...). Which one applies to a given
	// Machine is inferred from its spec.infrastructureRef.kind, matched
	// against each provider's own GVK() rather than hardcoded here, so
	// adding a new cloud/test provider means registering it, not
	// branching this reconciler.
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

	// PodCIDRs/ServiceCIDRs are the cluster's own declared ranges
	// (from the cluster's k0sctl/site config at gitops-render time).
	// Comma-separated; fed to the cloud dialer's --pod-cidrs/
	// --service-cidrs (WireGuard cryptokey accept-list only, not a
	// kernel route; see cmd/dialer/main.go's package doc).
	PodCIDRs     string
	ServiceCIDRs string

	// WireGuardAddress is the base tunnel address (e.g.
	// "10.100.0.2/24"). The first cloud Machine gets exactly this;
	// each subsequent one gets the next free address in the same
	// prefix (see WireGuardAddrAnnotation).
	WireGuardAddress    string
	WireGuardListenPort string

	// Peer Secret (namespace/name): where tunnel-endpoint nodes have
	// published their public keys and the controller has allocated
	// their tunnel addresses (node-* keys), and where this reconciler
	// records the new machine's peer-* entries.
	DialerPeerSecretNamespace string
	DialerPeerSecretName      string
	DialerListenPort          string

	// InterfaceName is the mesh's unique tunnel interface name
	// (tunnel.InterfaceName over the peer Secret identity), rendered
	// into the cloud node's systemd unit.
	InterfaceName string

	// Dialer binary delivery: the cloud node's stock image has no
	// wg-dialer; cloud-init downloads it from this per-arch URL and
	// verifies the pinned digest before enabling the unit. sha256 is
	// pinned at render time, so a compromised download host cannot swap
	// the binary.
	DialerBinaryURLARM64    string
	DialerBinarySHA256ARM64 string
	DialerBinaryURLAMD64    string
	DialerBinarySHA256AMD64 string

	// Standard containernetworking plugins. A cluster's CNI config may
	// chain bandwidth/portmap/tuning; calico-node installs only its
	// own, and a stock cloud image has none, so without these every
	// pod sandbox on a provisioned node fails. Same per-arch, sha-pinned
	// delivery as the dialer binary.
	CNIPluginsURLARM64    string
	CNIPluginsSHA256ARM64 string
	CNIPluginsURLAMD64    string
	CNIPluginsSHA256AMD64 string

	// BootstrapSecretNameFormat is templated with the Machine's name,
	// e.g. "%s-bootstrap".
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
		// Already provisioned, nothing to do. Re-provisioning after a
		// spec change is a delete-and-recreate operation, as with any
		// other CAPA spec-immutability case.
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

	// Not gated on the infrastructure resource being "ready" (e.g.
	// AWSMachine's instance running): cloud-init user-data has to exist
	// before the underlying compute launches, for any infrastructure
	// provider, since it is the boot mechanism rather than a post-boot
	// artifact. Waiting for "ready" first would deadlock, because the
	// infra provider is commonly blocked on this Secret existing: CAPA's
	// AWSMachine controller does not call RunInstances until the
	// bootstrap Secret is there. All that is needed here is that the
	// infrastructureRef target object exists.
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

	// Optional preflight: providers whose underlying operator (CAPA,
	// ...) can silently retry forever on a misconfiguration get a
	// chance to surface that clearly here instead.
	if v, ok := infra.(Validator); ok {
		if err := v.Validate(ctx, r.Reader, infraMachine); err != nil {
			return ctrl.Result{}, fmt.Errorf("validating infrastructure configuration: %w", err)
		}
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

	cloudWGAddress, err := r.allocateWireGuardAddress(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("allocating wireguard address: %w", err)
	}
	// The cloud node cannot read a cluster Secret before it joins, so
	// its bootstrap peer list travels in cloud-init as a plain JSON
	// file the same dialer binary reads via --peers-file. The peers are
	// every selected local tunnel-endpoint node (from the node-* keys
	// the local dialers published and the controller allocated), plus
	// each other cloud machine already in the mesh, since isolated
	// remotes share no LAN. On the cloud side a local peer's Endpoint
	// is empty: the cloud node only listens for the on-prem side, which
	// is behind NAT with no inbound path. A remote peer's Endpoint is
	// its public address, because two remotes have no NAT between them
	// and dial each other directly.
	cloudTunnelAddrEarly := strings.SplitN(strings.TrimSpace(cloudWGAddress), "/", 2)[0]
	peers, err := tunnel.RemotePeers(dialerSecret.Data, cloudTunnelAddrEarly, r.apiServers(dialerSecret))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("deriving mesh peer list: %w", err)
	}
	if len(peers) == 0 {
		// No local tunnel endpoint has published yet: rendering now
		// would bake an empty, useless peer list into immutable
		// userdata. Wait for the mesh side to exist first.
		log.Info("no published tunnel-endpoint nodes in the peer secret yet, waiting")
		return ctrl.Result{RequeueAfter: crdRecheckInterval}, nil
	}

	peersFileJSON, err := json.Marshal(tunnel.PeersFileDoc{
		PrivateKey:   cloudPriv.String(),
		LocalAddress: cloudWGAddress,
		Peers:        peers,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshaling cloud-side peers file: %w", err)
	}

	joinValues, err := r.Join.JoinValues(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting cluster join values: %w", err)
	}
	infraValues, err := infra.InfraValues(ctx, infraMachine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infra values: %w", err)
	}

	if err := r.validateDialerBinaries(); err != nil {
		return ctrl.Result{}, err
	}

	values := map[string]any{
		"sshAuthorizedKeys":       r.SSHAuthorizedKeys,
		"apiVIP":                  r.APIVIP,
		"kubeletExtraArgs":        r.KubeletExtraArgs,
		"wireguardAddress":        cloudWGAddress,
		"wireguardListenPort":     r.WireGuardListenPort,
		"podCIDRs":                r.PodCIDRs,
		"serviceCIDRs":            r.ServiceCIDRs,
		"peersFileJSON":           string(peersFileJSON),
		"interfaceName":           r.InterfaceName,
		"dialerBinaryURLArm64":    r.DialerBinaryURLARM64,
		"dialerBinarySHA256Arm64": r.DialerBinarySHA256ARM64,
		"dialerBinaryURLAmd64":    r.DialerBinaryURLAMD64,
		"dialerBinarySHA256Amd64": r.DialerBinarySHA256AMD64,
		"cniPluginsURLArm64":      r.CNIPluginsURLARM64,
		"cniPluginsSHA256Arm64":   r.CNIPluginsSHA256ARM64,
		"cniPluginsURLAmd64":      r.CNIPluginsURLAMD64,
		"cniPluginsSHA256Amd64":   r.CNIPluginsSHA256AMD64,
		"machineName":             machine.GetName(),
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

	// Owned by the Machine: `kubectl delete machine` (or a claim
	// cascade) garbage-collects this Secret, so a re-created Machine
	// gets a fresh render. Join tokens expire (TTL ~2h), and reusing a
	// stale Secret strands a re-created node at the join step with no
	// error reported.
	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapSecretName,
			Namespace: machine.GetNamespace(),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: machineGVK.GroupVersion().String(),
				Kind:       machineGVK.Kind,
				Name:       machine.GetName(),
				UID:        machine.GetUID(),
			}},
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

	// Record the new node's peer entry so local dialers accept and
	// route to it. Per-Machine-keyed (peer-*-<machine>), so a second
	// cloud Machine does not clobber the first's entry.
	//
	// AllowedIPs vs RouteHosts (see cmd/dialer/main.go's package doc):
	// both carry the machine's tunnel address, which is the address
	// the CNI is told to peer on. AllowedIPs because WireGuard's
	// cryptokey filter matches inner destinations, RouteHosts because
	// node-to-node reachability is this tunnel layer's only routing
	// job. Everything wider (pod blocks) is the CNI's concern, learned
	// over the sessions these host routes make possible.
	patch := client.MergeFrom(dialerSecret.DeepCopy())
	if dialerSecret.Data == nil {
		dialerSecret.Data = map[string][]byte{}
	}
	machineName := machine.GetName()
	cloudTunnelAddr := strings.SplitN(strings.TrimSpace(cloudWGAddress), "/", 2)[0]
	allowed := []string{tunnel.HostCIDR(cloudTunnelAddr)}
	routeHosts := []string{cloudTunnelAddr}
	dialerSecret.Data[tunnel.PeerPublicKeyPrefix+machineName] = []byte(cloudPub.String())
	dialerSecret.Data[tunnel.PeerEndpointPrefix+machineName] = []byte(tunnel.PeerEndpointPending)
	dialerSecret.Data[tunnel.PeerAllowedIPsPrefix+machineName] = []byte(strings.Join(allowed, ","))
	dialerSecret.Data[tunnel.PeerRouteHostsPrefix+machineName] = []byte(strings.Join(routeHosts, ","))
	if err := r.Patch(ctx, dialerSecret, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating dialer peer secret: %w", err)
	}

	machinePatch := client.MergeFrom(machine.DeepCopy())
	annotations := machine.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[WireGuardAddrAnnotation] = cloudWGAddress
	machine.SetAnnotations(annotations)
	if err := r.Patch(ctx, machine, machinePatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("annotating machine with its allocated tunnel address: %w", err)
	}

	log.Info("bootstrap secret provisioned", "machine", req.NamespacedName, "tunnelAddress", cloudTunnelAddr)
	return ctrl.Result{}, nil
}

// apiServers is every control-plane address a remote must reach: the
// list the mesh reconciler discovered (all control planes), falling
// back to the single configured VIP. k0s workers load-balance across
// all of them via nllb, so publishing only one leaves the remote
// dependent on that single control plane staying up.
func (r *Reconciler) apiServers(dialerSecret *corev1.Secret) []string {
	if raw, ok := dialerSecret.Data[tunnel.APIServersKey]; ok {
		if servers := tunnel.SplitList(string(raw)); len(servers) > 0 {
			return servers
		}
	}
	if r.APIVIP == "" {
		return nil
	}
	return []string{r.APIVIP}
}

// infraProviderFor finds the registered InfraProvider whose GVK.Kind
// matches a Machine's spec.infrastructureRef.kind, or nil if none is
// registered for it. The match is a Kind comparison and nothing more
// specific, so registering a new InfraProvider is all that is needed
// to support a new infrastructure.
func (r *Reconciler) infraProviderFor(kind string) InfraProvider {
	for _, p := range r.InfraProviders {
		if p.GVK().Kind == kind {
			return p
		}
	}
	return nil
}

// validateDialerBinaries refuses to render userdata that would fetch a
// binary it cannot verify. The bootstrap picks by architecture at boot,
// so both entries travel in every rendered document, and a URL without
// its digest is a supply-chain hole rather than a fallback.
func (r *Reconciler) validateDialerBinaries() error {
	pairs := []struct {
		arch, url, sha string
	}{
		{"arm64", r.DialerBinaryURLARM64, r.DialerBinarySHA256ARM64},
		{"amd64", r.DialerBinaryURLAMD64, r.DialerBinarySHA256AMD64},
	}
	configured := 0
	for _, p := range pairs {
		if p.url == "" && p.sha == "" {
			continue
		}
		if p.url == "" || p.sha == "" {
			return fmt.Errorf("dialer binary for %s needs both a URL and its sha256", p.arch)
		}
		configured++
	}
	if configured == 0 {
		return fmt.Errorf("no dialer binary configured for any architecture")
	}
	return nil
}

// allocateWireGuardAddress finds the next free cloud tunnel address by
// scanning existing cloud-worker Machines' WireGuardAddrAnnotation,
// starting from r.WireGuardAddress (the base address). Mirrors
// allocateNodeVIPIndex; see WireGuardAddrAnnotation's doc comment for
// why each Machine needs a distinct address.
func (r *Reconciler) allocateWireGuardAddress(ctx context.Context) (string, error) {
	ip, ipNet, err := net.ParseCIDR(r.WireGuardAddress)
	if err != nil {
		return "", fmt.Errorf("parsing base WireGuardAddress %q: %w", r.WireGuardAddress, err)
	}
	prefixLen, _ := ipNet.Mask.Size()
	baseIP4 := ip.To4()
	if baseIP4 == nil {
		return "", fmt.Errorf("WireGuardAddress %q must be IPv4", r.WireGuardAddress)
	}
	prefix := fmt.Sprintf("%d.%d.%d.", baseIP4[0], baseIP4[1], baseIP4[2])
	startIndex := int(baseIP4[3])

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"})
	if err := r.List(ctx, list); err != nil {
		return "", err
	}
	maxIndex := startIndex - 1
	for _, m := range list.Items {
		v, ok := m.GetAnnotations()[WireGuardAddrAnnotation]
		if !ok {
			continue
		}
		allocIP, _, err := net.ParseCIDR(v)
		if err != nil {
			continue
		}
		allocIP4 := allocIP.To4()
		if allocIP4 == nil || fmt.Sprintf("%d.%d.%d.", allocIP4[0], allocIP4[1], allocIP4[2]) != prefix {
			continue
		}
		if n := int(allocIP4[3]); n > maxIndex {
			maxIndex = n
		}
	}
	return fmt.Sprintf("%s%d/%d", prefix, maxIndex+1, prefixLen), nil
}
