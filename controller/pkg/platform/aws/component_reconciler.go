package aws

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/discover"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// FieldOwner is the field manager of everything the product applies.
const FieldOwner = "cloud-provisioning"

// awsNode is the VPC CNI DaemonSet; amazon-vpc-cni its ConfigMap.
// windowsCNI is the Windows nodes' plugin initializer; windowsIPAM the
// VPC resource controller.
var (
	awsNode     = types.NamespacedName{Namespace: "kube-system", Name: "aws-node"}
	windowsCNI  = types.NamespacedName{Namespace: "kube-system", Name: windowsCNIName}
	windowsIPAM = types.NamespacedName{Namespace: "kube-system", Name: "vpc-resource-local-controller"}
)

// factsRecheck is how often the site's ranges are read again: a Service or
// pod range change raises no event this controller watches.
const factsRecheck = 10 * time.Minute

// VPCCNIComponent keeps aws-node applied with the site's current facts.
type VPCCNIComponent struct {
	// Client reads and applies without a cache: discovering the Service
	// range makes a dry-run request.
	Client           client.Client
	Family           string
	Image, InitImage string
	// WindowsIPAMImage, the patched VPC resource controller, turns on
	// Windows VPC CNI nodes: their plugin's configuration and Windows IPAM.
	WindowsIPAMImage       string
	WindowsIPAMPullSecrets []string
}

// windows reports whether Windows VPC CNI nodes are configured.
func (c *VPCCNIComponent) windows() bool { return c.WindowsIPAMImage != "" }

// Facts reads what aws-node depends on from the cluster.
func (c *VPCCNIComponent) Facts(ctx context.Context) (VPCCNIFacts, error) {
	var ranges []netip.Prefix
	add := func(raw string) error {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		if !slices.Contains(ranges, p.Masked()) {
			ranges = append(ranges, p.Masked())
		}
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := c.Client.List(ctx, nodes); err != nil {
		return VPCCNIFacts{}, err
	}
	pods, err := sitePodRanges(ctx, c.Client, nodes.Items)
	if err != nil {
		return VPCCNIFacts{}, fmt.Errorf("reading the site's pod ranges: %w", err)
	}
	services, err := discover.ServiceCIDRs(ctx, c.Client)
	if err != nil {
		return VPCCNIFacts{}, fmt.Errorf("reading the Service ranges: %w", err)
	}
	for _, raw := range append(pods, services...) {
		if err := add(raw); err != nil {
			return VPCCNIFacts{}, err
		}
	}
	// Each site node's own address: the site's node network is not
	// recorded anywhere, and its addresses are what replies return to.
	for _, node := range nodes.Items {
		if _, cloud := node.Labels[platform.RoleLabel]; cloud {
			continue
		}
		for _, a := range node.Status.Addresses {
			if a.Type != corev1.NodeInternalIP {
				continue
			}
			addr, err := netip.ParseAddr(a.Address)
			if err != nil {
				return VPCCNIFacts{}, fmt.Errorf("node %s InternalIP %q: %w", node.Name, a.Address, err)
			}
			if covered(ranges, addr) {
				continue
			}
			if err := add(netip.PrefixFrom(addr, addr.BitLen()).String()); err != nil {
				return VPCCNIFacts{}, err
			}
		}
	}
	mtu, err := podMTU(ctx, c.Client)
	if err != nil {
		return VPCCNIFacts{}, err
	}
	return VPCCNIFacts{Family: c.Family, SiteRanges: ranges, PodMTU: mtu, Image: c.Image, InitImage: c.InitImage}, nil
}

// podMTU is the tunnel's MTU, or the site's pod MTU where the site has set
// a smaller budget for its paths: an AWS pod's packets cross the same
// tunnels to the same pods.
func podMTU(ctx context.Context, c client.Client) (int, error) {
	list := &appsv1.DaemonSetList{}
	if err := c.List(ctx, list); err != nil {
		return 0, err
	}
	mtu := tunneldevice.DefaultTransportMTU
	for i := range list.Items {
		ds := &list.Items[i]
		if ds.Name != "calico-node" {
			continue
		}
		site, _, err := cni.CalicoPodMTU(ctx, c, ds)
		if err != nil {
			return 0, fmt.Errorf("reading the site's pod MTU: %w", err)
		}
		// Zero is Calico's automatic MTU, which states no budget.
		if site >= 1280 && site < mtu {
			mtu = site
		}
	}
	return mtu, nil
}

var (
	calicoPoolList = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList"}
	ciliumNodeList = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNodeList"}
)

// sitePodRanges are the ranges site pods take addresses from, read where
// the site's network records them exactly: Calico's enabled IP pools,
// Cilium's per-node ranges, or else each site node's own pod ranges.
func sitePodRanges(ctx context.Context, c client.Reader, nodes []corev1.Node) ([]string, error) {
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(calicoPoolList)
	if err := c.List(ctx, pools); err != nil && !meta.IsNoMatchError(err) {
		return nil, err
	}
	var out []string
	for _, pool := range pools.Items {
		if disabled, _, _ := unstructured.NestedBool(pool.Object, "spec", "disabled"); disabled {
			continue
		}
		if cidr, _, _ := unstructured.NestedString(pool.Object, "spec", "cidr"); cidr != "" {
			out = append(out, cidr)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	site := map[string]bool{}
	for _, node := range nodes {
		if _, cloud := node.Labels[platform.RoleLabel]; !cloud {
			site[node.Name] = true
		}
	}
	cilium := &unstructured.UnstructuredList{}
	cilium.SetGroupVersionKind(ciliumNodeList)
	if err := c.List(ctx, cilium); err != nil && !meta.IsNoMatchError(err) {
		return nil, err
	}
	for _, n := range cilium.Items {
		if !site[n.GetName()] {
			continue
		}
		cidrs, _, _ := unstructured.NestedStringSlice(n.Object, "spec", "ipam", "podCIDRs")
		out = append(out, cidrs...)
	}
	if len(out) > 0 {
		return out, nil
	}
	for _, node := range nodes {
		if !site[node.Name] {
			continue
		}
		cidrs := node.Spec.PodCIDRs
		if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
			cidrs = []string{node.Spec.PodCIDR}
		}
		out = append(out, cidrs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Calico pool, Cilium node range or site node pod range found")
	}
	return out, nil
}

func covered(ranges []netip.Prefix, addr netip.Addr) bool {
	for _, p := range ranges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// WindowsFacts reads what Windows nodes' plugin configuration depends on,
// beyond the facts aws-node shares.
func (c *VPCCNIComponent) WindowsFacts(ctx context.Context, linux VPCCNIFacts) (WindowsCNIFacts, error) {
	f := WindowsCNIFacts{SiteRanges: linux.SiteRanges, PodMTU: linux.PodMTU}
	services, err := discover.ServiceCIDRs(ctx, c.Client)
	if err != nil {
		return f, fmt.Errorf("reading the Service ranges: %w", err)
	}
	for _, raw := range services {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return f, err
		}
		f.ServiceRanges = append(f.ServiceRanges, p.Masked())
	}
	dns, err := discover.ClusterDNS(ctx, c.Client)
	if err != nil {
		return f, err
	}
	for _, raw := range dns {
		a, err := netip.ParseAddr(raw)
		if err != nil {
			return f, err
		}
		f.ClusterDNS = append(f.ClusterDNS, a)
	}
	if f.ClusterDomain, err = discover.ClusterDomain(ctx, c.Client); err != nil {
		return f, err
	}
	return f, nil
}

// WindowsIPAMFacts reads the AWS cluster Windows IPAM manages: the one CAPI
// Cluster on an AWSCluster, and that AWSCluster's VPC.
func (c *VPCCNIComponent) WindowsIPAMFacts(ctx context.Context) (WindowsIPAMFacts, error) {
	f := WindowsIPAMFacts{Image: c.WindowsIPAMImage, PullSecrets: c.WindowsIPAMPullSecrets}
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind(clusterGVK.Kind + "List"))
	if err := c.Client.List(ctx, clusters); err != nil {
		return f, fmt.Errorf("listing Clusters: %w", err)
	}
	var aws []unstructured.Unstructured
	for _, cluster := range clusters.Items {
		if kind, _, _ := unstructured.NestedString(cluster.Object, "spec", "infrastructureRef", "kind"); kind == awsClusterGVK.Kind {
			aws = append(aws, cluster)
		}
	}
	if len(aws) != 1 {
		return f, fmt.Errorf("Windows IPAM manages one AWS cluster's VPC; there are %d Clusters on AWSClusters", len(aws))
	}
	name, _, _ := unstructured.NestedString(aws[0].Object, "spec", "infrastructureRef", "name")
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(awsClusterGVK)
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: aws[0].GetNamespace(), Name: name}, infra); err != nil {
		return f, fmt.Errorf("reading AWSCluster %s: %w", name, err)
	}
	_, vpc, _, err := awsClusterNetwork(infra)
	if err != nil {
		return f, err
	}
	f.ClusterName, f.VPCID = aws[0].GetName(), vpc
	return f, nil
}

// Ensure applies aws-node and its ConfigMap for the current facts and, for
// Windows VPC CNI nodes, their plugin's initializer and Windows IPAM. Windows
// facts that cannot be read leave Linux nodes' network applied.
func (c *VPCCNIComponent) Ensure(ctx context.Context) error {
	facts, err := c.Facts(ctx)
	if err != nil {
		return err
	}
	var windows []client.Object
	var windowsErr error
	if c.windows() {
		windows, facts.WindowsIPAM, windowsErr = c.windowsObjects(ctx, facts)
	}
	configMap, ds, err := VPCCNIObjects(facts)
	if err != nil {
		return err
	}
	for _, obj := range append([]client.Object{configMap, ds}, windows...) {
		if err := apply(ctx, c.Client, obj); err != nil {
			return fmt.Errorf("applying %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, client.ObjectKeyFromObject(obj), err)
		}
	}
	return windowsErr
}

// windowsObjects are the Windows nodes' initializer and Windows IPAM, and
// whether IPAM's controller is available to enable.
func (c *VPCCNIComponent) windowsObjects(ctx context.Context, linux VPCCNIFacts) ([]client.Object, bool, error) {
	cniFacts, err := c.WindowsFacts(ctx, linux)
	if err != nil {
		return nil, false, fmt.Errorf("Windows VPC CNI: %w", err)
	}
	scripts, settings, ds, err := WindowsCNIObjects(cniFacts)
	if err != nil {
		return nil, false, fmt.Errorf("Windows VPC CNI: %w", err)
	}
	objects := []client.Object{scripts, settings, ds}
	ipamFacts, err := c.WindowsIPAMFacts(ctx)
	if err != nil {
		return objects, false, fmt.Errorf("Windows IPAM: %w", err)
	}
	deployment, err := WindowsIPAMDeployment(ipamFacts)
	if err != nil {
		return objects, false, fmt.Errorf("Windows IPAM: %w", err)
	}
	current := &appsv1.Deployment{}
	if err := c.Client.Get(ctx, windowsIPAM, current); client.IgnoreNotFound(err) != nil {
		return objects, false, err
	}
	return append(objects, deployment), current.Status.AvailableReplicas > 0, nil
}

// apply server-side applies a typed object as the product's field manager,
// taking over fields another manager (such as a hand install) set.
func apply(ctx context.Context, c client.Client, obj client.Object) error {
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return err
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(gvk)
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// Reconcile applies aws-node on any change to it or to the site's nodes.
func (c *VPCCNIComponent) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := c.Ensure(ctx); err != nil {
		return ctrl.Result{}, err
	}
	// Windows IPAM is enabled once its controller is available, which
	// raises no event this controller watches.
	if c.windows() {
		current := &appsv1.Deployment{}
		if err := c.Client.Get(ctx, windowsIPAM, current); err == nil && current.Status.AvailableReplicas == 0 {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}
	logf.FromContext(ctx).V(1).Info("applied the AWS VPC CNI", "daemonSet", awsNode)
	return ctrl.Result{RequeueAfter: factsRecheck}, nil
}

// Register runs the component: on aws-node's own changes, and on nodes
// joining, leaving or changing address.
func (c *VPCCNIComponent) Register(mgr ctrl.Manager) error {
	toAWSNode := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: awsNode}}
	})
	addresses := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, okOld := e.ObjectOld.(*corev1.Node)
			now, okNew := e.ObjectNew.(*corev1.Node)
			return !okOld || !okNew || old.Labels[platform.RoleLabel] != now.Labels[platform.RoleLabel] ||
				!slices.Equal(old.Status.Addresses, now.Status.Addresses) || !slices.Equal(old.Spec.PodCIDRs, now.Spec.PodCIDRs)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("aws-vpc-cni").
		For(&appsv1.DaemonSet{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			key := client.ObjectKeyFromObject(o)
			return key == awsNode || (c.windows() && key == windowsCNI)
		}))).
		Watches(&corev1.Node{}, toAWSNode, builder.WithPredicates(addresses)).
		Complete(c)
}
