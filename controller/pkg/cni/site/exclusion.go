// Package site keeps the site's own pod network off nodes whose pods a
// provider-native network owns.
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
	Now   func() time.Time

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

// Verify reports whether a native-network node can join: every site network
// DaemonSet excludes it, and the site network is one that can be excluded.
func (e *Excluder) Verify(ctx context.Context, network cni.Network) error {
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
	req := Requirement(e.Native)
	var pending []string
	for _, ds := range targets {
		key := client.ObjectKeyFromObject(&ds)
		if e.tripped(key) {
			return &WriterConflictError{DaemonSet: key.String()}
		}
		if !excludes(&ds.Spec.Template.Spec, req) {
			pending = append(pending, key.String())
		}
	}
	if len(pending) > 0 {
		return &PendingError{DaemonSets: pending}
	}
	return nil
}

// Ensure excludes native-network nodes from one site network DaemonSet.
func (e *Excluder) Ensure(ctx context.Context, ds *appsv1.DaemonSet) error {
	req := Requirement(e.Native)
	if len(req.Values) == 0 || excludes(&ds.Spec.Template.Spec, req) {
		return nil
	}
	key := client.ObjectKeyFromObject(ds)
	if !e.admit(key) {
		logf.FromContext(ctx).Info("not restoring the native-network exclusion: another writer keeps removing it", "daemonSet", key)
		return nil
	}
	if owner := installationOwner(ds); owner != "" {
		if err := e.ensureInstallation(ctx, owner, ds, req); err != nil {
			return err
		}
	} else {
		patched := ds.DeepCopy()
		exclude(&patched.Spec.Template.Spec, req)
		if err := e.Client.Patch(ctx, patched, client.MergeFromWithOptions(ds, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("excluding native-network nodes from %s: %w", key, err)
		}
	}
	e.mu.Lock()
	if e.applied == nil {
		e.applied = map[client.ObjectKey]bool{}
	}
	e.applied[key] = true
	e.mu.Unlock()
	logf.FromContext(ctx).Info("excluded native-network nodes from the site pod network", "daemonSet", key, "requirement", req)
	return nil
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
func (e *Excluder) ensureInstallation(ctx context.Context, name string, ds *appsv1.DaemonSet, req corev1.NodeSelectorRequirement) error {
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
	exclude(spec, req)
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

// excludes reports whether every required term carries req.
func excludes(spec *corev1.PodSpec, req corev1.NodeSelectorRequirement) bool {
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !slices.ContainsFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
			return r.Key == req.Key && r.Operator == req.Operator && covers(r.Values, req.Values)
		}) {
			return false
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

// exclude ANDs req into every required term, replacing any earlier
// exclusion on the same key; with no required terms it adds one.
func exclude(spec *corev1.PodSpec, req corev1.NodeSelectorRequirement) {
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{req}}},
		}
		return
	}
	for i := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[i]
		term.MatchExpressions = slices.DeleteFunc(term.MatchExpressions, func(r corev1.NodeSelectorRequirement) bool {
			return r.Key == req.Key && r.Operator == corev1.NodeSelectorOpNotIn
		})
		term.MatchExpressions = append(term.MatchExpressions, req)
	}
}
