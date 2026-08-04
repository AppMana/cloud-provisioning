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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// Real-AWS integration tests. Same conventions as harness/aws-bringup:
// every created resource carries Project=cloud-provisioning-harness (so
// the scoped identity from scripts/aws/bootstrap-harness-iam.sh can act
// on it and on nothing else), state is recorded before it's used, and
// teardown tolerates already-gone resources. It drives the `aws` CLI
// via exec rather than taking an SDK dependency, matching how
// containernet drives a real `docker` binary.
//
// Skipped (not failed) without working credentials, so `go test ./...`
// stays green on machines and CI runs that have none. The launch test
// additionally needs the same env bringup.sh requires:
//
//	SUBNET_ID subnet to launch into
//	AMI_ID    arm64 AMI in the region
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

// TestInfraMachineSpecActuallyLaunches is the end-to-end proof that the
// claim path's rendered values are real: resolve a request against the
// catalog, render the AWSMachine spec from a provider-config Secret
// (exactly what the claim reconciler does), then launch a real instance
// from those rendered values, playing CAPA's role synchronously the
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
	// The machine's spec as a template carries it: the same values a
	// claim's infrastructureRef would hand to the provider.
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"instanceType": "t4g.small",
		"ami":          map[string]any{"id": amiID},
		"subnet":       map[string]any{"id": subnetID},
		// No public IP: this proves the spec is launchable, not that
		// anything can reach it.
		"publicIP": false,
	}}}
	obj.SetGroupVersionKind(gvk)

	// Launch from the rendered values, not the inputs; the render is
	// what is under test.
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
			t.Errorf("terminate-instances %s: %v: %s. TERMINATE IT MANUALLY", instanceID, err, strings.TrimSpace(string(out)))
			return
		}
		if err := exec.CommandContext(cctx, "aws", "ec2", "wait", "instance-terminated", "--instance-ids", instanceID).Run(); err != nil {
			t.Errorf("waiting for %s to terminate: %v. Verify manually", instanceID, err)
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
