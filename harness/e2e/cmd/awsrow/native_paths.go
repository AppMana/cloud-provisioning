package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type nativeRoute struct {
	Dev     string `json:"dev"`
	Gateway string `json:"gateway,omitempty"`
	Type    string `json:"type,omitempty"`
}

func nativeRouteDevice(raw []byte, expected string) (string, error) {
	var routes []nativeRoute
	if err := json.Unmarshal(raw, &routes); err != nil {
		return "", err
	}
	if len(routes) != 1 || routes[0].Dev == "" || (routes[0].Type != "" && routes[0].Type != "unicast") {
		return "", fmt.Errorf("ambiguous or unusable native route")
	}
	if expected != "" && routes[0].Dev != expected {
		return "", fmt.Errorf("route uses %s instead of native interface %s", routes[0].Dev, expected)
	}
	return routes[0].Dev, nil
}

// Check both host-originated and forwarded pod traffic. A successful workload
// exchange alone cannot distinguish the VPC underlay from a redundant tunnel.
func verifyNativePaths(ctx context.Context, fleet *rig.Fleet, targets []check.Target, bindings []map[string]any, family, reportDir string) error {
	nics := map[string]string{}
	for _, binding := range bindings {
		nics[binding["node"].(string)] = binding["physicalNIC"].(string)
	}
	observations := []map[string]any{}
	var failure error
	address := func(target check.Target) string {
		if family == "IPv6" {
			return target.PodIP6
		}
		return target.PodIP
	}
	for _, source := range targets {
		nic := nics[source.Node]
		if nic == "" {
			continue
		}
		sourceIP := address(source)
		raw, err := fleet.Node(source.Node).Exec(ctx, "ip", "-j", "route", "get", sourceIP)
		ingress := ""
		if err == nil {
			ingress, err = nativeRouteDevice(raw, "")
		}
		if err != nil {
			failure = fmt.Errorf("pod interface on %s: %w", source.Node, err)
			break
		}
		for _, destination := range targets {
			if source.Node == destination.Node || nics[destination.Node] == "" {
				continue
			}
			for _, forwarded := range []bool{false, true} {
				args := []string{"ip", "-j", "route", "get", address(destination)}
				if forwarded {
					args = append(args, "from", sourceIP, "iif", ingress)
				}
				raw, err := fleet.Node(source.Node).Exec(ctx, args...)
				if err == nil {
					_, err = nativeRouteDevice(raw, nic)
				}
				observation := map[string]any{"source": source.Node, "destination": destination.Node, "sourcePod": sourceIP, "destinationPod": address(destination), "forwardedPod": forwarded, "expectedInterface": nic, "route": string(raw), "passed": err == nil}
				if err != nil {
					observation["error"] = err.Error()
					failure = err
				}
				observations = append(observations, observation)
			}
		}
	}
	if len(observations) == 0 && failure == nil {
		failure = fmt.Errorf("native routing requires at least two AWS workers")
	}
	data, _ := json.MarshalIndent(map[string]any{"passed": failure == nil, "observations": observations, "error": fmt.Sprint(failure)}, "", "  ")
	if err := os.WriteFile(filepath.Join(reportDir, "native-vpc-routes.json"), data, 0600); err != nil {
		return err
	}
	return failure
}
