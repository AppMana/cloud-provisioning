// Command endpoint-controller is the cloud-provisioning operator. It
// runs three reconcilers over one manager:
//
//   - claim: expands a ProvisionedNodeClaim (the ONE resource a user
//     commits) into the CAPI Machine + provider machine pair.
//   - join: renders the tunnel-bootstrapping userdata into each
//     Machine's own bootstrap Secret.
//   - mesh (this file): owns the tunnel mesh -- allocates each selected
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
// current Cluster API, and a stale version here silently produced a
// watch that never fired (so no DaemonSet was ever created).
var machineGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}

var gatewayGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "Gateway",
}

const externalDNSTargetAnnotation = "external-dns.alpha.kubernetes.io/target"

// cloudWorkerTaintKey is the ONE taint every cloud-worker node
// registers itself with (via kubelet's own --register-with-taints,
// baked into the join-pattern template) and the ONE toleration the
// public Gateway's own data-plane DaemonSet must have. The on-prem
// dialer DaemonSet deliberately does not tolerate it.
//
// A single Go constant, not independently-configured flag defaults,
// precisely because letting the taint key drift between where it's
// applied and where it's tolerated was a real bug caught live: a node
// ended up with a taint nothing tolerated, so its own Gateway
// data-plane pod could never schedule.
const cloudWorkerTaintKey = "cloud-provisioning.appmana.com/internet-facing"

// cloudWorkerRoleLabel/Value select which Machine this operator
// treats as a remote peer -- also the default --machine-selector
// value, so the label a node registers with and the selector used to
// find its Machine can't drift either.
const (
	cloudWorkerRoleLabel = "cloud-provisioning.appmana.com/role"
	cloudWorkerRoleValue = "cloud-worker"
)

// controlPlaneLabel marks control-plane nodes. The on-prem dialer
// DaemonSet excludes them by nodeAffinity, not merely by lacking a
// toleration: the other side of a cloud tunnel must never land on a
// controller. Control planes therefore carry no WireGuard interface
// and no tunnel routes at all -- the jarvis incident class is excluded
// by scheduling, not only by code. Remotes reach the API through a
// designated worker that masquerades tunnel-sourced traffic.
const controlPlaneLabel = "node-role.kubernetes.io/control-plane"

// meshReconciler owns the tunnel mesh.
type meshReconciler struct {
	client.Client
	// reader is the manager's uncached API reader. The Secret, Gateway
	// and DaemonSets are read once per reconcile, never watched --
	// routing those Gets through the cached client would make
	// controller-runtime start cluster-wide informers for those types,
	// needing list/watch RBAC this identity deliberately doesn't have.
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

	// Dialer DaemonSets: this operator owns both specs directly --
	// there is no CRD and they are never hand-authored in gitops.
	dialerDaemonSetName   string
	dialerServiceAccount  string
	dialerImage           string
	dialerImagePullSecret string
	dialerPodCIDRs        string
	dialerServiceCIDRs    string
	dialerPrivateKeyDir   string
	ifaceName             string
	apiVIP                string

	dialerCloudDaemonSetName string
	dialerCloudListenPort    string
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
	// SET, not any one Machine): mesh-wide maintenance above is all
	// they ask for. An empty-name Get would be a non-NotFound error --
	// an infinite error requeue, not a no-op.
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
	// immutable userdata -- which is how post-join corrections (pod/
	// service CIDRs, added or removed peers, changed endpoints) ever
	// reach the node at all. Never contains a private key: this
	// document lands on an internet-facing machine.
	if err := r.ensureAdoptionConfig(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring adoption config: %w", err)
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
	// real cloud's public address), otherwise InternalIP -- some
	// providers (CAPD containers, private-addressed infra) only ever
	// report internal addresses, and for them that IS the reachable
	// endpoint. Never invented here: absent both, keep waiting.
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

	// Per-Machine key (r.secretKey is a PREFIX, e.g. "peer-endpoint-"),
	// not a flat singleton -- a second cloud Machine must never clobber
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
		// The peer Secret is controller-managed state -- nothing else
		// should have to create it (no manual steps, no gitops-authored
		// Secret for a controller-owned object).
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.secretNamespace, Name: r.secretName}}
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
		// Cluster VIPs: the node's real addresses, which is what BGP
		// sessions and kubelet traffic actually use -- the tunnel
		// address alone is not enough for either.
		var vips []string
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address != "" {
				vips = append(vips, a.Address)
			}
		}
		vipKey := tunnel.NodeClusterVIPsPrefix + node.Name
		joined := strings.Join(vips, ",")
		if joined != "" && string(secret.Data[vipKey]) != joined {
			secret.Data[vipKey] = []byte(joined)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, secret, patch)
}

// isTunnelEndpoint reports whether a node should terminate tunnels:
// Linux, and matching the tunnelEndpoints selector. Control-plane
// nodes are excluded unless the selector names them explicitly --
// "the other side of the tunnel does not land on a controller" is the
// default, overridable only by asking for it.
func (r *meshReconciler) isTunnelEndpoint(node *corev1.Node) bool {
	if node.Labels["kubernetes.io/os"] == "windows" {
		return false
	}
	if _, isCP := node.Labels[controlPlaneLabel]; isCP && !selectorNamesControlPlane(r.tunnelEndpointsRaw) {
		return false
	}
	if r.tunnelEndpointSelector == nil || r.tunnelEndpointSelector.Empty() {
		return true
	}
	return r.tunnelEndpointSelector.Matches(labels.Set(node.Labels))
}

// selectorNamesControlPlane reports whether the operator was asked,
// explicitly, to place tunnels on control-plane nodes.
func selectorNamesControlPlane(raw string) bool {
	return strings.Contains(raw, controlPlaneLabel)
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
func (r *meshReconciler) ensureAdoptionConfig(ctx context.Context, machine *unstructured.Unstructured) error {
	peerSecret := &corev1.Secret{}
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}, peerSecret); err != nil {
		return fmt.Errorf("getting peer secret: %w", err)
	}
	selfTunnelAddr := strings.SplitN(strings.TrimSpace(machine.GetAnnotations()["cloud-provisioning.appmana.com/wireguard-addr4"]), "/", 2)[0]
	peers, err := tunnel.RemotePeers(peerSecret.Data, selfTunnelAddr, r.apiVIP)
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.secretNamespace},
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
// DaemonSet directly -- no CRD, no gitops YAML for its pod spec. Its
// desired state is entirely computed from this operator's own
// constants/flags plus the Secret it manages, so it can never drift
// out of sync with what this operator expects (the actual root cause
// of a real bug caught live: a hand-authored DaemonSet's AllowedIPs
// and taint values were wrong with no way to be kept honest).
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

	hostPathDirectoryOrCreate := corev1.HostPathDirectoryOrCreate
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.dialerDaemonSetName,
			Namespace: r.secretNamespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": r.dialerDaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": r.dialerDaemonSetName}},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: r.dialerServiceAccount,
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
							},
							Args: []string{
								fmt.Sprintf("--secret-namespace=%s", r.secretNamespace),
								fmt.Sprintf("--secret-name=%s", r.secretName),
								fmt.Sprintf("--iface=%s", r.ifaceName),
								fmt.Sprintf("--private-key-file=%s/private.key", r.dialerPrivateKeyDir),
								fmt.Sprintf("--pod-cidrs=%s", r.dialerPodCIDRs),
								fmt.Sprintf("--service-cidrs=%s", r.dialerServiceCIDRs),
								fmt.Sprintf("--transit-masquerade-source=%s", r.tunnelSubnet),
								"--keepalive-seconds=15",
								"--mtu=1420",
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "dialer-key", MountPath: r.dialerPrivateKeyDir},
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
	var out []corev1.NodeSelectorRequirement
	for _, term := range strings.Split(raw, ",") {
		term = strings.TrimSpace(term)
		if term == "" || !strings.Contains(term, "=") || strings.Contains(term, "!=") {
			continue
		}
		parts := strings.SplitN(term, "=", 2)
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "" || value == "" {
			continue
		}
		out = append(out, corev1.NodeSelectorRequirement{
			Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{value},
		})
	}
	return out
}

// ensureCloudDialerDaemonSet creates or updates the remote-side dialer
// DaemonSet -- the same binary, scheduled onto ONLY the cloud-worker
// node(s) (nodeSelector + toleration for the cloud-worker taint, the
// exact opposite of the on-prem DaemonSet's scheduling). It keeps its
// IDENTITY from the /etc/wg-dialer/peers.json cloud-init wrote
// (hostPath, read-only: the private key must never travel through the
// API) but takes its PEER LIST from the per-machine adoption Secret
// this operator re-renders every reconcile -- so post-join
// corrections reach a node whose userdata is immutable.
//
// This deliberately does NOT disable the wg-dialer.service systemd
// unit cloud-init installed: both converge on the same kernel
// interface (ConfigureDevice is idempotent; nothing ever calls
// LinkDel), and if this pod could never schedule, a disabled
// bootstrap tunnel would leave the node with no path back to the API
// at all -- undoing the one guarantee that must always hold. What the
// DaemonSet buys is a Kubernetes-native upgrade path (bump
// --dialer-image, rolling update) instead of host binary swaps.
func (r *meshReconciler) ensureCloudDialerDaemonSet(ctx context.Context) error {
	hostPathDirectory := corev1.HostPathDirectory
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.dialerCloudDaemonSetName,
			Namespace: r.secretNamespace,
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
							Image:           r.dialerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
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
								// machine's own name is node-local DATA
								// (cloud-init wrote it), never a per-node flag
								// baked into this template.
								"--machine-name-file=/etc/wg-dialer/machine-name",
								// The image becomes the upgrade channel once the
								// node has joined: this copy installs itself over
								// the host binary the bootstrap unit runs, so a
								// fleet upgrade is one digest bump in gitops and
								// the download URL only ever mattered at first
								// boot.
								"--install-host-binary=/host-bin/wg-dialer",
								fmt.Sprintf("--listen-port=%s", r.dialerCloudListenPort),
								fmt.Sprintf("--pod-cidrs=%s", r.dialerPodCIDRs),
								fmt.Sprintf("--service-cidrs=%s", r.dialerServiceCIDRs),
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
		nodeVIP4Prefix            string
		nodeVIP6Prefix            string
		nodeVIPStart              int
		dialerListenPort          string
		bootstrapSecretNameFormat string
		dialerDaemonSetName       string
		dialerServiceAccount      string
		dialerImage               string
		dialerImagePullSecret     string
		dialerPodCIDRs            string
		dialerServiceCIDRs        string
		dialerCloudDaemonSetName  string
		dialerBinaryURLARM64      string
		dialerBinarySHA256ARM64   string
		dialerBinaryURLAMD64      string
		dialerBinarySHA256AMD64   string
		awsConfigNamespace        string
		awsConfigName             string
	)
	flag.StringVar(&machineSelector, "machine-selector", fmt.Sprintf("%s=%s", cloudWorkerRoleLabel, cloudWorkerRoleValue),
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key-prefix", tunnel.PeerEndpointPrefix, "prefix (Machine name is appended) for the Secret key this Machine's endpoint is written into -- per-Machine, not a flat singleton")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")
	flag.StringVar(&tunnelEndpoints, "tunnel-endpoints", "", "node selector (k=v,k2=v2) choosing which local nodes terminate tunnels; empty = every Linux worker. Control-plane nodes are excluded unless this selector names node-role.kubernetes.io/control-plane explicitly")
	flag.StringVar(&dialerPrivateKeyDir, "dialer-private-key-dir", "/var/lib/cloud-provisioning", "host directory where each node's dialer keeps its own WireGuard private key (generated on first start; never leaves the node)")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "wg-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "wg-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "", "REQUIRED image for the dialer DaemonSets, pinned by digest (tag@sha256:...). Deliberately has no default: a stale built-in default once pointed at a pre-hardening build")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "ghcr-pull", "imagePullSecret for the dialer DaemonSets")
	flag.StringVar(&dialerPodCIDRs, "dialer-pod-cidrs", "", "REQUIRED comma-separated cluster pod-CIDR ranges (v4/v6) -- WireGuard cryptokey accept-list only, never a kernel route. Empty silently drops all Calico traffic, so it is fatal instead")
	flag.StringVar(&dialerServiceCIDRs, "dialer-service-cidrs", "", "REQUIRED comma-separated cluster service-CIDR ranges (v4/v6), same treatment as --dialer-pod-cidrs")
	flag.StringVar(&dialerCloudDaemonSetName, "dialer-cloud-daemonset-name", "wg-dialer-cloud", "name of the remote-side dialer DaemonSet this operator provisions directly")

	flag.BoolVar(&joinEnabled, "join-enabled", true, "enable bootstrap-secret provisioning (join.Reconciler) and claim expansion -- the whole point of this operator; disable only for an endpoint-mirror-only deployment")
	flag.StringVar(&joinProviderName, "join-provider", "k0s", "which cluster technology's join specialization mints join credentials (k0s, kubeadm); pair with the matching --join-template-path")
	flag.StringVar(&joinTemplatePath, "join-template-path", "/join-patterns/k0s-worker.cloud-config.tmpl", "path to the join-pattern template to render")
	flag.StringVar(&joinAPIAddress, "join-api-address", "", "REQUIRED cluster API server address used to mint join tokens (bracket IPv6 literals, e.g. https://[fd8f:cf26:522a::1]:6443)")
	flag.StringVar(&joinAPIVIP, "join-api-vip", "", "REQUIRED cluster API VIP the new node must reach through the tunnel before joining")
	flag.StringVar(&joinKubeletExtraArgs, "join-kubelet-extra-args",
		fmt.Sprintf("--node-labels=%s=%s --register-with-taints=%s:NoSchedule", cloudWorkerRoleLabel, cloudWorkerRoleValue, cloudWorkerTaintKey),
		"extra kubelet args applied to every joining cloud-worker node -- defaults derived from the same constants the DaemonSet toleration and --machine-selector default use, so they can't drift")
	flag.StringVar(&joinSSHAuthorizedKeys, "join-ssh-authorized-keys", "", "comma-separated SSH public keys to authorize on every new node")
	flag.DurationVar(&joinTokenTTL, "join-token-ttl", 2*time.Hour, "validity window for a minted join token")
	flag.StringVar(&wireGuardAddress, "join-wireguard-address", "10.100.0.128/24", "base WireGuard tunnel address for REMOTE (cloud) nodes; each gets the next free address in this subnet")
	flag.StringVar(&localAddressBase, "tunnel-local-address-base", "10.100.0.1/24", "base WireGuard tunnel address for LOCAL tunnel-endpoint nodes; each selected node gets the next free address in this subnet")
	flag.StringVar(&wireGuardListenPort, "join-wireguard-listen-port", "51820", "WireGuard listen port on the remote side")
	flag.StringVar(&nodeVIP4Prefix, "join-node-vip4-prefix", "", "REQUIRED IPv4 prefix for allocated Calico vip0 addresses (e.g. 10.101.0.)")
	flag.StringVar(&nodeVIP6Prefix, "join-node-vip6-prefix", "", "IPv6 prefix for allocated Calico vip0 addresses (e.g. fd8f:cf26:522a::)")
	flag.IntVar(&nodeVIPStart, "join-node-vip-start", 200, "first node-VIP index to allocate (must not collide with existing node addresses)")
	flag.StringVar(&dialerListenPort, "join-dialer-listen-port", "51820", "WireGuard listen port the local dialers expect the remote peer to use")
	flag.StringVar(&bootstrapSecretNameFormat, "join-bootstrap-secret-name-format", "%s-bootstrap", "printf format (with the Machine's name) for the bootstrap Secret's name")
	flag.StringVar(&dialerBinaryURLARM64, "join-dialer-binary-url-arm64", "", "REQUIRED (arm64 nodes) URL cloud-init downloads the dialer binary from; nothing installs it on a stock image")
	flag.StringVar(&dialerBinarySHA256ARM64, "join-dialer-binary-sha256-arm64", "", "REQUIRED (arm64 nodes) sha256 of that binary, verified by cloud-init before the tunnel unit starts")
	flag.StringVar(&dialerBinaryURLAMD64, "join-dialer-binary-url-amd64", "", "URL cloud-init downloads the amd64 dialer binary from")
	flag.StringVar(&dialerBinarySHA256AMD64, "join-dialer-binary-sha256-amd64", "", "sha256 of the amd64 binary")
	flag.StringVar(&awsConfigNamespace, "aws-config-namespace", "wg-dialer", "namespace of the AWS provider-config Secret (AMIs, subnet, security groups, keypair)")
	flag.StringVar(&awsConfigName, "aws-config-name", "aws-provider-config", "name of the AWS provider-config Secret")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Fatal-on-empty for every value whose absence is silent breakage
	// rather than a visible error: an empty dialer image once meant a
	// stale pre-hardening build, and empty pod/service CIDRs mean
	// WireGuard drops all Calico traffic with no log anywhere.
	required := map[string]string{
		"--dialer-image":          dialerImage,
		"--dialer-pod-cidrs":      dialerPodCIDRs,
		"--dialer-service-cidrs":  dialerServiceCIDRs,
		"--join-api-address":      joinAPIAddress,
		"--join-api-vip":          joinAPIVIP,
		"--join-node-vip4-prefix": nodeVIP4Prefix,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			fmt.Fprintf(os.Stderr, "%s is required\n", name)
			os.Exit(1)
		}
	}

	selector, err := labels.Parse(machineSelector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --machine-selector: %v\n", err)
		os.Exit(1)
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
		// tunnel-endpoint set, so they must trigger allocation --
		// otherwise a newly-selected node waits for an unrelated
		// Machine event before it ever gets a tunnel address.
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
			tunnelEndpointsRaw:     tunnelEndpoints,
			tunnelSubnet:           tunnelSubnet,
			localAddressBase:       localAddressBase,
			dialerDaemonSetName:    dialerDaemonSetName,
			dialerServiceAccount:   dialerServiceAccount,
			dialerImage:            dialerImage,
			dialerImagePullSecret:  dialerImagePullSecret,
			dialerPodCIDRs:         dialerPodCIDRs,
			dialerServiceCIDRs:     dialerServiceCIDRs,
			dialerPrivateKeyDir:    dialerPrivateKeyDir,
			ifaceName:              ifaceName,
			apiVIP:                 joinAPIVIP,

			dialerCloudDaemonSetName: dialerCloudDaemonSetName,
			dialerCloudListenPort:    dialerListenPort,
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

		// Each cluster technology is one SPECIALIZATION of
		// join.ClusterJoinProvider behind the same seam -- selection is
		// by name, each implementation's own knobs live in its own
		// provider-config Secret (the aws-provider-config pattern),
		// never in this binary's generic flags.
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

			PodCIDRs:     dialerPodCIDRs,
			ServiceCIDRs: dialerServiceCIDRs,

			WireGuardAddress:    wireGuardAddress,
			WireGuardListenPort: wireGuardListenPort,

			NodeVIP4Prefix: nodeVIP4Prefix,
			NodeVIP6Prefix: nodeVIP6Prefix,
			NodeVIPStart:   nodeVIPStart,

			DialerPeerSecretNamespace: secretNamespace,
			DialerPeerSecretName:      secretName,
			DialerListenPort:          dialerListenPort,

			InterfaceName: ifaceName,

			DialerBinaryURLARM64:    dialerBinaryURLARM64,
			DialerBinarySHA256ARM64: dialerBinarySHA256ARM64,
			DialerBinaryURLAMD64:    dialerBinaryURLAMD64,
			DialerBinarySHA256AMD64: dialerBinarySHA256AMD64,

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

// subnetOf turns "10.100.0.1/24" into "10.100.0.0/24" -- the tunnel
// subnet the transit masquerade rule is scoped to.
func subnetOf(base string) string {
	_, ipNet, err := net.ParseCIDR(base)
	if err != nil {
		return ""
	}
	return ipNet.String()
}
