// Package observe records diagnostic facts without reading bootstrap or peer
// Secrets. Captures are evidence of a stage, never a replacement for assertions.
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type Command struct {
	Node   string
	Args   []string
	Output string
	Error  string `json:",omitempty"`
}
type Snapshot struct {
	Stage    string
	Time     time.Time
	Commands []Command
}

func Capture(ctx context.Context, workDir, stage string, k *kube.Client, r rig.Nodes, nodes []string) error {
	snapshot := Snapshot{Stage: stage, Time: time.Now().UTC()}
	record := func(node string, args []string, run func(context.Context) ([]byte, error)) []byte {
		bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := run(bounded)
		cmd := Command{Node: node, Args: args, Output: string(out)}
		if err != nil {
			cmd.Error = err.Error()
		}
		snapshot.Commands = append(snapshot.Commands, cmd)
		return out
	}
	for _, args := range [][]string{
		{"get", "nodes", "-o", "json"},
		{"get", "pods", "-A", "-o", "json"},
		{"get", "daemonsets", "-A", "-o", "json"},
		{"get", "deployments", "-A", "-o", "json"},
		{"get", "ippools.crd.projectcalico.org", "-o", "json"},
		{"get", "blockaffinities.crd.projectcalico.org", "-o", "json"},
	} {
		record("api", args, func(ctx context.Context) ([]byte, error) { return k.Run(ctx, args...) })
	}
	// Follow observed Machine references instead of assuming the local provider.
	args := []string{"get", "machines.cluster.x-k8s.io", "-A", "-o", "json"}
	rawMachines := record("api", args, func(ctx context.Context) ([]byte, error) { return k.Run(ctx, args...) })
	var machines struct {
		Items []struct {
			Metadata struct{ Namespace string }
			Spec     struct {
				InfrastructureRef struct{ Kind, Name, APIGroup, APIVersion string }
			}
		}
	}
	if json.Unmarshal(rawMachines, &machines) == nil {
		for _, machine := range machines.Items {
			ref := machine.Spec.InfrastructureRef
			group := ref.APIGroup
			if group == "" {
				group = strings.Split(ref.APIVersion, "/")[0]
			}
			if ref.Kind == "" || ref.Name == "" || group == "" {
				continue
			}
			args := []string{"-n", machine.Metadata.Namespace, "get", ref.Kind + "." + group, ref.Name, "-o", "json"}
			record("api", args, func(ctx context.Context) ([]byte, error) { return k.Run(ctx, args...) })
		}
	}
	for _, node := range nodes {
		for _, args := range [][]string{{"ip", "-j", "address"}, {"ip", "-j", "route", "show", "table", "all"}, {"ip", "-j", "rule"}, {"ip", "-d", "-j", "link"}} {
			record(node, args, func(ctx context.Context) ([]byte, error) { return r.Node(node).Exec(ctx, args...) })
		}
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(workDir, "observations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, stage+"-"+snapshot.Time.Format("20060102T150405.000000000Z")+".json"), raw, 0600); err != nil {
		return fmt.Errorf("recording %s: %w", stage, err)
	}
	return nil
}
