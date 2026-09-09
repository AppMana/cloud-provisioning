package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	provider "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	rig "github.com/appmana/cloud-provisioning/harness/e2e/rig/aws"
)

func releaseLeases(ctx context.Context, r *provider.Routes, leases []string, target provider.RouteTarget) []string {
	var failures []string
	for _, lease := range leases {
		for {
			done, err := r.Release(ctx, lease, target)
			if err != nil {
				failures = append(failures, fmt.Sprintf("release incomplete: %v", err))
				break
			}
			if done {
				break
			}
			select {
			case <-ctx.Done():
				failures = append(failures, "release deadline exceeded")
			case <-time.After(time.Second):
				continue
			}
			break
		}
	}
	return failures
}

// Recovery needs only persisted resource ownership, not a surviving worker or
// Node. A changed run scope or missing journal prevents cloud deletion.
func recoverRoutes(ctx context.Context, o options, expected provider.Scope) error {
	raw, err := os.ReadFile(filepath.Join(o.output, "inputs.json"))
	if err != nil {
		return err
	}
	var input struct {
		Leases []string
		Target provider.RouteTarget
		Scope  provider.Scope
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if input.Scope != expected || len(input.Leases) != 2 {
		return fmt.Errorf("recovery inputs do not match this run")
	}
	j := journal{filepath.Join(o.output, "route.json")}
	if _, err = os.Stat(j.path); err != nil {
		return fmt.Errorf("cannot recover without the owned route journal: %w", err)
	}
	api := &observedAPI{cli: &rig.CLI{Region: expected.Region, SessionPath: filepath.Join(o.work, "gateway-session.json")}, journal: j}
	r := &provider.Routes{API: api, Journal: j, Scope: expected}
	failures := releaseLeases(ctx, r, input.Leases, input.Target)
	if len(failures) > 0 {
		return fmt.Errorf("route recovery incomplete: %v", failures)
	}
	raw, err = api.cli.Call(ctx, "ec2", "describe-route-tables", map[string]any{"RouteTableIds": []string{expected.RouteTableID}})
	if err != nil {
		return err
	}
	var tables struct {
		RouteTables []struct {
			Routes []struct{ DestinationCidrBlock string }
		}
	}
	if err = json.Unmarshal(raw, &tables); err != nil {
		return err
	}
	if len(tables.RouteTables) != 1 {
		return fmt.Errorf("ambiguous recovery read-back")
	}
	for _, route := range tables.RouteTables[0].Routes {
		if route.DestinationCidrBlock == input.Target.Destination.String() {
			return fmt.Errorf("target route remains; ownership requires inspection")
		}
	}
	report, _ := json.MarshalIndent(map[string]any{"scope": "Recovery of persisted route test leases only", "observedAt": time.Now().UTC(), "targetRouteAbsent": true, "mutations": api.ops}, "", "  ")
	path := filepath.Join(o.output, "recovery-"+time.Now().UTC().Format("20060102T150405.000000000")+".json")
	if err = os.WriteFile(path, report, 0600); err != nil {
		return err
	}
	fmt.Println("Persisted route leases released; target route confirmed absent")
	return nil
}
