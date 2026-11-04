package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
)

// ensureDialerDaemonSet creates or updates the on-prem dialer
// DaemonSet directly: no CRD, no gitops YAML for its pod spec. Its
// desired state is computed entirely from this operator's own
// constants/flags plus the Secret it manages, so it cannot drift out
// of sync with what this operator expects. A hand-authored DaemonSet's
// AllowedIPs and taint values have no such check.
//
// Scheduling: Linux nodes matching the tunnelEndpoints selector,
// with control-plane nodes excluded by nodeAffinity (see
// controlPlaneLabel) and no toleration for the cloud-worker taint (so
// it never lands on the remote node it dials).
func (r *meshReconciler) ensureDialerDaemonSet(ctx context.Context) error {
	// A selected or retained control-plane endpoint needs a NoSchedule
	// toleration. Affinity determines placement; toleration permits that
	// placement and must remain present throughout endpoint retention.
	tolerations := dialerTolerations()

	hostPathDirectoryOrCreate := corev1.HostPathDirectoryOrCreate
	hostPathDirectory := corev1.HostPathDirectory
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            r.dialerDaemonSetName,
			Namespace:       r.secretNamespace,
			OwnerReferences: r.owners(),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": r.dialerDaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": r.dialerDaemonSetName}},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: r.dialerServiceAccount,
					Tolerations:        tolerations,
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: r.siteDialerNodeAffinity(),
						},
					},
					ImagePullSecrets: imagePullSecrets(r.dialerImagePullSecret),
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           r.dialerImage,
							ImagePullPolicy: effectiveDialerPullPolicy(r.dialerImagePullPolicy),
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
							},
							Env: []corev1.EnvVar{
								{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
								{Name: "NODE_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
								// Every family's address, for a speaker that
								// names this node as the next hop of routes in
								// each family.
								{Name: "NODE_IPS", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIPs"}}},
							},
							Args: []string{
								fmt.Sprintf("--secret-namespace=%s", r.secretNamespace),
								fmt.Sprintf("--secret-name=%s", r.secretName),
								fmt.Sprintf("--iface=%s", r.ifaceName),
								fmt.Sprintf("--private-key-file=%s/private.key", r.dialerPrivateKeyDir),
								fmt.Sprintf("--transit-masquerade-source=%s", r.tunnelSubnet),
								// Advertising the remote nodes to the rest of
								// the site. The next hop is this node itself,
								// which is where their traffic has to arrive.
								fmt.Sprintf("--transit-bgp-port=%d", r.transitBGPPort),
								fmt.Sprintf("--transit-bgp-asn=%d", r.transitBGPASN),
								"--transit-bgp-next-hop=$(NODE_IPS)",
								"--keepalive-seconds=15",
								// No --mtu: it is derived from the interface the
								// encapsulated packets leave by, so a number written
								// here would override that with a guess about a
								// network this does not run on.
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "dialer-key", MountPath: r.dialerPrivateKeyDir},
								// The node's real sysctls. A container runtime mounts
								// /proc/sys read-only and NET_ADMIN does not change
								// that, so forwarding and reverse path filtering could
								// be read but never set.
								{Name: "sysctl-net", MountPath: tunnel.HostSysctlNet},
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
						{
							Name: "sysctl-net",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/proc/sys/net", Type: &hostPathDirectory},
							},
						},
					},
				},
			},
		},
	}

	configureReceiveSysctlPolicy(desired)
	r.configureReceiptAgent(desired)
	existing := &appsv1.DaemonSet{}
	err := r.reader.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.pinSiteDialerAdmission(ctx, desired, nil); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting existing daemonset %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	if err := r.pinSiteDialerAdmission(ctx, desired, existing); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

// retainedEndpoints lists the nodes the peer Secret still publishes a
// tunnel address for. During a migration that is the departing node as
// well as the arriving one, which is the whole point: the departing
// node's dialer has to outlive the selector change that names its
// replacement, because the replacement's name travels over it.
//
// No Secret yet means no mesh yet, which is not an error: the first
// reconcile creates both the Secret and this DaemonSet.
func (r *meshReconciler) retainedEndpoints(ctx context.Context) ([]string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting secret %s to find retained endpoints: %w", key, err)
	}
	var names []string
	for k, v := range secret.Data {
		if !strings.HasPrefix(k, tunnel.NodeTunnelAddressPrefix) {
			continue
		}
		if strings.TrimSpace(string(v)) == "" {
			continue
		}
		names = append(names, strings.TrimPrefix(k, tunnel.NodeTunnelAddressPrefix))
	}
	sort.Strings(names)
	return names, nil
}

// dialerNodeAffinity is where the on-prem dialer is allowed to run:
// every Linux node at the site. Not only the selected endpoints,
// because the dialer has a job on every node. On an endpoint it
// terminates the tunnel; on a node with no tunnel it installs the
// transit that reaches the remotes through the relay, derived from the
// same data the render uses.
//
// Placement no longer appears here at all, and that is load bearing
// twice over. Which nodes hold tunnels is decided by the Secret the
// controller writes, and the dialer reads it every pass, so moving a
// tunnel changes data rather than pod scheduling: no dialer restarts,
// no speaker or session teardown, no window in which the node that
// must hand over is the node whose pod was just killed. And a node the
// selector let go of keeps its dialer for the retention window and
// after it, so the teardown of its interface is done by the same
// process that built it, rather than by a shutdown hook racing the
// scheduler.
//
// A node this operator provisioned is on the far side of a tunnel and
// never one of the site's ends of it, and Windows terminates nothing.
func dialerNodeAffinity() *corev1.NodeSelector {
	return &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
			{Key: cloudWorkerRoleLabel, Operator: corev1.NodeSelectorOpNotIn, Values: []string{cloudWorkerRoleValue}},
		},
	}}}
}

// parseSelectorRequirements turns a plain "k=v,k2=v2" selector string
// into node-affinity requirements. Only equality terms are supported
// (that is all a node placement selector needs here); anything else
// is ignored rather than silently mis-scheduling.
func parseSelectorRequirements(raw string) []corev1.NodeSelectorRequirement {
	// "all" is this project's own word for every node, not a selector.
	// A label parser reads it as "the label all must exist", which no
	// node carries, so the DaemonSet would be scheduled nowhere and
	// every node would look like it had simply declined to publish.
	if isAllNodes(raw) {
		return nil
	}
	// The same parser Kubernetes uses for a label selector, rather than
	// splitting on commas: a set based term ("k in (a,b)") contains
	// commas of its own, so splitting turns one requirement into two
	// fragments that match nothing. Silently, which put a dialer on
	// every node instead of the two the selector named.
	selector, err := labels.Parse(raw)
	if err != nil {
		return nil
	}
	requirements, _ := selector.Requirements()
	var out []corev1.NodeSelectorRequirement
	for _, req := range requirements {
		var op corev1.NodeSelectorOperator
		switch req.Operator() {
		case selection.Equals, selection.DoubleEquals, selection.In:
			op = corev1.NodeSelectorOpIn
		case selection.NotEquals, selection.NotIn:
			op = corev1.NodeSelectorOpNotIn
		case selection.Exists:
			op = corev1.NodeSelectorOpExists
		case selection.DoesNotExist:
			op = corev1.NodeSelectorOpDoesNotExist
		default:
			continue
		}
		values := req.Values().List()
		// A node affinity term with a value and no operator to use it
		// would match everything, so an empty set is only valid for the
		// operators that take none.
		if len(values) == 0 && op != corev1.NodeSelectorOpExists && op != corev1.NodeSelectorOpDoesNotExist {
			continue
		}
		out = append(out, corev1.NodeSelectorRequirement{Key: req.Key(), Operator: op, Values: values})
	}
	return out
}

// ensureCloudDialerDaemonSet creates or updates the remote-side dialer
// DaemonSet: the same binary, scheduled onto the cloud-worker node(s)
// only (nodeSelector + toleration for the cloud-worker taint, the
// inverse of the on-prem DaemonSet's scheduling). It keeps its
// identity from the /etc/wg-dialer/peers.json cloud-init wrote
// (hostPath, read-only, so the private key does not travel through the
// API) but takes its peer list from the per-machine adoption Secret
// this operator re-renders every reconcile, so post-join corrections
// reach a node whose userdata is immutable.
//
// It does not disable the wg-dialer.service systemd unit cloud-init
// installed: both converge on the same kernel interface
// (ConfigureDevice is idempotent; nothing calls LinkDel), and if this
// pod could not schedule, a disabled bootstrap tunnel would leave the
// node with no path back to the API. What the DaemonSet adds is a
// Kubernetes-native upgrade path (bump --dialer-image, rolling update)
// instead of host binary swaps.
// stopBeingRelayed takes a node's addresses and blocks off the site
// entries, which is where they live while some other endpoint relays to
// it.
//
// The counterpart of stopOwningAsEndpoint, and required for the same
// reason: a node that leaves the selector hands its prefixes to the site
// entries, and a node that returns takes them back. Doing only the first
// half leaves a returning endpoint owning its block twice, once on its
// own peer entry and once on whichever endpoint relays the site, and the
// accept list resolves that by keeping whichever was written last.
// takeBackFromRelay hands a returning endpoint its own prefixes back,
// and only once it is a peer as published.
//
// Selection makes this operator allocate a tunnel address; it is the
// node's own dialer that publishes the key, and RemotePeers renders a
// node only when both are there. Dropping the relayed entry at
// selection left the node's address and pod block owned by nobody until
// that key landed, which is however long the dialer takes to schedule
// and come up. A remote reading the list in that window prunes the
// route and cannot read the correction, because the site is reachable
// only over what it just pruned.
func takeBackFromRelay(data map[string][]byte, name string) bool {
	if !publishedEndpoint(data, name) {
		return false
	}
	return stopBeingRelayed(data, name)
}

func stopBeingRelayed(data map[string][]byte, name string) bool {
	changed := false
	for _, key := range []string{
		tunnel.SiteAddressesPrefix + name,
		tunnel.SitePodCIDRsPrefix + name,
	} {
		if _, ok := data[key]; ok {
			delete(data, key)
			changed = true
		}
	}
	return changed
}

// stopOwningAsEndpoint takes a departing endpoint's node addresses and
// pod blocks off its own peer entry, leaving its key and tunnel address
// so the tunnel it still holds goes on working.
//
// Those prefixes are published as site entries in the same pass, and a
// prefix must have exactly one owner: WireGuard's accept list is a trie,
// so a range named on two peers belongs to whichever was written last
// and traffic for it follows whichever that happened to be.
func stopOwningAsEndpoint(data map[string][]byte, name string) bool {
	changed := false
	for _, key := range []string{
		tunnel.NodeAddressesPrefix + name,
		tunnel.NodePodCIDRsPrefix + name,
	} {
		if _, ok := data[key]; ok {
			delete(data, key)
			changed = true
		}
	}
	return changed
}

// dialerTolerations lets a dialer run on a control plane. See the
// caller for why this is unconditional.
func dialerTolerations() []corev1.Toleration {
	var tolerations []corev1.Toleration
	for _, key := range []string{controlPlaneLabel, "node-role.kubernetes.io/master"} {
		tolerations = append(tolerations, corev1.Toleration{
			Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	return tolerations
}

// imagePullSecrets is the pull secret list for a dialer pod, which is
// empty when no secret is configured rather than holding a reference to
// nothing.
//
// A LocalObjectReference with an empty name is accepted by the API and
// then breaks every strategic merge patch against the object, because
// name is the merge key for this list and the element has none:
// "does not contain declared merge key: name". kubectl rollout restart,
// kubectl set image and kubectl apply all fail against a DaemonSet
// carrying one, so an operator cannot restart their own dialers, on a
// cluster where nothing is visibly wrong.
func imagePullSecrets(names string) []corev1.LocalObjectReference {
	var refs []corev1.LocalObjectReference
	seen := make(map[string]bool)
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			refs = append(refs, corev1.LocalObjectReference{Name: name})
			seen[name] = true
		}
	}
	return refs
}

func (r *meshReconciler) ensureCloudDialerDaemonSet(ctx context.Context) error {
	hostPathDirectory := corev1.HostPathDirectory
	// Default: the project image, which also self-installs onto the
	// host (the upgrade channel). When the image is not pullable on a
	// remote node, a public base image runs the host binary instead;
	// adoption still works, but it stops being an upgrade channel.
	cloudImage := r.dialerImage
	var cloudCommand []string
	if r.dialerCloudImage != "" {
		cloudImage = r.dialerCloudImage
		cloudCommand = []string{r.dialerCloudHostBinary}
	}
	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            r.dialerCloudDaemonSetName,
			Namespace:       r.secretNamespace,
			OwnerReferences: r.owners(),
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
					ImagePullSecrets: imagePullSecrets(r.dialerImagePullSecret),
					Containers: []corev1.Container{
						{
							Name:            "dialer",
							Image:           cloudImage,
							ImagePullPolicy: effectiveDialerPullPolicy(r.dialerImagePullPolicy),
							Command:         cloudCommand,
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
								// machine's own name is node-local data
								// (cloud-init wrote it), not a per-node flag
								// baked into this template.
								"--machine-name-file=/etc/wg-dialer/machine-name",
								// The image becomes the upgrade channel once the
								// node has joined: this copy installs itself over
								// the host binary the bootstrap unit runs, so a
								// fleet upgrade is one digest bump in gitops and
								// the download URL only ever mattered at first
								// boot.
								fmt.Sprintf("--listen-port=%s", r.dialerCloudListenPort),
								"--keepalive-seconds=15",
								"--mtu=1420",
								"--poll-interval=30s",
							},
							VolumeMounts: []corev1.VolumeMount{
								// Writable for one file: the interface claim
								// that tells the node's cloud-init systemd
								// unit to stand off while this pod holds the
								// live peer list. The peers file and identity
								// here are still only ever read.
								{Name: "wg-dialer-config", MountPath: "/etc/wg-dialer"},
								{Name: "host-bin", MountPath: "/host-bin"},
								{Name: "sysctl-net", MountPath: tunnel.HostSysctlNet},
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
						{
							Name: "sysctl-net",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/proc/sys/net", Type: &hostPathDirectory},
							},
						},
					},
				},
			},
		},
	}
	// Self-install is the post-join upgrade channel, and applies only
	// when this DaemonSet runs the project image; a public base image
	// carries no binary of its own to install.
	if r.dialerCloudImage == "" {
		c := &desired.Spec.Template.Spec.Containers[0]
		c.Args = append(c.Args, "--install-host-binary=/host-bin/wg-dialer")
	}
	configureReceiveSysctlPolicy(desired)
	r.configureReceiptAgent(desired)

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
