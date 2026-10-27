package aws

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	corev1 "k8s.io/api/core/v1"
)

const ipamImage = "registry.example/aws-windows-ipam@sha256:d1d0b2bb00b9c8ccf91dde169e4849e81b3dc07d803484f13e074fe1828695a7"

// AWS's VPC resource controller, patched to manage only this product's
// Windows VPC CNI nodes, runs on Linux VPC CNI workers: in the VPC, where
// IMDS gives it its region and the worker role's credentials. It manages the
// one AWS cluster's VPC, under that cluster's name.
func TestWindowsIPAMRunsInTheVPCForItsCluster(t *testing.T) {
	d, err := WindowsIPAMDeployment(WindowsIPAMFacts{ClusterName: "cldt-site", VPCID: "vpc-0123", Image: ipamImage, PullSecrets: []string{"ipam-pull"}})
	if err != nil {
		t.Fatal(err)
	}
	spec := d.Spec.Template.Spec
	if d.Name != "vpc-resource-local-controller" || d.Namespace != "kube-system" || d.Labels[platform.ComponentLabel] != (VPCCNI{}).Name() {
		t.Fatalf("Deployment %s/%s %v", d.Namespace, d.Name, d.Labels)
	}
	if !spec.HostNetwork || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Fatal("not on the worker's network, where IMDS answers")
	}
	if spec.NodeSelector[corev1.LabelOSStable] != "linux" || spec.NodeSelector[platform.CNILabel] != (VPCCNI{}).Name() || spec.NodeSelector[platform.RoleLabel] != platform.RoleValue {
		t.Fatalf("node selector %v", spec.NodeSelector)
	}
	if !slices.ContainsFunc(spec.Tolerations, func(t corev1.Toleration) bool { return t.Key == platform.TaintKey }) {
		t.Fatal("does not tolerate the cloud worker taint")
	}
	anti := spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(anti) != 1 || anti[0].TopologyKey != corev1.LabelHostname {
		t.Fatalf("replicas can share a host and collide on host ports: %+v", anti)
	}
	if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == "aws-iam-token" }) {
		t.Fatal("keeps EKS's projected identity token")
	}
	if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "ipam-pull" {
		t.Fatalf("pull secrets %v", spec.ImagePullSecrets)
	}
	c := spec.Containers[0]
	if c.Image != ipamImage {
		t.Fatalf("image %s", c.Image)
	}
	want := []string{"--cluster-name=cldt-site", "--vpc-id=vpc-0123", "--leader-elect", "--metrics-bind-address=:8443", "--introspect-bind-addr=:22775",
		"--node-selector=" + strings.Join([]string{corev1.LabelOSStable + "=windows", platform.CNILabel + "=aws-vpc", platform.RoleLabel + "=" + platform.RoleValue}, ",")}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("args\n got %q\nwant %q", c.Args, want)
	}
}

func TestWindowsIPAMRefusesWhatItCannotRun(t *testing.T) {
	for name, f := range map[string]WindowsIPAMFacts{
		"unpinned image": {ClusterName: "c", VPCID: "vpc-1", Image: "registry.example/aws-windows-ipam:latest"},
		"no cluster":     {VPCID: "vpc-1", Image: ipamImage},
		"no VPC":         {ClusterName: "c", Image: ipamImage},
	} {
		if _, err := WindowsIPAMDeployment(f); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The VPC CNI's ConfigMap turns Windows IPAM on only once the controller
// serving it is available: its webhooks fail closed, and the controller
// starts Windows address management from this setting.
func TestWindowsIPAMIsEnabledOnceItsControllerIsAvailable(t *testing.T) {
	for _, available := range []bool{false, true} {
		f := qualifiedFacts()
		f.WindowsIPAM = available
		configMap, _, err := VPCCNIObjects(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := configMap.Data["enable-windows-ipam"]; got != map[bool]string{false: "false", true: "true"}[available] {
			t.Errorf("available %v: enable-windows-ipam %q", available, got)
		}
	}
}

// Windows VPC CNI nodes need Windows IPAM; without it their claims are
// refused rather than given nodes whose pods get no addresses.
func TestWindowsVPCCNINodesNeedWindowsIPAM(t *testing.T) {
	if err := (VPCCNI{}).Supports("windows", "amd64"); err == nil {
		t.Fatal("Windows admitted without Windows IPAM")
	}
	if err := (VPCCNI{Windows: true}).Supports("windows", "amd64"); err != nil {
		t.Fatal(err)
	}
	if err := (VPCCNI{Windows: true}).Supports("windows", "arm64"); err == nil {
		t.Fatal("Windows arm64 admitted; AWS's Windows IPAM is amd64 only")
	}
	if err := (VPCCNI{}).Supports("linux", "arm64"); err != nil {
		t.Fatal(err)
	}
}

// The embedded Deployment and the chart's Windows IPAM objects are what the
// vendored upstream config renders.
func TestTheWindowsIPAMRenderIsTheVendoredConfig(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("requires kustomize")
	}
	dir := t.TempDir()
	deployment, chart := filepath.Join(dir, "deployment.yaml"), filepath.Join(dir, "chart.yaml")
	if out, err := exec.Command("upstream/vpc-resource-controller/render.sh", deployment, chart).CombinedOutput(); err != nil {
		t.Fatalf("render: %v: %s", err, out)
	}
	for rendered, committed := range map[string]string{
		deployment: "upstream/vpc-resource-controller/deployment.yaml",
		chart:      "../../../../charts/cloud-provisioning/templates/aws-windows-ipam.yaml",
	} {
		got, err := os.ReadFile(rendered)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(committed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is not what the vendored config renders; run go generate", committed)
		}
	}
}
