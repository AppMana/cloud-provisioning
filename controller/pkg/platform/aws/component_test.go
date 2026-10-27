package aws

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/upstream"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

func prefixes(raw ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range raw {
		out = append(out, netip.MustParsePrefix(r))
	}
	return out
}

// The facts of the 2026-10-14 qualification site.
func qualifiedFacts() VPCCNIFacts {
	return VPCCNIFacts{Family: "ipv4", SiteRanges: prefixes("10.101.0.0/24", "10.101.128.0/17", "10.101.4.0/22"), PodMTU: 1420}
}

func decodeObjects(t *testing.T, raw []byte) map[string]*unstructured.Unstructured {
	t.Helper()
	objects := map[string]*unstructured.Unstructured{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(u.Object) > 0 {
			objects[u.GetKind()] = u
		}
	}
	return objects
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

// The product's aws-node is the one the qualification ran, apart from the
// harness's run tag on ENIs: with one ENI per node, the VPC CNI creates no
// interface to tag.
func TestTheProductsAWSNodeIsTheQualifiedOne(t *testing.T) {
	raw, err := os.ReadFile("testdata/qualified-ipv4-20261014.yaml")
	if err != nil {
		t.Fatal(err)
	}
	qualified := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(decodeObjects(t, raw)["DaemonSet"].Object, qualified); err != nil {
		t.Fatal(err)
	}
	configMap, ds, err := VPCCNIObjects(qualifiedFacts())
	if err != nil {
		t.Fatal(err)
	}
	want, got := qualified.Spec.Template.Spec.DeepCopy(), ds.Spec.Template.Spec.DeepCopy()
	wantEnv, gotEnv := envOf(want.Containers[0]), envOf(got.Containers[0])
	delete(wantEnv, "ADDITIONAL_ENI_TAGS")
	delete(gotEnv, "ADDITIONAL_ENI_TAGS")
	// The exclusions are a set; the product lists them in address order.
	for _, env := range []map[string]corev1.EnvVar{wantEnv, gotEnv} {
		e := env["AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS"]
		ranges := strings.Split(e.Value, ",")
		sort.Strings(ranges)
		e.Value = strings.Join(ranges, ",")
		env[e.Name] = e
	}
	if !reflect.DeepEqual(wantEnv, gotEnv) {
		for name, e := range wantEnv {
			if !reflect.DeepEqual(gotEnv[name], e) {
				t.Errorf("%s = %+v, qualified %+v", name, gotEnv[name], e)
			}
		}
		for name := range gotEnv {
			if _, ok := wantEnv[name]; !ok {
				t.Errorf("%s set, not in the qualified render", name)
			}
		}
	}
	want.Containers[0].Env, got.Containers[0].Env = nil, nil
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("pod spec differs from the qualified render:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(ds.Spec.Selector, qualified.Spec.Selector) {
		t.Fatal("the selector differs, so an aws-node installed by hand could not be adopted")
	}
	if ds.Labels[platform.ComponentLabel] != (VPCCNI{}).Name() || ds.Labels["app.kubernetes.io/managed-by"] != "cloud-provisioning" || ds.Labels["helm.sh/chart"] != "" {
		t.Fatalf("labels = %v", ds.Labels)
	}
	if configMap.Name != "amazon-vpc-cni" || configMap.Namespace != "kube-system" || configMap.Data["enable-windows-ipam"] != "false" {
		t.Fatalf("ConfigMap = %+v", configMap)
	}
}

// IPv6 pods take delegated prefixes from the primary interface.
func TestIPv6PodsUsePrefixDelegation(t *testing.T) {
	facts := qualifiedFacts()
	facts.Family = "ipv6"
	_, ds, err := VPCCNIObjects(facts)
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(ds.Spec.Template.Spec.Containers[0])
	for name, value := range map[string]string{"ENABLE_IPv4": "false", "ENABLE_IPv6": "true", "ENABLE_PREFIX_DELEGATION": "true", "WARM_PREFIX_TARGET": "1"} {
		if env[name].Value != value {
			t.Errorf("%s = %q, want %q", name, env[name].Value, value)
		}
	}
	if _, ok := env["WARM_IP_TARGET"]; ok {
		t.Error("an IPv4 warm target is set on IPv6 pods")
	}
	if envOf(ds.Spec.Template.Spec.InitContainers[0])["ENABLE_IPv6"].Value != "true" {
		t.Error("the init container does not prepare IPv6")
	}
}

// What the facts cannot support is refused rather than rendered.
func TestImpossibleFactsAreRefused(t *testing.T) {
	for name, mutate := range map[string]func(*VPCCNIFacts){
		"no site ranges":    func(f *VPCCNIFacts) { f.SiteRanges = nil },
		"dual-stack":        func(f *VPCCNIFacts) { f.Family = "dual" },
		"MTU above tunnel":  func(f *VPCCNIFacts) { f.PodMTU = 1500 },
		"MTU below IPv6":    func(f *VPCCNIFacts) { f.PodMTU = 1200 },
		"unpinned image":    func(f *VPCCNIFacts) { f.Image = "public.ecr.aws/eks/amazon-k8s-cni:v1.23.2" },
		"overlapping range": func(f *VPCCNIFacts) { f.SiteRanges = prefixes("10.101.0.0/16", "10.101.4.0/22") },
	} {
		facts := qualifiedFacts()
		mutate(&facts)
		if _, _, err := VPCCNIObjects(facts); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// The embedded render is the pinned chart's.
func TestTheEmbeddedRenderIsThePinnedChart(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("requires Helm")
	}
	dir := t.TempDir()
	for _, name := range []string{"render.sh", "values.yaml", "aws-vpc-cni-1.23.2.tgz"} {
		raw, err := os.ReadFile(filepath.Join("upstream", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command(filepath.Join(dir, "render.sh")).CombinedOutput(); err != nil {
		t.Fatalf("render: %v: %s", err, out)
	}
	rendered, err := os.ReadFile(filepath.Join(dir, "aws-vpc-cni.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rendered, upstream.Manifest) {
		t.Fatal("upstream/aws-vpc-cni.yaml is not what the pinned chart renders; run go generate")
	}
}

// The chart's aws-node identity is upstream's: its ClusterRole grants what
// the pinned VPC CNI needs and nothing more.
func TestTheChartsAWSNodeRulesAreUpstreams(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("requires Helm")
	}
	out, err := exec.Command("helm", "template", "rules-test", "../../../../charts/cloud-provisioning", "--show-only", "templates/aws-vpc-cni.yaml").Output()
	if err != nil {
		t.Fatal(err)
	}
	var chart, pinned *unstructured.Unstructured
	for _, objects := range []struct {
		raw  []byte
		into **unstructured.Unstructured
	}{{out, &chart}, {upstream.Manifest, &pinned}} {
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(objects.raw), 4096)
		for {
			u := &unstructured.Unstructured{}
			if err := decoder.Decode(&u.Object); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if u.GetKind() == "ClusterRole" && u.GetName() == "aws-node" {
				*objects.into = u
			}
		}
	}
	if chart == nil || pinned == nil || !reflect.DeepEqual(chart.Object["rules"], pinned.Object["rules"]) {
		t.Fatalf("chart rules %v, upstream %v", chart, pinned)
	}
}

// AWS pods get the tunnel's MTU, or the site's pod MTU where the site set a
// smaller budget for its paths.
func TestAWSPodsTakeTheSitesSmallerPodMTU(t *testing.T) {
	raw, err := os.ReadFile("../../cni/site/testdata/k0s-v1.36.5-calico-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for site, want := range map[string]int{"1450": 1420, "1370": 1370} {
		ds := &appsv1.DaemonSet{}
		if err := sigsyaml.Unmarshal(bytes.Replace(raw, []byte(`"mtu": 1450`), []byte(`"mtu": `+site), 1), ds); err != nil {
			t.Fatal(err)
		}
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		got, err := podMTU(context.Background(), fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds).Build())
		if err != nil || got != want {
			t.Errorf("site MTU %s: pod MTU %d, %v; want %d", site, got, err, want)
		}
	}
}
