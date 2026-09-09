package check

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// ManagementResult is independent of the out-of-band workload matrix. Each
// control plane must reach each kubelet; failover to another API would conceal
// a broken member's route or streaming proxy.
type ManagementResult struct {
	Server    string `json:"server"`
	Node      string `json:"node"`
	Operation string `json:"operation"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

func KubeletAccess(ctx context.Context, k *kube.Client, namespace string, nodes []string) []ManagementResult {
	servers := make([]string, 0, len(k.ControlPlanes))
	for _, host := range k.ControlPlanes {
		servers = append(servers, "https://"+net.JoinHostPort(host, strconv.Itoa(k.ServingPort())))
	}
	return kubeletAccess(ctx, k.Bastion.Exec, servers, namespace, nodes)
}

func kubeletAccess(ctx context.Context, run func(context.Context, ...string) ([]byte, error), servers []string, namespace string, nodes []string) []ManagementResult {
	var results []ManagementResult
	for _, server := range servers {
		for _, node := range nodes {
			for _, operation := range []string{"exec", "logs"} {
				args := []string{"kubectl", "--server=" + server, "--request-timeout=20s", "-n", namespace, operation, "hc-" + node}
				if operation == "exec" {
					args = append(args, "--", "cat", "/tmp/www/index.html")
				} else {
					args = append(args, "--tail=1")
				}
				probe, cancel := context.WithTimeout(ctx, 25*time.Second)
				out, err := run(probe, args...)
				cancel()
				if err == nil && operation == "exec" && strings.TrimSpace(string(out)) != "ok" {
					err = fmt.Errorf("exec did not return the probe's expected body")
				}
				result := ManagementResult{Server: server, Node: node, Operation: operation, OK: err == nil}
				if err != nil {
					result.Error = err.Error()
				}
				results = append(results, result)
			}
		}
	}
	return results
}
