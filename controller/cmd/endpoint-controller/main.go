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

// dialerAllowedIPs is deliberately maximally permissive (route/accept
// everything through the tunnel), not narrowed to the peer's known
// addresses or its dynamically Calico-assigned pod CIDR: WireGuard's
// real security boundary is the handshake (only a peer holding the
// correct private key can ever establish the tunnel at all), and this
// is a single-peer-per-node tunnel, so there's no ambiguity about
// which peer traffic should route through. Trying to narrow this to
// an exact CIDR set added real complexity (dynamically discovering a
// CNI's pod-CIDR allocation) for no actual security benefit.
const dialerAllowedIPs = "0.0.0.0/0,::/0"

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

	if string(secret.Data[r.secretKey]) != endpoint {
		patch := client.MergeFrom(secret.DeepCopy())
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[r.secretKey] = []byte(endpoint)
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
							Name:  "dialer",
							Image: r.dialerImage,
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
								fmt.Sprintf("--allowed-ips=%s", dialerAllowedIPs),
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
	)
	flag.StringVar(&machineSelector, "machine-selector", fmt.Sprintf("%s=%s", cloudWorkerRoleLabel, cloudWorkerRoleValue),
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key", "peer-endpoint", "key within the Secret to write the endpoint into")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")
	flag.StringVar(&dialerDaemonSetName, "dialer-daemonset-name", "wg-dialer", "name of the on-prem dialer DaemonSet this operator provisions directly")
	flag.StringVar(&dialerServiceAccount, "dialer-service-account", "wg-dialer", "ServiceAccount the dialer DaemonSet's pods run as")
	flag.StringVar(&dialerImage, "dialer-image", "ghcr.io/appmana/cloud-provisioning-dialer:e1f8655", "image for the dialer DaemonSet's container")
	flag.StringVar(&dialerImagePullSecret, "dialer-image-pull-secret", "ghcr-pull", "imagePullSecret for the dialer DaemonSet")

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
