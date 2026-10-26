package platform

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func machine(labels map[string]string) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetLabels(labels)
	return m
}

func awsDefault(os string) string { return "aws-vpc" }

// A template that names no CNI takes the platform's default; one that names
// the site CNI keeps the site CNI over the tunnel.
func TestClassOfTakesTheTemplateLabelOrThePlatformDefault(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   Class
	}{
		{"platform default", map[string]string{"kubernetes.io/os": "windows", "kubernetes.io/arch": "amd64"},
			Class{OS: "windows", Arch: "amd64", CNI: "aws-vpc"}},
		{"unlabelled guest is linux", nil, Class{OS: "linux", CNI: "aws-vpc"}},
		{"site CNI chosen explicitly", map[string]string{CNILabel: SiteCNI}, Class{OS: "linux", CNI: SiteCNI}},
		{"template overrides the default", map[string]string{CNILabel: "other"}, Class{OS: "linux", CNI: "other"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassOf(machine(c.labels), awsDefault); got != c.want {
				t.Fatalf("ClassOf = %+v, want %+v", got, c.want)
			}
		})
	}
	if got := ClassOf(machine(nil), nil); got.CNI != SiteCNI {
		t.Fatalf("a platform without a native network keeps the site CNI, got %q", got.CNI)
	}
}

// The class is the only producer of the cloud worker's labels and taint, so
// every distribution's template registers the node the same way.
func TestKubeletArgsCarryTheClassLabelsAndTaint(t *testing.T) {
	native := Class{OS: "linux", CNI: "aws-vpc"}.KubeletArgs("--max-pods=58")
	want := "--node-labels=cloud-provisioning.appmana.com/cni=aws-vpc,cloud-provisioning.appmana.com/role=cloud-worker" +
		" --register-with-taints=cloud-provisioning.appmana.com/internet-facing:NoSchedule --max-pods=58"
	if native != want {
		t.Fatalf("native args = %q\nwant %q", native, want)
	}
	site := Class{OS: "linux", CNI: SiteCNI}.KubeletArgs("")
	if strings.Contains(site, CNILabel) {
		t.Fatalf("a site-CNI node must not carry a native CNI label: %q", site)
	}
	if !strings.Contains(site, RoleLabel+"="+RoleValue) || !strings.Contains(site, TaintKey+":NoSchedule") {
		t.Fatalf("site-CNI args lost the cloud-worker role or taint: %q", site)
	}
}

// Labels and taints come only from the class; free-form flags may not
// restate them, or two sources would disagree about what a node is.
func TestExtraFlagsMayNotSetLabelsOrTaints(t *testing.T) {
	for _, flags := range []string{"--node-labels=a=b", "--max-pods=10 --register-with-taints=x:NoSchedule"} {
		if err := ValidateExtraFlags(flags); err == nil {
			t.Errorf("ValidateExtraFlags(%q) accepted a label or taint", flags)
		}
	}
	if err := ValidateExtraFlags("--max-pods=10"); err != nil {
		t.Errorf("ValidateExtraFlags rejected an ordinary flag: %v", err)
	}
}

type linuxOnly struct{}

func (linuxOnly) Name() string                  { return "linux-net" }
func (linuxOnly) VerifyNode(*corev1.Node) error { return nil }
func (linuxOnly) Supports(os, arch string) error {
	if os != "linux" {
		return errUnsupported
	}
	return nil
}

var errUnsupported = errorString("unsupported")

type errorString string

func (e errorString) Error() string { return string(e) }

// A class is admitted only when its platform runs the chosen network for
// that guest; the site CNI needs nothing from the platform.
func TestCheckAdmitsOnlyClassesThePlatformRuns(t *testing.T) {
	networks := []Network{linuxOnly{}}
	if err := (Class{OS: "linux", CNI: "linux-net"}).Check(networks); err != nil {
		t.Errorf("supported class rejected: %v", err)
	}
	if err := (Class{OS: "windows", CNI: "linux-net"}).Check(networks); err == nil {
		t.Error("a guest the network does not support was admitted")
	}
	if err := (Class{OS: "linux", CNI: "unknown"}).Check(networks); err == nil {
		t.Error("a network the platform does not provide was admitted")
	}
	if err := (Class{OS: "windows", CNI: SiteCNI}).Check(nil); err != nil {
		t.Errorf("the site CNI was rejected: %v", err)
	}
}
