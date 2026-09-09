package aws

import (
	"context"
	"encoding/json"
	"fmt"
)

// DiscoverGuestCommands selects the native SSM document from EC2 platform
// metadata. Image names, Kubernetes flavor and GPU model do not identify an OS.
func DiscoverGuestCommands(ctx context.Context, api API, instanceID string) (GuestCommands, error) {
	raw, err := api.Call(ctx, "ec2", "describe-instances", map[string]any{"InstanceIds": []string{instanceID}})
	if err != nil {
		return nil, err
	}
	var result struct {
		Reservations []struct {
			Instances []struct {
				InstanceID      string `json:"InstanceId"`
				Platform        string
				PlatformDetails string
			}
		}
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode EC2 guest platform: %w", err)
	}
	if len(result.Reservations) != 1 || len(result.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("expected exactly one EC2 instance for guest discovery")
	}
	instance := result.Reservations[0].Instances[0]
	if instance.InstanceID != instanceID {
		return nil, fmt.Errorf("EC2 guest discovery returned a different instance")
	}
	switch instance.Platform {
	case "windows":
		return WindowsCommands{}, nil
	case "":
		// An omitted Platform also occurs on non-Linux systems. Only select
		// a shell for the Linux platforms covered by this harness.
		switch instance.PlatformDetails {
		case "Linux/UNIX", "Red Hat Enterprise Linux", "SUSE Linux", "Ubuntu Pro":
			return LinuxCommands{}, nil
		}
	}
	return nil, fmt.Errorf("unsupported EC2 platform %q (%q)", instance.Platform, instance.PlatformDetails)
}
