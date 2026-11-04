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
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	"github.com/appmana/cloud-provisioning/controller/pkg/controlplane"
	"github.com/appmana/cloud-provisioning/controller/pkg/discover"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joindocker "github.com/appmana/cloud-provisioning/controller/pkg/join/docker"
	joink0s "github.com/appmana/cloud-provisioning/controller/pkg/join/k0s"
	joink3s "github.com/appmana/cloud-provisioning/controller/pkg/join/k3s"
	joinkubeadm "github.com/appmana/cloud-provisioning/controller/pkg/join/kubeadm"
	joinlabcontainers "github.com/appmana/cloud-provisioning/controller/pkg/join/labcontainers"
	joinmicrok8s "github.com/appmana/cloud-provisioning/controller/pkg/join/microk8s"
	joinrke2 "github.com/appmana/cloud-provisioning/controller/pkg/join/rke2"
	"github.com/appmana/cloud-provisioning/controller/pkg/nodegroup"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	platformaws "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws"
	awsiam "github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/iam"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/controller/pkg/tuple"
	tuplecli "github.com/appmana/cloud-provisioning/controller/pkg/tuple/cli"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// machineGVK uses the same CAPI API version as claim creation and attachment
// lifecycle queries. Older served versions depend on the installed CAPI release.
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
const cloudWorkerTaintKey = platform.TaintKey

// cloudWorkerRoleLabel/Value select which Machine this operator
// treats as a remote peer. They are also the default
// --machine-selector value, so the label a node registers with and the
// selector used to find its Machine cannot drift either.
const (
	cloudWorkerRoleLabel = platform.RoleLabel
	cloudWorkerRoleValue = platform.RoleValue
)

// controlPlaneLabel is the conventional control-plane role label and
// taint. Control planes are recognised by platform.IsControlPlane, which
// also knows the labels k0s and MicroK8s use instead, and terminate
// tunnels only when tunnel.endpoints names them. Remotes reach the API
// through a designated worker that masquerades tunnel-sourced traffic.
const controlPlaneLabel = platform.ControlPlaneLabel

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
	siteDialerSelector     labels.Selector
	// nativeNetworks are the provider-native pod networks the registered
	// providers can run; nativeObservers report the configured ones'
	// allocations. Both are keyed by the name a node's class registers.
	nativeNetworks     map[string]platform.Network
	nativeObservers    map[string]platform.Observer
	receivePreparation *receivePreparationPolicy
	receiveJournal     handover.Store
	receiveAPI         kubernetes.Interface
	tunnelSubnet       string
	localAddressBase   string
	// tunnelIPv6Prefix is the prefix IPv6 tunnel addresses are paired
	// into (tunnel.TunnelAddress6), set only when the cluster's pod
	// network is dual-stack. Empty means a single-stack mesh.
	tunnelIPv6Prefix string
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
	// Finish ordinary reconciliation before admitting cleanup. Old/expired
	// receipts must not block agent updates or surviving peer publication.
	defer func() {
		if err != nil {
			return
		}
		result, err = r.finishRetiredReceiveAdoptions(ctx, result)
	}()

	// Node events enqueue a nameless request because they change the endpoint
	// set rather than one Machine. Refresh every remote's adoption document:
	// endpoint changes do not necessarily produce a Machine event.
	if req.Name == "" {
		result := ctrl.Result{}
		if r.receivePreparation != nil {
			result.RequeueAfter = 30 * time.Second
		}
		return result, r.refreshAdoptionConfigs(ctx)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.reader.Get(ctx, req.NamespacedName, machine); err != nil {
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

	if !machine.GetDeletionTimestamp().IsZero() || machine.GetAnnotations()[attachment.DrainIntentAnnotation] != "" {
		return ctrl.Result{}, nil
	}

	// Keep reconciliation pending until the remote Node and its pod allocations
	// are observed and published. Either can appear after Machine events stop;
	// absence of a Node must not disable the periodic retry.
	remoteBlocksPending := true
	// A native network's allocations change without any Kubernetes event,
	// so a node on one is observed again periodically.
	observedNative := false
	nativeNode := false
	nodeName := r.nodeNameForMachine(ctx, machine)
	if nodeName != "" {
		published, err := r.publishRemotePodCIDRs(ctx, machine, nodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		remoteBlocksPending = !published
		node := &corev1.Node{}
		if err := r.reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err == nil {
			nativeNode = node.Labels[platform.CNILabel] != ""
			_, observedNative = r.nativeObservers[node.Labels[platform.CNILabel]]
		}
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
	externalIP, machineAddrs := machineHosts(addresses)
	if externalIP == "" {
		log.V(1).Info("no ExternalIP/InternalIP in status.addresses yet, waiting")
		return ctrl.Result{}, nil
	}

	endpoint := net.JoinHostPort(externalIP, r.port)

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting secret %s: %w", secretKey, err)
	}

	// Per-Machine key (r.secretKey is a prefix, e.g. "peer-endpoint-"),
	// not a flat singleton, so a second cloud Machine does not clobber
	// the first's endpoint entry.
	//
	// The machine's own addresses join its accept list and route
	// hosts, learned here: the dialled address and its node addresses,
	// which differ where a cloud dials a public address. The API server
	// reaches the kubelet at the node address. An encapsulating network addresses its packets to exactly it
	// (flannel's vxlan outers, cilium's tunnel outers), so the site's
	// dialers must both accept it through cryptokey routing and route
	// it into the tunnel; the dialer's fwmark is what makes that route
	// safe for the address the tunnel also dials. The join reconciler
	// cannot write these at render time because the address does not
	// exist yet; this mirror is where it first becomes known.
	//
	// Before the Node exists or on a provider-native network, only the
	// dialled endpoint address is published here; InternalIP addresses on a
	// native network are published atomically with peer-native-network-<machine>
	// in publishRemotePodCIDRs so same-VPC neighbors never observe an unverified
	// underlay address that would later have to be withdrawn.
	endpointAddrs := machineAddrs
	if nodeName == "" || nativeNode {
		endpointAddrs = []string{externalIP}
	}
	machineKey := r.secretKey + machine.GetName()
	allowedKey := tunnel.PeerAllowedIPsPrefix + machine.GetName()
	routeHostsKey := tunnel.PeerRouteHostsPrefix + machine.GetName()
	allowed := tunnel.SplitList(string(secret.Data[allowedKey]))
	routeHosts := tunnel.SplitList(string(secret.Data[routeHostsKey]))
	wantAllowed, wantRouteHosts := allowed, routeHosts
	for _, addr := range endpointAddrs {
		wantAllowed = appendMissing(wantAllowed, tunnel.HostCIDR(addr))
		wantRouteHosts = appendMissing(wantRouteHosts, addr)
	}
	if string(secret.Data[machineKey]) != endpoint ||
		len(wantAllowed) != len(allowed) || len(wantRouteHosts) != len(routeHosts) {
		if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
			return ctrl.Result{}, err
		}
		patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
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

	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring adoption config: %w", err)
	}
	if err := r.ensureDialerRole(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("scoping the dialer Role: %w", err)
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
	if remoteBlocksPending || observedNative || r.receivePreparation != nil {
		// Come back for the blocks this node has not been given yet.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// machineHosts reads a Machine's status.addresses: the address the
// tunnel dials (its first ExternalIP, else its first InternalIP) and
// every address of its own to accept and route on its peer entry, the
// dialled one first and then each InternalIP, the Node's own addresses
// that the API server and the network address it by. Names are not
// addresses and are skipped.
func machineHosts(addresses []any) (string, []string) {
	var external string
	var internal []string
	for _, entry := range addresses {
		address, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		text, _ := address["address"].(string)
		ip := net.ParseIP(text)
		if ip == nil {
			continue
		}
		switch address["type"] {
		case "ExternalIP":
			if external == "" {
				external = ip.String()
			}
		case "InternalIP":
			internal = append(internal, ip.String())
		}
	}
	endpoint := external
	if endpoint == "" && len(internal) > 0 {
		endpoint = internal[0]
	}
	if endpoint == "" {
		return "", nil
	}
	hosts := []string{endpoint}
	for _, addr := range internal {
		hosts = appendMissing(hosts, addr)
	}
	return endpoint, hosts
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

func main() {
	if len(os.Args) > 0 && filepath.Base(os.Args[0]) == "platform-tuple" {
		os.Exit(tuplecli.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == microk8sTokenAgentCommand {
		os.Exit(runMicrok8sTokenAgent(os.Args[2:]))
	}
	if len(os.Args) > 1 && tuplecli.IsCommand(os.Args[1]) {
		os.Exit(tuplecli.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	var (
		machineSelector  string
		secretNamespace  string
		secretName       string
		secretKey        string
		port             string
		metricsAddr      string
		gatewayNamespace string
		gatewayName      string

		tunnelEndpoints       string
		siteDialerSelectorRaw string
		endpointRetention     time.Duration
		dialerPrivateKeyDir   string

		joinEnabled               bool
		joinTemplatePath          string
		joinAPIAddress            string
		joinAPIVIP                string
		joinAPIProxyPort          int
		joinKubeletExtraFlags     string
		joinSSHAuthorizedKeys     string
		joinTokenTTL              time.Duration
		joinProviderName          string
		wireGuardAddress          string
		wireGuardListenPort       string
		localAddressBase          string
		tunnelIPv6Prefix          string
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
		dialerManifestPath        string
		dialerManifestURL         string
		dialerPreferUniversal     bool
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
	siteCNIExclusion := flag.Bool("site-cni-exclusion", true, "keep the site pod network's DaemonSets off provider-native-network nodes by adding a required node-affinity term, and on a Calico site run Calico on tunnel-class cloud nodes with stated addresses and limit BGP peering to site nodes; when false, only verify these before creating a cloud node's machine")
	siteCNIDaemonSets := flag.String("site-cni-daemonsets", "", "comma-separated namespace/name of site pod network DaemonSets that mount no host CNI directory, in addition to those recognised")
	capiWorkloadServiceAccount := flag.String("capi-workload-service-account", "", "ServiceAccount, in the release namespace, whose bound token Cluster API uses on this cluster; enables ImportedControlPlane")
	flag.StringVar(&secretName, "secret-name", "tunnel-peers", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key-prefix", tunnel.PeerEndpointPrefix, "prefix (Machine name is appended) for the Secret key this Machine's endpoint is written into: per-Machine, not a flat singleton")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")
	flag.StringVar(&tunnelEndpoints, "tunnel-endpoints", "", "node selector (k=v,k2=v2) choosing which local nodes terminate tunnels; empty = every Linux worker. Control-plane nodes are excluded unless this selector names their distribution's role label (node-role.kubernetes.io/control-plane, node.k0sproject.io/role or node.kubernetes.io/microk8s-controlplane) or is \"all\"")
	flag.StringVar(&siteDialerSelectorRaw, "site-dialer-node-selector", "", "additional label selector admitting site dialers; empty preserves all Linux site nodes; selected tunnel endpoints must be admitted")
	flag.DurationVar(&endpointRetention, "tunnel-endpoint-retention", 3*time.Minute, "how long a node that has left --tunnel-endpoints goes on being one: dialer still scheduled, entries still published, tunnel still carrying traffic. A remote reads its peer list over the tunnel it is about to lose, so this is the time it is given to read the one naming the replacement, and it is measured in the dialer's 30s poll interval rather than in anything about the departing node's health")
	flag.StringVar(&dialerPrivateKeyDir, "dialer-private-key-dir", "/var/lib/cloud-provisioning", "host directory where each node's dialer keeps its own WireGuard private key (generated on first start; never leaves the node)")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "tunnel-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "cloud-provisioning-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "", "REQUIRED image for the dialer DaemonSets, pinned by digest (tag@sha256:...). Deliberately has no default: a stale built-in default once pointed at a pre-hardening build")
	flag.StringVar(&dialerImagePullPolicy, "dialer-image-pull-policy", "IfNotPresent", "pull policy for generated Linux dialer DaemonSets: Always, IfNotPresent, or Never for preloaded images")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "", "optional comma-separated imagePullSecret names for agent DaemonSets; leave empty when images are publicly pullable. Missing Secrets produce pull-secret warnings, so this defaults to none")
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
	flag.StringVar(&joinKubeletExtraFlags, "join-kubelet-extra-flags", "",
		"extra kubelet flags for every joining cloud-worker node; its labels and taint come from its node class and may not be restated here")
	flag.StringVar(&joinSSHAuthorizedKeys, "join-ssh-authorized-keys", "", "comma-separated SSH public keys to authorize on every new node")
	flag.DurationVar(&joinTokenTTL, "join-token-ttl", 2*time.Hour, "validity window for a minted join token")
	flag.StringVar(&wireGuardAddress, "join-wireguard-address", "10.100.0.128/24", "base WireGuard tunnel address for REMOTE (cloud) nodes; each gets the next free address in this subnet")
	flag.StringVar(&localAddressBase, "tunnel-local-address-base", "10.100.0.1/24", "base WireGuard tunnel address for LOCAL tunnel-endpoint nodes; each selected node gets the next free address in this subnet")
	flag.StringVar(&tunnelIPv6Prefix, "tunnel-ipv6-prefix", "fd00:10:100::/96", "IPv6 prefix (/96 or shorter) for a dual-stack mesh's tunnel addresses: each node's IPv6 tunnel address is its IPv4 one in the low 32 bits. Applied only when the cluster's pod network allocates IPv6; empty keeps every mesh single-stack")
	flag.StringVar(&wireGuardListenPort, "join-wireguard-listen-port", "51820", "WireGuard listen port on the remote side")
	flag.StringVar(&dialerListenPort, "join-dialer-listen-port", "51820", "WireGuard listen port the local dialers expect the remote peer to use")
	flag.StringVar(&bootstrapSecretNameFormat, "join-bootstrap-secret-name-format", "%s-bootstrap", "printf format (with the Machine's name) for the bootstrap Secret's name")
	flag.StringVar(&dialerBinaryURLARM64, "join-dialer-binary-url-arm64", "", "optional override URL cloud-init downloads the arm64 dialer binary from (defaults to the multi-platform release manifest)")
	flag.StringVar(&dialerBinarySHA256ARM64, "join-dialer-binary-sha256-arm64", "", "optional override sha256 of the arm64 dialer binary (defaults to the multi-platform release manifest)")
	flag.StringVar(&dialerBinaryURLAMD64, "join-dialer-binary-url-amd64", "", "optional override URL cloud-init downloads the amd64 dialer binary from (defaults to the multi-platform release manifest)")
	flag.StringVar(&dialerBinarySHA256AMD64, "join-dialer-binary-sha256-amd64", "", "optional override sha256 of the amd64 dialer binary (defaults to the multi-platform release manifest)")
	flag.StringVar(&dialerManifestPath, "join-dialer-manifest-path", join.DefaultDialerManifestPath, "path to the multi-platform dialer release manifest baked into the image")
	flag.StringVar(&dialerManifestURL, "join-dialer-manifest-url", "latest", "multi-platform dialer release manifest URL or 'latest' fallback when --join-dialer-manifest-path is absent")
	flag.BoolVar(&dialerPreferUniversal, "join-dialer-prefer-universal", true, "use the universal multi-arch Linux wg-dialer fat binary when available in the release manifest")
	flag.StringVar(&cniPluginsURLARM64, "join-cni-plugins-url-arm64", "", "optional per-arch containernetworking-plugins tarball URL; required when the cluster's CNI config chains plugins (bandwidth/portmap/tuning) a stock cloud image does not ship")
	flag.StringVar(&cniPluginsSHA256ARM64, "join-cni-plugins-sha256-arm64", "", "sha256 of the arm64 CNI plugins tarball; a URL without it is ignored, never fetched unverified")
	flag.StringVar(&cniPluginsURLAMD64, "join-cni-plugins-url-amd64", "", "amd64 containernetworking-plugins tarball URL")
	flag.StringVar(&cniPluginsSHA256AMD64, "join-cni-plugins-sha256-amd64", "", "sha256 of the amd64 CNI plugins tarball")
	flag.StringVar(&awsConfigNamespace, "aws-config-namespace", "cloud-provisioning", "namespace of the AWS provider-config Secret (AMIs, subnet, security groups, keypair)")
	flag.StringVar(&awsConfigName, "aws-config-name", "aws-provider-config", "name of the AWS provider-config Secret")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	nodeGroups := flag.Bool("experimental-node-groups", false, "enable experimental node groups in the mesh namespace; requires the group CRD and drain RBAC")
	vpcCNIFamily := flag.String("aws-vpc-cni-family", "ipv4", "the AWS VPC CNI's pod address family, ipv4 or ipv6")
	artifactRegion := flag.String("artifact-region", "", "region of the S3 bucket s3:// artifact sources name; the controller signs them when it renders a bootstrap")
	vpcCNI := flag.Bool("aws-vpc-cni", true, "apply the AWS VPC CNI (aws-node) for AWS VPC CNI nodes, configured from the site's ranges")
	vpcCNIImage := flag.String("aws-vpc-cni-image", "", "digest-pinned aws-node image; empty uses the pinned upstream image")
	vpcCNIInitImage := flag.String("aws-vpc-cni-init-image", "", "digest-pinned aws-node init image; empty uses the pinned upstream image")
	windowsIPAMImage := flag.String("aws-windows-ipam-image", "", "digest-pinned patched VPC resource controller; runs Windows IPAM and admits Windows VPC CNI nodes")
	windowsIPAMPullSecrets := flag.String("aws-windows-ipam-pull-secrets", "", "comma-separated kube-system Secrets for pulling the Windows IPAM image")
	receivePolicyJSON := flag.String("receive-preparation-policy", "", "experimental receiver policy JSON with nativeCIDRs and permitted ports; optional journal enables fenced handover and explicit expiry recovery")
	gatewayConfig := flag.String("aws-gateway-config", "", "optional path to AWS Windows Calico gateway scope JSON")
	platformTuple := flag.String("tuple", "", "optional platform tuple expression (e.g. 'k0s + calico,bgp + aws') driving default join provider and cloud CNI")
	awsIAMBootstrap := flag.Bool("aws-iam-bootstrap", false, "reconcile AWS IAM policies, roles, and instance profile at startup using current AWS credentials")
	awsIAMScope := flag.String("aws-iam-scope", "account", "IAM scope when --aws-iam-bootstrap is enabled: 'account' (shared clusterawsadm roles + AWS-Managed Policies) or 'cluster'")
	awsIAMTuple := flag.String("aws-iam-tuple", "k0s + calico,bgp + aws", "platform tuple expression used when --aws-iam-bootstrap is enabled")
	awsIAMCluster := flag.String("aws-iam-cluster", "", "optional CAPI cluster name override when --aws-iam-bootstrap is enabled")
	awsIAMRegion := flag.String("aws-iam-region", "", "optional AWS region override when --aws-iam-bootstrap is enabled")
	flag.Parse()
	explicitFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})
	if strings.TrimSpace(*platformTuple) != "" {
		parsedTuple, err := tuple.Parse(*platformTuple)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --tuple: %v\n", err)
			os.Exit(2)
		}
		if !explicitFlags["join-provider"] {
			joinProviderName = parsedTuple.Distribution
		}
		if !explicitFlags["aws-iam-tuple"] {
			*awsIAMTuple = parsedTuple.String()
		}
		if !explicitFlags["aws-vpc-cni"] && parsedTuple.Provider == "aws" {
			*vpcCNI = parsedTuple.CloudCNI == "aws-vpc"
		}
	}
	receivePolicy, policyErr := parseReceivePreparationPolicy(*receivePolicyJSON, wireGuardListenPort)
	if policyErr != nil {
		fmt.Fprintln(os.Stderr, policyErr)
		os.Exit(2)
	}
	if err := validateReceiptAgentRelease(receivePolicy, dialerImage, dialerCloudImage); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
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

	if err := platform.ValidateExtraFlags(joinKubeletExtraFlags); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --join-kubelet-extra-flags: %v\n", err)
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
	siteDialerSelector, err := labels.Parse(siteDialerSelectorRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --site-dialer-node-selector: %v\n", err)
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
		if joinProviderName == "" || joinProviderName == "auto" {
			if detectedDistro, err := discover.Distro(ctx, discoveryClient); err == nil && detectedDistro != "" {
				joinProviderName = detectedDistro
			} else {
				joinProviderName = "k0s"
			}
		}
		if !explicitFlags["join-template-path"] {
			joinTemplatePath = fmt.Sprintf("/join-patterns/%s-worker.cloud-config.tmpl", joinProviderName)
		}
		network, err = cni.Detect(ctx, discoveryClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine the container network: %v\n", err)
			os.Exit(1)
		}
		dualStack, err := network.DualStack(ctx, discoveryClient)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine whether the container network is dual-stack: %v\n", err)
			os.Exit(1)
		}
		tunnelIPv6Prefix, err = effectiveTunnelIPv6Prefix(tunnelIPv6Prefix, dualStack)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "cluster: api=%s network=%s/%s (%s) dual-stack=%v tunnel-ipv6-prefix=%q\n",
			joinAPIAddress, network.Name, network.Encapsulation, network.Detail, dualStack, tunnelIPv6Prefix)
	}

	// The mesh's interface name is derived from the peer Secret's
	// identity: deterministic on every member, unique per mesh, and
	// never colliding with a node's existing wg0/tailscale devices.
	ifaceName := tunnel.InterfaceName(secretNamespace + "/" + secretName)
	tunnelSubnet := subnetOf(localAddressBase)

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)

	// AWS calls use the SDK's standard credentials.
	awsSDK := &platformaws.SDK{}
	if *awsIAMBootstrap {
		iamCtx, iamCancel := context.WithTimeout(context.Background(), 60*time.Second)
		iamClient, acct, err := awsiam.NewClientFromDefaultConfig(iamCtx, *awsIAMRegion)
		if err != nil {
			iamCancel()
			fmt.Fprintf(os.Stderr, "--aws-iam-bootstrap failed to initialize IAM client: %v\n", err)
			os.Exit(1)
		}
		report, _, err := tuple.ApplyIAMBundle(iamCtx, iamClient, mgr.GetClient(), mgr.GetAPIReader(), tuple.IAMConfig{
			Tuple:                 *awsIAMTuple,
			Scope:                 *awsIAMScope,
			AccountID:             acct,
			Region:                *awsIAMRegion,
			ClusterName:           *awsIAMCluster,
			AssetBucket:           *artifactRegion,
			VPCCNIFamily:          *vpcCNIFamily,
			IncludeAMILifecycle:   true,
			IncludeAMIPerformance: true,
			IncludeObservability:  true,
			AllowedNamespace:      secretNamespace,
		}, false)
		iamCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "--aws-iam-bootstrap failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "aws-iam-bootstrap: reconciled %d changes for cluster %s (%s)\n", len(report.Changes), report.ClusterName, report.AccountID)
	}
	awsProvider := joinaws.Provider{ConfigNamespace: awsConfigNamespace, ConfigName: awsConfigName, EC2: awsSDK.API, WindowsIPAM: *vpcCNI && *windowsIPAMImage != ""}
	dockerProvider := joindocker.Provider{}
	// Adopts a container someone else owns, which is how a lab topology
	// presents a machine. Shipped like the docker provider rather than
	// hidden behind a build tag, so a harness exercises the same binary
	// that is released.
	labcontainersProvider := joinlabcontainers.Provider{}
	infraProviders := []join.InfraProvider{awsProvider, dockerProvider, labcontainersProvider}

	excluder, err := siteExcluder(mgr.GetClient(), mgr.GetAPIReader(), infraProviders, *siteCNIDaemonSets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --site-cni-daemonsets: %v\n", err)
		os.Exit(1)
	}
	if *vpcCNIFamily != "ipv4" && *vpcCNIFamily != "ipv6" {
		fmt.Fprintln(os.Stderr, "--aws-vpc-cni-family must be ipv4 or ipv6")
		os.Exit(1)
	}
	// The VPC CNI's ranges are cluster-internal to a Calico site.
	excluder.ProviderRanges = platformaws.ProviderRanges{Reader: mgr.GetAPIReader(), Family: *vpcCNIFamily, API: awsSDK.API}.Ranges
	if *siteCNIExclusion {
		if err := site.Register(mgr, excluder); err != nil {
			fmt.Fprintf(os.Stderr, "unable to register the site CNI exclusion: %v\n", err)
			os.Exit(1)
		}
	}

	mesh := &meshReconciler{
		nativeNetworks:         nativeNetworksOf(infraProviders),
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
		siteDialerSelector:     siteDialerSelector,
		endpointRetention:      endpointRetention,
		receivePreparation:     receivePolicy,
		tunnelSubnet:           tunnelSubnet,
		localAddressBase:       localAddressBase,
		tunnelIPv6Prefix:       tunnelIPv6Prefix,
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
	}
	if *vpcCNI {
		// Uncached: the facts include the Service range, read from a dry run.
		direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var ipamPullSecrets []string
		for _, ref := range imagePullSecrets(*windowsIPAMPullSecrets) {
			ipamPullSecrets = append(ipamPullSecrets, ref.Name)
		}
		component := &platformaws.VPCCNIComponent{Client: direct, Family: *vpcCNIFamily, Image: *vpcCNIImage, InitImage: *vpcCNIInitImage,
			WindowsIPAMImage: *windowsIPAMImage, WindowsIPAMPullSecrets: ipamPullSecrets}
		if err := component.Register(mgr); err != nil {
			fmt.Fprintf(os.Stderr, "unable to register the AWS VPC CNI: %v\n", err)
			os.Exit(1)
		}
	}
	// The VPC CNI's allocations are observed in the scope of each Machine's
	// AWSCluster.
	mesh.nativeObservers = map[string]platform.Observer{platformaws.VPCCNI{}.Name(): platformaws.Observer{Observer: &awspod.Observer{
		Reader: mgr.GetAPIReader(),
		Scopes: platformaws.ClusterScopes{Reader: mgr.GetAPIReader(), Family: *vpcCNIFamily, API: awsSDK.API, Account: awsSDK.Account},
	}}}
	if receivePolicy != nil && receivePolicy.Journal != nil {
		api, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = mesh.configureReceiveJournal(ctx, api)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "receiver journal configuration: %v\n", err)
			os.Exit(1)
		}
		if receiptPolicy := receivePolicy.Journal.Receipts; receiptPolicy != nil {
			if mesh.dialerCloudImage != "" {
				fmt.Fprintln(os.Stderr, "receipt advancement requires image-entrypoint cloud agents")
				os.Exit(1)
			}
			handler, err := handover.NewReceiptCollectionHandler(func(ctx context.Context, machineUID string) (handover.ReceiptCollection, error) {
				return mesh.resolveReceiveReceiptCollection(ctx, api, *receiptPolicy, machineUID)
			}, 128, func(progress handover.ReceiptProgress) {
				ctrl.Log.WithName("receipts").V(1).Info("receipt collection progress", "progress", progress)
			})
			if err == nil {
				err = mgr.Add(&receiveReceiptServer{policy: *receiptPolicy, handler: handler})
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "receiver receipt configuration: %v\n", err)
				os.Exit(1)
			}
		}
	}
	meshController := ctrl.NewControllerManagedBy(mgr).
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
		}))
	if network.Name == cni.Calico {
		// A remote's blocks are allocated after its Machine stops
		// changing, one family at a time, and more as it fills. Each
		// brings the remote's Machine back to publish it.
		meshController = meshController.Watches(cni.CalicoBlockAffinity(),
			handler.EnqueueRequestsFromMapFunc(mesh.machinesForBlock))
	}
	err = meshController.Complete(mesh)
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

		var dialerManifest *join.DialerManifest
		if m, err := join.ResolveDialerManifest(context.Background(), dialerManifestPath, dialerManifestURL); err == nil {
			dialerManifest = m
		}
		if windowsTunnelSHA256 == "" && dialerManifest != nil && joinProviderName == "k0s" && windowsPublisherImage != "" {
			windowsTunnelSHA256 = dialerManifest.WindowsTunnelSHA256()
		}
		bootstrapRenderers := map[string]join.BootstrapRenderer{"linux": join.PatternBootstrap{Path: joinTemplatePath, Format: bootstrap.CloudConfig}}
		if (windowsWorkerVersion == "") != (windowsWorkerSHA256 == "") || (windowsWorkerVersion != "" && windowsTunnelSHA256 == "") {
			fmt.Fprintln(os.Stderr, "Windows bootstrap options require a pinned tunnel executable; worker overrides also require paired version and SHA256")
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
			InfraProviders: infraProviders,

			BootstrapRenderers: bootstrapRenderers,
			APIVIP:             joinAPIVIP,
			APIProxyPort:       joinAPIProxyPort,
			KubeletExtraFlags:  joinKubeletExtraFlags,
			SSHAuthorizedKeys:  sshKeys,

			WireGuardAddress:        wireGuardAddress,
			WireGuardAddress6Prefix: tunnelIPv6Prefix,
			WireGuardListenPort:     wireGuardListenPort,

			DialerPeerSecretNamespace: secretNamespace,
			DialerPeerSecretName:      secretName,
			DialerListenPort:          dialerListenPort,

			InterfaceName: ifaceName,

			SignArtifact:            awsSDK.SignS3(*artifactRegion),
			DialerBinaryURLARM64:    dialerBinaryURLARM64,
			DialerBinarySHA256ARM64: dialerBinarySHA256ARM64,
			DialerBinaryURLAMD64:    dialerBinaryURLAMD64,
			DialerBinarySHA256AMD64: dialerBinarySHA256AMD64,
			DialerManifest:          dialerManifest,
			DialerManifestPath:      dialerManifestPath,
			DialerManifestURL:       dialerManifestURL,
			DialerPreferUniversal:   dialerPreferUniversal,
			DialerUseLatestURL:      strings.TrimSpace(dialerManifestURL) == "latest",

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

		claimAPI, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create direct claim client: %v\n", err)
			os.Exit(1)
		}
		claimRetirementAge := time.Minute
		if receivePolicy != nil && receivePolicy.Journal != nil {
			claimRetirementAge = receivePolicy.Journal.MaxReceiptAge.Duration
		}
		err = ctrl.NewControllerManagedBy(mgr).
			Named("claim").
			For(&v1alpha1.ProvisionedNodeClaim{}).
			Complete(&claim.Reconciler{
				SiteNetwork: func(ctx context.Context, class platform.Class) error {
					network, err := cni.Detect(ctx, mgr.GetAPIReader())
					if err != nil {
						return err
					}
					return tuple.VerifyClaimEnvironment(ctx, mgr.GetAPIReader(), joinProviderName, network, class, excluder)
				},
				Client:                    claimAPI,
				Workload:                  claimAPI,
				RetirementAPI:             clientset,
				MeshName:                  secretName,
				RetirementMaxAge:          claimRetirementAge,
				Reader:                    mgr.GetAPIReader(),
				Provisioners:              []join.MachineProvisioner{awsProvider, dockerProvider, labcontainersProvider},
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

	if *capiWorkloadServiceAccount != "" {
		if err := controlplane.Register(mgr, secretNamespace, *capiWorkloadServiceAccount); err != nil {
			fmt.Fprintf(os.Stderr, "unable to register the imported control plane: %v\n", err)
			os.Exit(1)
		}
	}
	if *nodeGroups {
		if !joinEnabled {
			fmt.Fprintln(os.Stderr, "experimental node groups require --join-enabled")
			os.Exit(1)
		}
		retirementAge := time.Minute
		if receivePolicy != nil && receivePolicy.Journal != nil {
			retirementAge = receivePolicy.Journal.MaxReceiptAge.Duration
		}
		if err := nodegroup.Register(mgr, secretNamespace, secretName, joinAPIVIP, apiServerPortOf(joinAPIAddress), bootstrapSecretNameFormat, retirementAge); err != nil {
			fmt.Fprintf(os.Stderr, "unable to register node groups: %v\n", err)
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

// effectiveTunnelIPv6Prefix is the IPv6 tunnel prefix the mesh uses:
// the configured one on a dual-stack pod network, none on a single-stack
// one. A configured prefix that cannot hold an IPv4 address is refused
// at startup rather than at the first allocation.
func effectiveTunnelIPv6Prefix(configured string, dualStack bool) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", nil
	}
	if _, err := tunnel.TunnelAddress6(configured, "10.100.0.1"); err != nil {
		return "", fmt.Errorf("--tunnel-ipv6-prefix: %w", err)
	}
	if !dualStack {
		return "", nil
	}
	return configured, nil
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
