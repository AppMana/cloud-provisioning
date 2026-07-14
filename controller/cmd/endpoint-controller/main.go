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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

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
	)
	flag.StringVar(&machineSelector, "machine-selector", "cloud-provisioning.appmana.com/role=cloud-worker",
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key", "peer-endpoint", "key within the Secret to write the endpoint into")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "optional: namespace of a Gateway to annotate with the node's external IP for external-dns (blank disables this)")
	flag.StringVar(&gatewayName, "gateway-name", "", "optional: name of a Gateway to annotate with the node's external IP for external-dns")

	flag.BoolVar(&joinEnabled, "join-enabled", false, "enable the bootstrap-secret provisioning reconciler (join.Reconciler)")
	flag.StringVar(&joinTemplatePath, "join-template-path", "/join-patterns/k0s-worker.cloud-config.tmpl", "path to the join-pattern template to render")
	flag.StringVar(&joinAPIAddress, "join-api-address", "https://10.101.0.1:6443", "cluster API server address used to mint k0s join tokens (bracket IPv6 literals, e.g. https://[fd8f:cf26:522a::1]:6443)")
	flag.StringVar(&joinAPIVIP, "join-api-vip", "10.101.0.1", "cluster API VIP the new node must reach through the tunnel before joining")
	flag.StringVar(&joinKubeletExtraArgs, "join-kubelet-extra-args", "--node-labels=cloud-provisioning.appmana.com/role=cloud-worker --register-with-taints=cloud-provisioning.appmana.com/role=cloud-worker:NoSchedule", "extra kubelet args applied to every joining cloud-worker node, matching the existing taint/label convention")
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
			Client:           mgr.GetClient(),
			reader:           mgr.GetAPIReader(),
			secretNamespace:  secretNamespace,
			secretName:       secretName,
			secretKey:        secretKey,
			port:             port,
			gatewayNamespace: gatewayNamespace,
			gatewayName:      gatewayName,
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
			Client: mgr.GetClient(),
			Reader: mgr.GetAPIReader(),
			Join:   &joink0s.Provider{Client: clientset, APIAddress: joinAPIAddress, TTL: joinTokenTTL},
			Infra:  joinaws.Provider{},

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
