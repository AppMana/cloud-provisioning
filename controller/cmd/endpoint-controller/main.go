// Command wg-dialer-endpoint-controller watches core Cluster API Machine
// objects and mirrors their external address into the wg-dialer DaemonSet's
// peer Secret, so the cluster-side dialer always has an up to date endpoint
// for the node it's dialing -- without anything having to know AWSMachine's
// schema exists.
//
// Cluster API's own Machine controller copies the address up from
// whatever infrastructure provider sits underneath (AWSMachine today,
// anything else later) into Machine.status.addresses automatically. That
// is the one thing this controller depends on; it never reads AWSMachine
// (or any other provider-specific type) directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joink0s "github.com/appmana/cloud-provisioning/controller/pkg/join/k0s"
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
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var machineGVK = schema.GroupVersionKind{
	Group:   "cluster.x-k8s.io",
	Version: "v1beta1",
	Kind:    "Machine",
}

var gatewayGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "Gateway",
}

const externalDNSTargetAnnotation = "external-dns.alpha.kubernetes.io/target"

// cloudWorkerTaintKey is the ONE taint every cloud-worker node
// registers itself with (via kubelet's own --register-with-taints,
// baked into the join-pattern template) and the ONE toleration the
// on-prem wg-dialer DaemonSet must NOT have (so it never schedules
// onto the cloud-worker node itself -- that node dials no one, it's
// dialed) while the public Gateway's own DaemonSet (created entirely
// separately, outside this operator) DOES tolerate it.
//
// This is a single Go constant, not two independently-configured
// flag defaults, precisely because letting the taint key drift
// between where it's applied and where it's tolerated was a real bug
// caught live: a node ended up with a taint nothing tolerated, so its
// own Gateway data-plane pod could never schedule.
const cloudWorkerTaintKey = "cloud-provisioning.appmana.com/internet-facing"

// cloudWorkerRoleLabel/Value select which Machine this operator
// treats as the cloud-worker peer -- also the default
// --machine-selector value, so the label a node registers with and
// the selector used to find its Machine can't drift either.
const (
	cloudWorkerRoleLabel = "cloud-provisioning.appmana.com/role"
	cloudWorkerRoleValue = "cloud-worker"
)

// Historical note: this used to be defaultDialerAllowedIPs, a single
// flag value fed into BOTH WireGuard's own peer config AND a literal
// kernel-route-installation loop in cmd/dialer/main.go -- a real
// incident (jarvis, AllowedIPs=0.0.0.0/0) hijacked the node's entire
// routing table this way. The dialer no longer takes a single conflated
// --allowed-ips value at all: WireGuard's own accept-list (real cluster
// pod-CIDR/service-CIDR ranges, --pod-cidrs/--service-cidrs below) and
// the kernel route it installs (always a single narrow host address per
// peer, derived automatically, never configurable) are now genuinely
// separate mechanisms -- see cmd/dialer/main.go's package doc.

// reconciler mirrors one Machine's external address into a Secret key.
// It never watches or reads the Secret it writes -- the dialer DaemonSet
// (manifests/wg-dialer/dialer.yaml) polls the mounted file itself and
// re-applies `wg set` when it changes, so there is nothing for this
// controller to coordinate beyond a plain patch.
//
// Optionally (when gatewayNamespace/gatewayName are set) it also stamps
// the same IP onto a Gateway's external-dns target annotation. This
// exists because a Gateway whose data plane is pinned to one node via
// hostPorts (rather than a cloud LoadBalancer) has no dependable
// Gateway.status.addresses for external-dns to read -- the annotation
// bypasses that entirely, and this controller already has the one piece
// of information (the node's real external IP) that annotation needs.
//
// This must be the Gateway, not the HTTPRoute: confirmed by reading
// external-dns's gateway.go (gatewayRouteResolver.resolve) --
// annotations.TargetsFromTargetAnnotation is called on gw.gateway, the
// parent Gateway object, never on the route. A target annotation on the
// HTTPRoute itself is silently ignored.
type reconciler struct {
	client.Client
	// reader is the manager's uncached API reader. The Secret and
	// Gateway are read once per reconcile, never watched -- routing
	// those Gets through the normal cached client would make
	// controller-runtime start a cluster-wide list+watch informer for
	// the whole type, which needs list/watch RBAC this identity
	// deliberately doesn't have (it only has get, scoped to the one
	// named object). The cached Client is still used for the Machine
	// watch (which is the actual thing meant to be watched) and for
	// Patch, which doesn't go through the cache either way.
	reader           client.Reader
	secretNamespace  string
	secretName       string
	secretKey        string
	port             string
	gatewayNamespace string
	gatewayName      string

	// Dialer DaemonSet provisioning: this operator owns the on-prem
	// dialer DaemonSet's full spec directly -- there is no separate
	// CRD, and it is never hand-authored in gitops. It's provisioned
	// unconditionally alongside every reconcile of a matching Machine
	// (not gated on anything Machine-specific): dialing out to the
	// cloud worker is simply always wanted, whenever this operator is
	// running at all.
	dialerDaemonSetName   string
	dialerServiceAccount  string
	dialerImage           string
	dialerImagePullSecret string
	dialerPodCIDRs        string
	dialerServiceCIDRs    string

	// Cloud-worker dialer DaemonSet: the SAME dialer binary, running
	// containerized (hostNetwork, NET_ADMIN) on the cloud-worker node
	// itself once it has joined as a real k8s Node -- see
	// ensureCloudDialerDaemonSet's doc comment for why this coexists
	// with, rather than replaces, the wg-dialer.service systemd unit
	// cloud-init already installed at bootstrap.
	dialerCloudDaemonSetName string
	dialerCloudListenPort    string
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Provisioning the on-prem dialer DaemonSet is unconditional: we
	// always want the tunnel, it isn't gated on this specific Machine
	// reaching any particular state. Doing it first means it exists
	// even before the peer's ExternalIP/endpoint is known (the dialer
	// itself tolerates "peer-endpoint: pending" already).
	if err := r.ensureDialerDaemonSet(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring dialer daemonset: %w", err)
	}
	if err := r.ensureCloudDialerDaemonSet(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring cloud dialer daemonset: %w", err)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
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

	var externalIP string
	for _, entry := range addresses {
		address, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if address["type"] != "ExternalIP" {
			continue
		}
		if ip, ok := address["address"].(string); ok && ip != "" {
			externalIP = ip
			break
		}
	}
	if externalIP == "" {
		log.V(1).Info("no ExternalIP in status.addresses yet, waiting")
		return ctrl.Result{}, nil
	}

	endpoint := fmt.Sprintf("%s:%s", externalIP, r.port)

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting secret %s: %w", secretKey, err)
	}

	// Per-Machine key (r.secretKey is a PREFIX, e.g. "peer-endpoint-"),
	// not a flat singleton -- a second cloud Machine matching the
	// selector must never clobber the first's endpoint entry.
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

// ensureDialerDaemonSet creates or updates the on-prem wg-dialer
// DaemonSet directly -- no CRD, no gitops YAML for its pod spec. Its
// desired state is entirely computed from this operator's own fixed
// Go constants/flags (image, AllowedIPs, the shared taint) plus the
// Secret it already manages, so it can never drift out of sync with
// what this operator itself expects (the actual root cause of a real
// bug caught live: a hand-authored DaemonSet's AllowedIPs and taint
// values were wrong and had no way to be kept honest against reality).
func (r *reconciler) ensureDialerDaemonSet(ctx context.Context) error {
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
					// Control-plane toleration only: this DaemonSet
					// must run on every ON-PREM node (including
					// jarvis, a control-plane node), but must NEVER
					// run on the cloud-worker node itself -- it dials
					// out to that node, it doesn't run there. Since
					// it has no toleration for cloudWorkerTaintKey,
					// the scheduler excludes it from that node
					// automatically; the Gateway's own DaemonSet
					// (created entirely separately) is the one thing
					// that does tolerate it.
					Tolerations: []corev1.Toleration{
						{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: r.dialerImagePullSecret}},
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           r.dialerImage,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
							},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
							},
							Args: []string{
								fmt.Sprintf("--secret-namespace=%s", r.secretNamespace),
								fmt.Sprintf("--secret-name=%s", r.secretName),
								"--iface=wg0",
								"--private-key-secret-key=dialer-private-key-$(NODE_NAME)",
								"--local-address-secret-key=local-address-$(NODE_NAME)",
								fmt.Sprintf("--pod-cidrs=%s", r.dialerPodCIDRs),
								fmt.Sprintf("--service-cidrs=%s", r.dialerServiceCIDRs),
								"--keepalive-seconds=15",
								"--mtu=1420",
								"--poll-interval=30s",
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

// ensureCloudDialerDaemonSet creates or updates a SECOND dialer
// DaemonSet -- the same binary as ensureDialerDaemonSet's, but
// scheduled onto ONLY the cloud-worker node(s) (nodeSelector +
// toleration for cloudWorkerTaintKey, the exact opposite of the
// on-prem DaemonSet's scheduling, which structurally excludes this
// node instead). It reads its peer config from the same
// /etc/wg-dialer/peers.json file cloud-init already wrote at
// Machine-provisioning time (hostPath-mounted read-only), via
// --peers-file -- there is no in-cluster Secret this DaemonSet reads or
// needs RBAC for.
//
// This deliberately does NOT replace or disable the wg-dialer.service
// systemd unit cloud-init installed: both reconcile to the identical
// desired state (same peers.json, same pod/service CIDRs) against the
// same kernel WireGuard device, so running both is harmless
// redundancy, not a conflict -- ConfigureDevice is idempotent, and wg0
// itself is a kernel-resident device that outlives any single
// process's lifetime (confirmed: nothing in cmd/dialer ever calls
// LinkDel on shutdown). This was a deliberate design choice, not an
// oversight: the alternative -- having this DaemonSet somehow signal
// the systemd unit to stop once it takes over -- creates exactly the
// failure mode this must never have: if the DaemonSet's own pod can
// never schedule (a kubelet/CNI problem, the DaemonSet controller
// being down, anything), a bootstrap tunnel that had already been
// disabled would leave the node with no path back to the API server at
// all, undoing the one guarantee that must always hold. Leaving the
// systemd unit alone forever means it can never be the thing that
// broke. What this DaemonSet buys instead is a normal, Kubernetes-native
// upgrade path going forward: bumping --dialer-image and letting a
// rolling DaemonSet update happen is how the binary gets updated on an
// already-provisioned cloud-worker node -- no host-level binary-swap or
// systemctl-restart machinery needed, because Kubernetes' own pod
// lifecycle already does the equivalent (image pull, container
// restart, readiness-gated rollout) for a plain containerized process.
func (r *reconciler) ensureCloudDialerDaemonSet(ctx context.Context) error {
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
					HostNetwork: true,
					// Opposite of ensureDialerDaemonSet's scheduling:
					// this pod must run ONLY on the cloud-worker
					// node(s), never on-prem. nodeSelector matches the
					// same role label the join-pattern's kubelet args
					// already register the node with; the toleration
					// is for the one taint that same node registers
					// itself with, which is exactly what excludes the
					// ON-PREM DaemonSet from ever landing here (see
					// ensureDialerDaemonSet's own comment).
					NodeSelector: map[string]string{cloudWorkerRoleLabel: cloudWorkerRoleValue},
					Tolerations: []corev1.Toleration{
						{Key: cloudWorkerTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: r.dialerImagePullSecret}},
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           r.dialerImage,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
							},
							Args: []string{
								"--iface=wg0",
								"--peers-file=/etc/wg-dialer/peers.json",
								fmt.Sprintf("--listen-port=%s", r.dialerCloudListenPort),
								fmt.Sprintf("--pod-cidrs=%s", r.dialerPodCIDRs),
								fmt.Sprintf("--service-cidrs=%s", r.dialerServiceCIDRs),
								"--keepalive-seconds=15",
								"--mtu=1420",
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "wg-dialer-config", MountPath: "/etc/wg-dialer", ReadOnly: true},
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

		// join.Reconciler: automated bootstrap-secret provisioning,
		// replacing what used to be a manual per-node join process.
		joinEnabled               bool
		joinTemplatePath          string
		joinAPIAddress            string
		joinAPIVIP                string
		joinKubeletExtraArgs      string
		joinSSHAuthorizedKeys     string
		joinTokenTTL              time.Duration
		wireGuardAddress          string
		wireGuardListenPort       string
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
	)
	flag.StringVar(&machineSelector, "machine-selector", fmt.Sprintf("%s=%s", cloudWorkerRoleLabel, cloudWorkerRoleValue),
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key-prefix", "peer-endpoint-", "prefix (Machine name is appended) for the Secret key this Machine's endpoint is written into -- per-Machine, not a flat singleton, so multiple cloud Machines never clobber each other's entry")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "wg-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "wg-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "ghcr.io/appmana/cloud-provisioning-dialer:e1f8655", "image for the dialer DaemonSet's container")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "ghcr-pull", "imagePullSecret for the dialer DaemonSet")
	flag.StringVar(&dialerPodCIDRs, "dialer-pod-cidrs", "", "comma-separated cluster pod-CIDR ranges (v4/v6), sourced from cluster/k0sctl.yaml's own declared podCIDR at gitops-render time -- fed to the on-prem dialer's WireGuard peer config (cryptokey-routing accept-list only, never a kernel route -- see cmd/dialer/main.go's package doc)")
	flag.StringVar(&dialerServiceCIDRs, "dialer-service-cidrs", "", "comma-separated cluster service-CIDR ranges (v4/v6), same treatment as --dialer-pod-cidrs, sourced from k0sctl.yaml's serviceCIDR")
	flag.StringVar(&dialerCloudDaemonSetName, "dialer-cloud-daemonset-name", "wg-dialer-cloud", "name of the cloud-worker dialer DaemonSet this operator provisions directly -- coexists with, does not replace, the wg-dialer.service systemd unit cloud-init installs at bootstrap (see ensureCloudDialerDaemonSet's doc comment)")

	flag.BoolVar(&joinEnabled, "join-enabled", false, "enable the bootstrap-secret provisioning reconciler (join.Reconciler)")
	flag.StringVar(&joinTemplatePath, "join-template-path", "/join-patterns/k0s-worker.cloud-config.tmpl", "path to the join-pattern template to render")
	flag.StringVar(&joinAPIAddress, "join-api-address", "https://10.101.0.1:6443", "cluster API server address used to mint k0s join tokens (bracket IPv6 literals, e.g. https://[fd8f:cf26:522a::1]:6443)")
	flag.StringVar(&joinAPIVIP, "join-api-vip", "10.101.0.1", "cluster API VIP the new node must reach through the tunnel before joining")
	flag.StringVar(&joinKubeletExtraArgs, "join-kubelet-extra-args",
		fmt.Sprintf("--node-labels=%s=%s --register-with-taints=%s:NoSchedule", cloudWorkerRoleLabel, cloudWorkerRoleValue, cloudWorkerTaintKey),
		"extra kubelet args applied to every joining cloud-worker node -- defaults derived from the same cloudWorkerTaintKey/RoleLabel constants the dialer DaemonSet's toleration and this operator's own --machine-selector default use, so they can't drift out of sync")
	flag.StringVar(&joinSSHAuthorizedKeys, "join-ssh-authorized-keys", "", "comma-separated SSH public keys to authorize on every new node")
	flag.DurationVar(&joinTokenTTL, "join-token-ttl", 2*time.Hour, "validity window for a minted k0s bootstrap token")
	flag.StringVar(&wireGuardAddress, "join-wireguard-address", "10.100.0.2/24", "WireGuard tunnel address assigned to the cloud side")
	flag.StringVar(&wireGuardListenPort, "join-wireguard-listen-port", "51820", "WireGuard listen port on the cloud side")
	flag.StringVar(&nodeVIP4Prefix, "join-node-vip4-prefix", "10.101.0.", "IPv4 prefix for allocated Calico vip0 addresses")
	flag.StringVar(&nodeVIP6Prefix, "join-node-vip6-prefix", "fd8f:cf26:522a::", "IPv6 prefix for allocated Calico vip0 addresses")
	flag.IntVar(&nodeVIPStart, "join-node-vip-start", 4, "first node-VIP index to allocate (avoids the on-prem nodes' own fixed .1/.2/.3 addresses)")
	flag.StringVar(&dialerListenPort, "join-dialer-listen-port", "51820", "WireGuard listen port the on-prem dialer expects the cloud peer to use")
	flag.StringVar(&bootstrapSecretNameFormat, "join-bootstrap-secret-name-format", "%s-bootstrap", "printf format (with the Machine's name) for the bootstrap Secret's name")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	selector, err := labels.Parse(machineSelector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --machine-selector: %v\n", err)
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

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)

	err = ctrl.NewControllerManagedBy(mgr).
		For(machine, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return selector.Matches(labels.Set(obj.GetLabels()))
		}))).
		Complete(&reconciler{
			Client:                mgr.GetClient(),
			reader:                mgr.GetAPIReader(),
			secretNamespace:       secretNamespace,
			secretName:            secretName,
			secretKey:             secretKey,
			port:                  port,
			gatewayNamespace:      gatewayNamespace,
			gatewayName:           gatewayName,
			dialerDaemonSetName:   dialerDaemonSetName,
			dialerServiceAccount:  dialerServiceAccount,
			dialerImage:           dialerImage,
			dialerImagePullSecret: dialerImagePullSecret,
			dialerPodCIDRs:        dialerPodCIDRs,
			dialerServiceCIDRs:    dialerServiceCIDRs,

			dialerCloudDaemonSetName: dialerCloudDaemonSetName,
			dialerCloudListenPort:    dialerListenPort,
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to create controller: %v\n", err)
		os.Exit(1)
	}

	if joinEnabled {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to create clientset for join reconciler: %v\n", err)
			os.Exit(1)
		}
		var sshKeys []string
		if joinSSHAuthorizedKeys != "" {
			for _, k := range strings.Split(joinSSHAuthorizedKeys, ",") {
				if k = strings.TrimSpace(k); k != "" {
					sshKeys = append(sshKeys, k)
				}
			}
		}
		joinReconciler := &join.Reconciler{
			Client:         mgr.GetClient(),
			Reader:         mgr.GetAPIReader(),
			Join:           &joink0s.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL},
			InfraProviders: []join.InfraProvider{joinaws.Provider{}},

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

			BootstrapSecretNameFormat: bootstrapSecretNameFormat,
		}

		joinMachine := &unstructured.Unstructured{}
		joinMachine.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
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
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "problem running manager: %v\n", err)
		os.Exit(1)
	}
}
