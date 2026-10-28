// Package guard refuses harness writes that would do the product's work.
//
// The harness may build the lab, give the product what an operator gives
// it, inject faults and observe. Everything a real cluster needs beyond
// that is the product's, and a harness that writes it makes a test pass
// on the harness rather than the product. Once a site is built, every
// kubectl the harness runs passes through Kubectl, which allows only the
// writes listed here and refuses the rest.
package guard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Write is one object a kubectl invocation would change.
type Write struct {
	Verb        string
	Resource    string // lowercase plural, e.g. "secrets"
	Namespace   string // "" for cluster-scoped
	Name        string
	Subresource string
}

func (w Write) String() string {
	where := w.Namespace
	if where == "" {
		where = "(cluster)"
	}
	s := fmt.Sprintf("%s %s %s/%s", w.Verb, w.Resource, where, w.Name)
	if w.Subresource != "" {
		s += " [" + w.Subresource + "]"
	}
	return s
}

// writeVerbs are the kubectl commands that change the cluster.
var writeVerbs = []string{"apply", "create", "replace", "patch", "delete", "label", "annotate", "set", "scale",
	"edit", "taint", "cordon", "uncordon", "drain", "autoscale", "expose", "run", "rollout", "certificate"}

// readRollouts are rollout subcommands that only read.
var readRollouts = []string{"status", "history"}

// Protected namespaces hold the product, the site's own components and what
// an operator configures. Other namespaces are the harness's own workloads.
var protected = []string{"kube-system", "kube-public", "kube-node-lease", "default", "cloud-provisioning",
	"capa-system", "capi-system", "capi-operator-system", "calico-system", "tigera-operator", "cert-manager"}

// Allowed is one kind of write the harness may make in a protected
// namespace or at cluster scope, with why it is not product work.
type Allowed struct {
	Resource   string
	Namespaces []string // protected namespaces it is allowed in; nil means cluster-scoped
	Verbs      []string // nil means any write verb
	Names      []string // nil means any name; otherwise path.Match patterns
	Status     bool     // the status subresource may be written
	Why        string
}

// Policy is every allowed write outside the harness's own namespaces.
var Policy = []Allowed{
	{Resource: "namespaces", Why: "operator: the release and provider namespaces; harness: its own probe namespaces (deleting a protected one is refused)"},
	{Resource: "secrets", Namespaces: []string{"cloud-provisioning", "capa-system"}, Why: "operator: credentials and provider configuration; fault injection: removing a peer acknowledgement"},
	{Resource: "provisionednodeclaims", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create", "delete"}, Why: "operator: claiming and releasing a node"},
	{Resource: "provisionednodegroupclaims", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create", "patch", "delete"}, Why: "operator: claiming a node group and scaling it"},
	{Resource: "clusters", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create"}, Why: "operator: the CAPI Cluster for the existing site"},
	{Resource: "awsclusters", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create"}, Why: "operator: the externally managed AWSCluster"},
	{Resource: "awsmachinetemplates", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create"}, Why: "operator: worker templates"},
	{Resource: "importedcontrolplanes", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create"}, Why: "operator: presenting the site to CAPI"},
	{Resource: "labclusters", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create", "patch"}, Why: "operator: the lab's infrastructure cluster and its endpoint (its readiness is the product's)"},
	{Resource: "labmachinetemplates", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"apply", "create"}, Why: "operator: lab worker templates"},
	{Resource: "labmachines", Namespaces: []string{"cloud-provisioning"}, Status: true, Why: "lab: the lab is the infrastructure provider for its machines"},
	{Resource: "configmaps", Namespaces: []string{"cloud-provisioning"}, Verbs: []string{"create", "patch"}, Names: []string{"cldt-vm-slots"}, Why: "lab: the VM provider's slot pool"},
	{Resource: "awsclusterstaticidentities", Verbs: []string{"apply", "create"}, Why: "operator: CAPA's source credentials"},
	{Resource: "awsclusterroleidentities", Verbs: []string{"apply", "create"}, Why: "operator: CAPA's role"},
	{Resource: "secrets", Namespaces: []string{"kube-system"}, Verbs: []string{"apply"}, Names: []string{"*-ipam-pull", "*-calico-pull"}, Why: "operator: registry credentials for kube-system images (platforms.aws.vpcCNI.windows.pullSecrets, the site's Windows Calico)"},
	{Resource: "customresourcedefinitions", Verbs: []string{"apply", "create"}, Why: "lab: the lab infrastructure provider's own kinds"},
	{Resource: "clusterroles", Verbs: []string{"apply", "create"}, Names: []string{"capi-aggregate-labcontainers"}, Why: "lab: the lab infrastructure provider's CAPI aggregation role"},
}

// StandIns are writes the harness still makes for the product, each with
// the open tracker item that removes it. The list only shrinks.
var StandIns = []Allowed{
}

// Prerequisites are operator prerequisites the harness installs as pinned,
// rendered release manifests, admitted whole by the SHA-256 of what is
// applied: a manifest that differs in a byte is checked like any other.
var Prerequisites = map[string]string{
	"976a937f7348e6d360dc419995af68c1230a383e409bf9a40fdcada0886633a3": "CAPA v2.12.1 infrastructure-components.yaml as aws/install.py renders it",
	"1d51cdecd442f1f5f89783e9e0169b95d372724da203cc75dd7a5c4e50a10ce6": "cert-manager v1.16.2 cert-manager.yaml as install/capi.go applies it",
	"763a468d38bacd892e595ee9be96aeb8678910efc28d815e681efc75fdde71c6": "Cluster API v1.12.5 cluster-api-components.yaml as install/capi.go renders it",
}

// ErrRefused marks a write the policy does not allow.
var ErrRefused = errors.New("refused by the harness write guard")

// Kubectl checks one kubectl invocation, given its arguments without the
// program name and the manifests it would read from stdin.
func Kubectl(args []string, stdin []byte) error {
	if len(stdin) > 0 {
		sum := sha256.Sum256(stdin)
		if _, ok := Prerequisites[hex.EncodeToString(sum[:])]; ok {
			return nil
		}
	}
	writes, err := Writes(args, stdin)
	if err != nil {
		return err
	}
	for _, w := range writes {
		if err := Check(w); err != nil {
			return err
		}
	}
	return nil
}

// Check allows or refuses one write.
func Check(w Write) error {
	if w.Resource == "namespaces" && w.Verb == "delete" && slices.Contains(protected, w.Name) {
		return fmt.Errorf("%w: %s deletes a protected namespace", ErrRefused, w)
	}
	if w.Namespace != "" && !slices.Contains(protected, w.Namespace) {
		return nil
	}
	for _, list := range [][]Allowed{Policy, StandIns} {
		for _, a := range list {
			if a.Resource != w.Resource {
				continue
			}
			if (a.Namespaces == nil) != (w.Namespace == "") || (w.Namespace != "" && !slices.Contains(a.Namespaces, w.Namespace)) {
				continue
			}
			if a.Verbs != nil && !slices.Contains(a.Verbs, w.Verb) {
				continue
			}
			if w.Subresource == "status" && !a.Status {
				continue
			}
			if a.Names != nil && !slices.ContainsFunc(a.Names, func(pattern string) bool {
				ok, _ := path.Match(pattern, w.Name)
				return ok
			}) {
				continue
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s is the product's or the site's to write; if a real cluster needs it, the product must do it (see harness/e2e/product-work-audit.md)", ErrRefused, w)
}

// Writes lists what a kubectl invocation would change. A read changes
// nothing.
func Writes(args []string, stdin []byte) ([]Write, error) {
	var namespace, subresource string
	var files []string
	dryRun := false
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() string {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				return a[eq+1:]
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			i = len(args)
		case a == "-n" || a == "--namespace" || strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "--namespace="):
			namespace = value()
		case a == "-f" || a == "--filename" || strings.HasPrefix(a, "-f=") || strings.HasPrefix(a, "--filename="):
			files = append(files, value())
		case strings.HasPrefix(a, "--subresource"):
			subresource = value()
		case strings.HasPrefix(a, "--dry-run"):
			// --dry-run alone means client; only none writes.
			mode := "client"
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				mode = a[eq+1:]
			}
			dryRun = mode != "none"
		case a == "-p" || a == "--patch" || a == "-o" || a == "--output" || a == "--type" || a == "-l" || a == "--selector" ||
			a == "--field-manager" || a == "--timeout" || a == "-s" || a == "--server" || a == "--context" || a == "--kubeconfig" ||
			a == "-c" || a == "--container" || a == "--from-literal" || a == "--from-file" || a == "--image" ||
			a == "--request-timeout" || a == "--grace-period" || a == "--cascade" || a == "--patch-file" || a == "--for":
			value()
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return nil, nil
	}
	verb := positional[0]
	if !slices.Contains(writeVerbs, verb) || dryRun {
		return nil, nil
	}
	rest := positional[1:]
	if verb == "rollout" || verb == "set" || verb == "certificate" {
		if len(rest) == 0 || (verb == "rollout" && slices.Contains(readRollouts, rest[0])) {
			return nil, nil
		}
		rest = rest[1:]
	}
	if len(files) > 0 {
		if slices.ContainsFunc(files, func(f string) bool { return f != "-" }) {
			return nil, fmt.Errorf("%w: kubectl %s reads a file the guard cannot inspect; pass manifests on stdin", ErrRefused, verb)
		}
		return manifestWrites(verb, namespace, subresource, stdin)
	}
	if verb == "create" && len(rest) >= 2 {
		// A generator: create <kind> [<type>] <name>.
		resource := normalize(rest[0])
		name := rest[len(rest)-1]
		return []Write{{Verb: verb, Resource: resource, Namespace: scope(resource, namespace), Name: name, Subresource: subresource}}, nil
	}
	var writes []Write
	var resource string
	for _, token := range rest {
		// Assignments and removals (key=value, key-) are what a label,
		// annotate or set writes, not objects.
		if resource != "" && (strings.Contains(token, "=") || strings.HasSuffix(token, "-")) {
			continue
		}
		if kind, name, ok := strings.Cut(token, "/"); ok {
			r := normalize(kind)
			writes = append(writes, Write{Verb: verb, Resource: r, Namespace: scope(r, namespace), Name: name, Subresource: subresource})
			continue
		}
		if resource == "" {
			resource = normalize(token)
			continue
		}
		writes = append(writes, Write{Verb: verb, Resource: resource, Namespace: scope(resource, namespace), Name: token, Subresource: subresource})
	}
	if verb == "cordon" || verb == "uncordon" || verb == "drain" {
		// These name nodes without a resource word.
		writes = nil
		for _, token := range rest {
			writes = append(writes, Write{Verb: verb, Resource: "nodes", Name: token})
		}
	}
	if len(writes) == 0 && resource != "" {
		writes = append(writes, Write{Verb: verb, Resource: resource, Namespace: scope(resource, namespace), Subresource: subresource})
	}
	if len(writes) == 0 {
		return nil, fmt.Errorf("%w: kubectl %s names nothing the guard can check", ErrRefused, strings.Join(args, " "))
	}
	return writes, nil
}

func manifestWrites(verb, namespace, subresource string, stdin []byte) ([]Write, error) {
	var writes []Write
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(stdin), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%w: unreadable manifest: %v", ErrRefused, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if u.IsList() {
			items, err := u.ToList()
			if err != nil {
				return nil, err
			}
			for i := range items.Items {
				writes = append(writes, objectWrite(verb, namespace, subresource, &items.Items[i]))
			}
			continue
		}
		writes = append(writes, objectWrite(verb, namespace, subresource, u))
	}
	if len(writes) == 0 {
		return nil, fmt.Errorf("%w: kubectl %s with no manifest", ErrRefused, verb)
	}
	return writes, nil
}

func objectWrite(verb, namespace, subresource string, u *unstructured.Unstructured) Write {
	r := normalize(u.GetKind())
	ns := u.GetNamespace()
	if ns == "" {
		ns = namespace
	}
	return Write{Verb: verb, Resource: r, Namespace: scope(r, ns), Name: u.GetName(), Subresource: subresource}
}

// clusterScoped are the resources the harness can name that have no
// namespace.
var clusterScoped = []string{"namespaces", "nodes", "customresourcedefinitions", "clusterroles", "clusterrolebindings",
	"awsclusterstaticidentities", "awsclusterroleidentities", "awsclustercontrolleridentities", "ippools",
	"bgpconfigurations", "bgppeers", "felixconfigurations", "installations", "storageclasses", "persistentvolumes",
	"priorityclasses", "validatingwebhookconfigurations", "mutatingwebhookconfigurations", "apiservices", "ingressclasses"}

func scope(resource, namespace string) string {
	if slices.Contains(clusterScoped, resource) {
		return ""
	}
	if namespace == "" {
		return "default"
	}
	return namespace
}

// aliases maps kubectl's short and singular names to plural resources.
var aliases = map[string]string{
	"ns": "namespaces", "no": "nodes", "po": "pods", "svc": "services", "cm": "configmaps", "ds": "daemonsets",
	"deploy": "deployments", "sa": "serviceaccounts", "crd": "customresourcedefinitions", "ep": "endpoints",
	"pnc": "provisionednodeclaims",
}

func normalize(kind string) string {
	k := strings.ToLower(kind)
	if group := strings.IndexByte(k, '.'); group >= 0 {
		k = k[:group]
	}
	if a, ok := aliases[k]; ok {
		return a
	}
	switch {
	case strings.HasSuffix(k, "s") && !strings.HasSuffix(k, "ss"):
		return k
	case strings.HasSuffix(k, "y") && !strings.HasSuffix(k, "ey"):
		return k[:len(k)-1] + "ies"
	case strings.HasSuffix(k, "ss"):
		return k + "es"
	}
	return k + "s"
}
