package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Real-AWS integration tests. Same conventions as harness/aws-bringup:
// every created resource carries Project=cloud-provisioning-harness (so
// the scoped identity from scripts/aws/bootstrap-harness-iam.sh can act
// on it and on nothing else), state is recorded before it's used, and
// teardown tolerates already-gone resources. Deliberately the `aws` CLI
// via exec, not an SDK dependency -- matching how containernet drives a
// real `docker` binary.
//
// Skipped (not failed) without working credentials, so `go test ./...`
// stays green on machines and CI runs that have none. The launch test
// additionally needs the same env bringup.sh requires:
//
//	SUBNET_ID -- subnet to launch into
//	AMI_ID    -- arm64 AMI in the region
const harnessTag = "Project=cloud-provisioning-harness"

func requireAWS(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: skipping real-AWS integration test")
	}
	if _, err := exec.LookPath("aws"); err != nil {
		t.Skip("aws CLI not available, skipping AWS integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "aws", "sts", "get-caller-identity").CombinedOutput(); err != nil {
		t.Skipf("no working AWS credentials (%s), skipping AWS integration test", strings.TrimSpace(string(out)))
	}
	// The ambient identity must be able to do EC2, not merely exist --
	// a machine whose default profile is scoped to something unrelated
	// (S3, etc.) has no credentials FOR THIS TEST.
	if out, err := exec.CommandContext(ctx, "aws", "ec2", "describe-instance-types", "--instance-types", "t4g.nano").CombinedOutput(); err != nil {
		t.Skipf("AWS credentials lack EC2 access (%s), skipping AWS integration test", strings.TrimSpace(string(out)))
	}
}

func awsJSON(ctx context.Context, t *testing.T, target any, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "aws", append(args, "--output", "json")...).Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("aws %s: %s", strings.Join(args, " "), strings.TrimSpace(msg))
	}
	if err := json.Unmarshal(out, target); err != nil {
		t.Fatalf("parsing aws %s output: %v", args[0], err)
	}
}

// TestCatalogMatchesRealInstanceTypes proves the static catalog's
// arch/cpu/memory facts against the real EC2 API (read-only, creates
// nothing): a wrong catalog row would make ResolveInstanceType hand a
// claim an instance that can't satisfy its requests, or the wrong-arch
// dialer binary URL.
func TestCatalogMatchesRealInstanceTypes(t *testing.T) {
	requireAWS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	names := make([]string, 0, len(catalog))
	for _, e := range catalog {
		names = append(names, e.name)
	}
	var resp struct {
		InstanceTypes []struct {
			InstanceType string `json:"InstanceType"`
			VCpuInfo     struct {
				DefaultVCpus int64 `json:"DefaultVCpus"`
			} `json:"VCpuInfo"`
			MemoryInfo struct {
				SizeInMiB int64 `json:"SizeInMiB"`
			} `json:"MemoryInfo"`
			ProcessorInfo struct {
				SupportedArchitectures []string `json:"SupportedArchitectures"`
			} `json:"ProcessorInfo"`
		} `json:"InstanceTypes"`
	}
	awsJSON(ctx, t, &resp, append([]string{"ec2", "describe-instance-types", "--instance-types"}, names...)...)

	real := map[string]struct {
		cpuMillis, memoryBytes int64
		arch                   string
	}{}
	for _, it := range resp.InstanceTypes {
		arch := "amd64"
		for _, a := range it.ProcessorInfo.SupportedArchitectures {
			if a == "arm64" {
				arch = "arm64"
			}
		}
		real[it.InstanceType] = struct {
			cpuMillis, memoryBytes int64
			arch                   string
		}{it.VCpuInfo.DefaultVCpus * 1000, it.MemoryInfo.SizeInMiB << 20, arch}
	}

	for _, e := range catalog {
		r, ok := real[e.name]
		if !ok {
			t.Errorf("catalog entry %s does not exist in EC2", e.name)
			continue
		}
		if r.arch != e.arch {
			t.Errorf("%s: catalog arch %s, real arch %s", e.name, e.arch, r.arch)
		}
		// Floors, not equality, on capacity claims: the catalog may
		// understate what an instance offers (harmless -- smallest-fit
		// just picks a size up), but overstating would resolve a claim
		// onto an instance that can't satisfy its requests.
		if r.cpuMillis < e.cpuMillis {
			t.Errorf("%s: catalog claims %dm cpu, real instance has only %dm", e.name, e.cpuMillis, r.cpuMillis)
		}
		if r.memoryBytes < e.memoryBytes {
			t.Errorf("%s: catalog claims %dMi memory, real instance has only %dMi", e.name, e.memoryBytes>>20, r.memoryBytes>>20)
		}
	}
}

// TestInfraMachineSpecActuallyLaunches is the end-to-end proof that the
// claim path's rendered values are real: resolve a request against the
// catalog, render the AWSMachine spec from a provider-config Secret
// (exactly what the claim reconciler does), then launch a real instance
// from those rendered values -- playing CAPA's role synchronously, the
// same way containernet plays it for Docker. Terminated by t.Cleanup
// even on assertion failure.
func TestInfraMachineSpecActuallyLaunches(t *testing.T) {
	requireAWS(t)
	subnetID := os.Getenv("SUBNET_ID")
	amiID := os.Getenv("AMI_ID")
	if subnetID == "" || amiID == "" {
		t.Skip("export SUBNET_ID and AMI_ID (arm64) to run the real-launch integration test (same env as harness/aws-bringup/bringup.sh)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-provider-config", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"ami-arm64": []byte(amiID),
			"subnet-id": []byte(subnetID),
			// No public IP: this test proves launchability of the
			// rendered spec, not reachability -- no ingress surface, no
			// SG rules, nothing to reach it by.
			"public-ip": []byte("false"),
		},
	}).Build()
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config"}

	req := join.NodeRequest{Arch: "arm64", CPUMillis: 100, MemoryBytes: 256 << 20, InternetFacing: false}
	instanceType, err := p.ResolveInstanceType(req)
	if err != nil {
		t.Fatalf("ResolveInstanceType: %v", err)
	}
	obj, err := p.InfraMachine(ctx, c, "default", instanceType, req)
	if err != nil {
		t.Fatalf("InfraMachine: %v", err)
	}

	// Launch from the RENDERED values, not the inputs -- the render is
	// what's under test.
	renderedType, _, _ := unstructured.NestedString(obj.Object, "spec", "instanceType")
	renderedAMI, _, _ := unstructured.NestedString(obj.Object, "spec", "ami", "id")
	renderedSubnet, _, _ := unstructured.NestedString(obj.Object, "spec", "subnet", "id")
	if renderedType == "" || renderedAMI == "" || renderedSubnet == "" {
		t.Fatalf("rendered spec incomplete: type=%q ami=%q subnet=%q", renderedType, renderedAMI, renderedSubnet)
	}

	var run struct {
		Instances []struct {
			InstanceId string `json:"InstanceId"`
		} `json:"Instances"`
	}
	awsJSON(ctx, t, &run, "ec2", "run-instances",
		"--image-id", renderedAMI,
		"--instance-type", renderedType,
		"--subnet-id", renderedSubnet,
		"--no-associate-public-ip-address",
		"--count", "1",
		"--tag-specifications", fmt.Sprintf("ResourceType=instance,Tags=[{Key=%s,Value=%s},{Key=Name,Value=cloud-provisioning-infra-machine-test}]",
			strings.SplitN(harnessTag, "=", 2)[0], strings.SplitN(harnessTag, "=", 2)[1]),
	)
	if len(run.Instances) != 1 {
		t.Fatalf("expected exactly one launched instance, got %d", len(run.Instances))
	}
	instanceID := run.Instances[0].InstanceId
	t.Logf("launched %s (%s) from the rendered AWSMachine spec", instanceID, renderedType)
	t.Cleanup(func() {
		// Fresh context: the test's own context may already be done.
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer ccancel()
		if out, err := exec.CommandContext(cctx, "aws", "ec2", "terminate-instances", "--instance-ids", instanceID).CombinedOutput(); err != nil {
			t.Errorf("terminate-instances %s: %v: %s -- TERMINATE IT MANUALLY", instanceID, err, strings.TrimSpace(string(out)))
			return
		}
		if err := exec.CommandContext(cctx, "aws", "ec2", "wait", "instance-terminated", "--instance-ids", instanceID).Run(); err != nil {
			t.Errorf("waiting for %s to terminate: %v -- verify manually", instanceID, err)
		}
	})

	if err := exec.CommandContext(ctx, "aws", "ec2", "wait", "instance-running", "--instance-ids", instanceID).Run(); err != nil {
		t.Fatalf("instance %s never reached running: %v", instanceID, err)
	}

	var desc struct {
		Reservations []struct {
			Instances []struct {
				InstanceType string `json:"InstanceType"`
				Architecture string `json:"Architecture"`
			} `json:"Instances"`
		} `json:"Reservations"`
	}
	awsJSON(ctx, t, &desc, "ec2", "describe-instances", "--instance-ids", instanceID)
	got := desc.Reservations[0].Instances[0]
	if got.InstanceType != renderedType {
		t.Errorf("running instance type = %s, want %s", got.InstanceType, renderedType)
	}
	if got.Architecture != "arm64" {
		t.Errorf("running instance architecture = %s, want arm64 (the arch the dialer binary URL selection depends on)", got.Architecture)
	}
}
