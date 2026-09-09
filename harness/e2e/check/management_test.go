package check

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type managementBastion struct {
	rig.Node
	run func(context.Context, ...string) ([]byte, error)
}

func (b managementBastion) Exec(ctx context.Context, args ...string) ([]byte, error) {
	return b.run(ctx, args...)
}

func TestKubeletAccessKeepsObservedPrivateAddressFailureSeparate(t *testing.T) {
	var calls []string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded kubelet request")
		}
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "https://cp2:16443") && strings.Contains(call, "hc-aws-worker") {
			return nil, errors.New("dial tcp 172.29.0.45:10250: connect: no route to host")
		}
		if strings.Contains(call, " exec ") {
			return []byte("ok"), nil
		}
		return nil, nil // A silent HTTP server can have empty logs.
	}
	k := &kube.Client{Bastion: managementBastion{run: run}, ControlPlanes: []string{"cp", "cp2", "cp3"}, APIPort: 16443}
	results := KubeletAccess(context.Background(), k, "probe-run", []string{"w1", "aws-worker"})
	if len(results) != 12 || len(calls) != 12 {
		t.Fatalf("missing paths: %d results, %d calls", len(results), len(calls))
	}
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
			if r.Server != "https://cp2:16443" || r.Node != "aws-worker" || !strings.Contains(r.Error, "no route to host") {
				t.Fatalf("lost failure identity: %+v", r)
			}
		}
	}
	if failed != 2 {
		t.Fatalf("wanted both streaming and log failures, got %d", failed)
	}
}

func TestKubeletExecRequiresExpectedProbeBody(t *testing.T) {
	results := kubeletAccess(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte("unexpected"), nil }, []string{"https://cp:6443"}, "probe", []string{"worker"})
	if len(results) != 2 || results[0].OK || !results[1].OK {
		t.Fatalf("unexpected verification: %+v", results)
	}
}
