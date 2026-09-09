package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type gateBastion struct {
	rig.Node
	failedServer string
}

func (b gateBastion) Exec(_ context.Context, args ...string) ([]byte, error) {
	if b.failedServer != "" && strings.Contains(strings.Join(args, " "), b.failedServer) {
		return nil, errors.New("dial tcp 172.29.0.45:10250: connect: no route to host")
	}
	return []byte("ok"), nil
}

func TestManagementGatePreservesEveryControlPlaneResult(t *testing.T) {
	for _, failedServer := range []string{"", "https://cp:16443"} {
		t.Run("failed="+failedServer, func(t *testing.T) {
			oldPath := reportPath
			t.Cleanup(func() { reportPath = oldPath })
			reportPath = filepath.Join(t.TempDir(), "events.jsonl")
			k := &kube.Client{Bastion: gateBastion{failedServer: failedServer}, ControlPlanes: []string{"cp", "cp2"}, APIPort: 16443}
			err := qualifyManagement(context.Background(), k, "probes", []string{"aws-worker"}, "recreated remote1")
			if (err != nil) != (failedServer != "") {
				t.Fatalf("gate error = %v", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "recreated remote1") || !strings.Contains(err.Error(), failedServer)) {
				t.Fatalf("missing failure context: %v", err)
			}
			raw, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Event string
				Value struct {
					Stage, Namespace string
					Nodes            []string
					Results          []check.ManagementResult
				}
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.Event != "kubelet-access" || event.Value.Stage != "recreated remote1" || event.Value.Namespace != "probes" || len(event.Value.Nodes) != 1 || event.Value.Nodes[0] != "aws-worker" || len(event.Value.Results) != 4 {
				t.Fatalf("incomplete journal: %s", raw)
			}
			for _, result := range event.Value.Results {
				wantOK := result.Server != failedServer
				if result.OK != wantOK || result.Node != "aws-worker" || (!wantOK && !strings.Contains(result.Error, "no route to host")) {
					t.Fatalf("lost control plane evidence: %+v", result)
				}
			}
		})
	}
}

func TestManagementGateRejectsEmptyQualification(t *testing.T) {
	oldPath := reportPath
	t.Cleanup(func() { reportPath = oldPath })
	reportPath = filepath.Join(t.TempDir(), "events.jsonl")
	k := &kube.Client{Bastion: gateBastion{}}
	if err := qualifyManagement(context.Background(), k, "probes", []string{"worker"}, "baseline"); err == nil || !strings.Contains(err.Error(), "no kubelet management paths") {
		t.Fatalf("empty qualification accepted: %v", err)
	}
}
