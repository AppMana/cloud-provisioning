// Package site keeps the site's own pod network off nodes whose pods a
// provider-native network owns, and runs the site's Calico on tunnel-class
// cloud nodes in the form those nodes need (see calico_tunnel.go).
//
// Taints do not do this: a CNI DaemonSet tolerates every taint so that it
// reaches every node. Each one is given a required node-affinity term that
// excludes the native networks' CNILabel values instead. Nodes without the
// label, which is every site node and every tunnel-class cloud node, still
// match it.
package site

import (
	"context"
	"fmt"
	"net/netip"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// hostCNIDirs are where a pod network installs its plugin and configuration
// on a Linux node. A DaemonSet mounting one of them is a pod network (or a
// meta-plugin such as Multus that delegates to the site's).
var hostCNIDirs = []string{"/etc/cni/net.d", "/opt/cni/bin"}

// namedAgents are site network agents that mount no CNI directory by path:
// Windows Calico runs as a HostProcess pod over the whole host root.
var namedAgents = []string{"calico-node-windows"}

// Breaker bounds how often the exclusion is restored after another writer
// removes it, so two controllers never fight over a DaemonSet.
const (
	breakerLimit  = 3
	breakerWindow = 10 * time.Minute
)

var installationGVK = schema.GroupVersionKind{Group: "operator.tigera.io", Version: "v1", Kind: "Installation"}

// installationFields are the Installation overrides the Tigera operator
// renders each of its DaemonSets from.
var installationFields = map[string]string{
	"calico-node":         "calicoNodeDaemonSet",
	"calico-node-windows": "calicoNodeWindowsDaemonSet",
}

// PendingError lists the site network DaemonSets that can still schedule
// onto a native-network node.
type PendingError struct{ DaemonSets []string }

func (e *PendingError) Error() string {
	return "the site pod network can still run on native-network nodes: " + strings.Join(e.DaemonSets, ", ")
}

// UnsupportedError is a site network this product cannot keep off a node.
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string {
	return "native pod networks are unsupported here: " + e.Reason
}

// WriterConflictError is a DaemonSet whose exclusion another writer keeps
// removing.
type WriterConflictError struct{ DaemonSet string }

func (e *WriterConflictError) Error() string {
	return fmt.Sprintf("another writer removed the native-network exclusion from %s %d times within %s; restore it at its source (the installer that owns %[1]s)", e.DaemonSet, breakerLimit+1, breakerWindow)
}

// Excluder keeps every site network DaemonSet off native-network nodes.
type Excluder struct {
	Client client.Client
	Reader client.Reader
	// Native are the provider-native network names this cluster provisions.
	Native []string
	// Extra names site network DaemonSets the rules above do not recognise.
	Extra []client.ObjectKey
	// ProviderRanges are the ranges provider-native networks give pods; nil
	// means none.
	ProviderRanges func(context.Context) ([]netip.Prefix, error)
	Now            func() time.Time

	mu sync.Mutex
	// applied are DaemonSets this process has excluded; a later absence of
	// the exclusion means another writer removed it.
	applied   map[client.ObjectKey]bool
	reapplied map[client.ObjectKey][]time.Time
}

// Requirement is the node requirement every site network DaemonSet carries.
func Requirement(native []string) corev1.NodeSelectorRequirement {
	values := slices.Clone(native)
	sort.Strings(values)
	return corev1.NodeSelectorRequirement{Key: platform.CNILabel, Operator: corev1.NodeSelectorOpNotIn, Values: values}
}

// requirements are the node requirements one site network DaemonSet
// carries: off native-network nodes, and for the site's Calico, off every
// cloud worker, where the tunnel agent runs instead.
func (e *Excluder) requirements(ds *appsv1.DaemonSet) []corev1.NodeSelectorRequirement {
	var reqs []corev1.NodeSelectorRequirement
	if len(e.Native) > 0 {
		reqs = append(reqs, Requirement(e.Native))
	}
	if isCalicoAgent(ds) {
		reqs = append(reqs, cloudWorkers)
	}
	return reqs
}

// IsTarget reports whether a DaemonSet runs the site's pod network.
func (e *Excluder) IsTarget(ds *appsv1.DaemonSet) bool {
	if _, ok := ds.Labels[platform.ComponentLabel]; ok {
		return false
	}
	if slices.Contains(e.Extra, client.ObjectKeyFromObject(ds)) || slices.Contains(namedAgents, ds.Name) {
		return true
	}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.HostPath != nil && slices.Contains(hostCNIDirs, path.Clean(v.HostPath.Path)) {
			return true
		}
	}
	return false
}

// Targets lists the site network DaemonSets, sorted by namespace and name.
func (e *Excluder) Targets(ctx context.Context) ([]appsv1.DaemonSet, error) {
	list := &appsv1.DaemonSetList{}
	if err := e.Reader.List(ctx, list); err != nil {
		return nil, fmt.Errorf("listing DaemonSets: %w", err)
	}
	var targets []appsv1.DaemonSet
	for _, ds := range list.Items {
		if e.IsTarget(&ds) {
			targets = append(targets, ds)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Namespace != targets[j].Namespace {
			return targets[i].Namespace < targets[j].Namespace
		}
		return targets[i].Name < targets[j].Name
	})
	return targets, nil
}

// Verify reports whether a node of this class can join. A native-network
// node needs every site network DaemonSet to exclude it, on a site network
// that can be excluded. A tunnel-class node on a Calico site needs the
// product's Calico agent and BGP peering limited to site nodes.
func (e *Excluder) Verify(ctx context.Context, network cni.Network, class platform.Class) error {
	if !class.Native() {
		return e.verifyTunnel(ctx, network)
	}
	if network.Name == cni.OVNKubernetes {
		return &UnsupportedError{Reason: "OVN-Kubernetes is managed by the distribution's network operator, which places its node agent on every node"}
	}
	targets, err := e.Targets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return &UnsupportedError{Reason: fmt.Sprintf("no DaemonSet runs the %s pod network (%s), so it runs inside the node agent and cannot be kept off a node", network.Name, network.Detail)}
	}
	var pending []string
	for _, ds := range targets {
		key := client.ObjectKeyFromObject(&ds)
		if e.tripped(key) {
			return &WriterConflictError{DaemonSet: key.String()}
		}
		if !excludes(&ds.Spec.Template.Spec, e.requirements(&ds)) {
			pending = append(pending, key.String())
		}
	}
	if len(pending) > 0 {
		return &PendingError{DaemonSets: pending}
	}
	if network.Name == cni.Calico {
		return e.verifyProviderPools(ctx)
	}
	return nil
}

// verifyTunnel reports whether a tunnel-class node can join the site network.
func (e *Excluder) verifyTunnel(ctx context.Context, network cni.Network) error {
	if network.Name != cni.Calico {
		return nil
	}
	targets, err := e.Targets(ctx)
	if err != nil {
		return err
	}
	var agents []appsv1.DaemonSet
	for _, ds := range targets {
		if isCalicoAgent(&ds) {
			agents = append(agents, ds)
		}
	}
	if len(agents) == 0 {
		return &UnsupportedError{Reason: "the site runs Calico but no calico-node DaemonSet, so there is no agent to run on tunnel nodes with their stated addresses"}
	}
	for _, ds := range agents {
		key := client.ObjectKeyFromObject(&ds)
		if e.tripped(key) {
			return &WriterConflictError{DaemonSet: key.String()}
		}
		if !excludes(&ds.Spec.Template.Spec, e.requirements(&ds)) {
			return &PendingError{DaemonSets: []string{key.String()}}
		}
		if err := e.verifyTunnelAgent(ctx, &ds); err != nil {
			return err
		}
	}
	return e.verifyBGPScope(ctx)
}

// Ensure excludes native-network nodes from one site network DaemonSet and,
// for the site's Calico, cloud workers too, after which it brings up the
// tunnel agent, makes provider ranges cluster-internal to Calico and limits
// the BGP mesh to site nodes. The exclusion comes
// first so the two agents never run on one node.
func (e *Excluder) Ensure(ctx context.Context, ds *appsv1.DaemonSet) error {
	excluded, err := e.ensureExcluded(ctx, ds)
	if err != nil || !excluded || !isCalicoAgent(ds) {
		return err
	}
	if err := e.ensureTunnelAgent(ctx, ds); err != nil {
		return err
	}
	if err := e.ensureProviderPools(ctx); err != nil {
		return err
	}
	return e.ensureBGPScope(ctx)
}

// ensureExcluded reports whether the DaemonSet as it now runs carries the
// exclusion. Through an Installation that is only once the operator has
// rendered it, which brings this DaemonSet back to the reconciler.
func (e *Excluder) ensureExcluded(ctx context.Context, ds *appsv1.DaemonSet) (bool, error) {
	reqs := e.requirements(ds)
	if len(reqs) == 0 || excludes(&ds.Spec.Template.Spec, reqs) {
		return true, nil
	}
	key := client.ObjectKeyFromObject(ds)
	if !e.admit(key) {
		logf.FromContext(ctx).Info("not restoring the cloud-node exclusion: another writer keeps removing it", "daemonSet", key)
		return false, nil
	}
	rendered := true
	if owner := installationOwner(ds); owner != "" {
		if err := e.ensureInstallation(ctx, owner, ds, reqs); err != nil {
			return false, err
		}
		rendered = false
	} else {
		patched := ds.DeepCopy()
		exclude(&patched.Spec.Template.Spec, reqs)
		if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(ds, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("excluding cloud nodes from %s: %w", key, err)
		}
	}
	e.mu.Lock()
	if e.applied == nil {
		e.applied = map[client.ObjectKey]bool{}
	}
	e.applied[key] = true
	e.mu.Unlock()
	logf.FromContext(ctx).Info("excluded cloud nodes from the site pod network", "daemonSet", key, "requirements", reqs)
	return rendered, nil
}

// admit records a restore of an exclusion this process applied before, and
// refuses once the breaker has tripped within its window.
func (e *Excluder) admit(key client.ObjectKey) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.applied[key] {
		return true
	}
	now := e.Now()
	recent := e.recent(key, now)
	if len(recent) >= breakerLimit {
		e.reapplied[key] = append(recent, now)
		return false
	}
	if e.reapplied == nil {
		e.reapplied = map[client.ObjectKey][]time.Time{}
	}
	e.reapplied[key] = append(recent, now)
	return true
}

func (e *Excluder) tripped(key client.ObjectKey) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.recent(key, e.Now())) > breakerLimit
}

// recent are a DaemonSet's restores within the breaker window; callers hold mu.
func (e *Excluder) recent(key client.ObjectKey, now time.Time) []time.Time {
	var kept []time.Time
	for _, at := range e.reapplied[key] {
		if now.Sub(at) < breakerWindow {
			kept = append(kept, at)
		}
	}
	return kept
}

func installationOwner(ds *appsv1.DaemonSet) string {
	for _, ref := range ds.OwnerReferences {
		if ref.Kind == installationGVK.Kind && strings.HasPrefix(ref.APIVersion, installationGVK.Group+"/") {
			return ref.Name
		}
	}
	return ""
}

// ensureInstallation writes the exclusion into the Installation override the
// operator renders this DaemonSet from. The override's affinity replaces the
// operator's default, so it starts from what the operator rendered.
func (e *Excluder) ensureInstallation(ctx context.Context, name string, ds *appsv1.DaemonSet, reqs []corev1.NodeSelectorRequirement) error {
	field, ok := installationFields[ds.Name]
	if !ok {
		return fmt.Errorf("the Tigera operator renders %s from no Installation override this controller knows", client.ObjectKeyFromObject(ds))
	}
	installation := &unstructured.Unstructured{}
	installation.SetGroupVersionKind(installationGVK)
	if err := e.Reader.Get(ctx, client.ObjectKey{Name: name}, installation); err != nil {
		return fmt.Errorf("reading Installation %s: %w", name, err)
	}
	spec := ds.Spec.Template.Spec.DeepCopy()
	exclude(spec, reqs)
	affinity, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec.Affinity)
	if err != nil {
		return err
	}
	patched := installation.DeepCopy()
	if err := unstructured.SetNestedMap(patched.Object, affinity, "spec", field, "spec", "template", "spec", "affinity"); err != nil {
		return err
	}
	if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(installation, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("excluding native-network nodes through Installation %s: %w", name, err)
	}
	return nil
}

// excludes reports whether every required term carries every requirement.
func excludes(spec *corev1.PodSpec, reqs []corev1.NodeSelectorRequirement) bool {
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		for _, req := range reqs {
			if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
				return r.Key == req.Key && r.Operator == req.Operator && covers(r.Values, req.Values)
			}) {
				return false
			}
		}
	}
	return true
}

func covers(have, need []string) bool {
	for _, v := range need {
		if !slices.Contains(have, v) {
			return false
		}
	}
	return true
}

// exclude ANDs each requirement into every required term, replacing any
// earlier exclusion on the same key; with no required terms it adds one.
func exclude(spec *corev1.PodSpec, reqs []corev1.NodeSelectorRequirement) {
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: slices.Clone(reqs)}},
		}
		return
	}
	for i := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[i]
		for _, req := range reqs {
			term.MatchExpressions = slices.DeleteFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
				return r.Key == req.Key && r.Operator == corev1.NodeSelectorOpNotIn
			})
			term.MatchExpressions = append(term.MatchExpressions, req)
		}
	}
}
