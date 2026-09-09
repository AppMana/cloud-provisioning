package aws

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type ssmAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// SSMGuestReader revalidates CAPI, Node and ENI identity before dispatch and after
// observing completion. Only the fixed forwarding/HNS probes are accepted.
// A command is sent once; eventual-consistency misses only retry observation.
type SSMGuestReader struct {
	Resolver     Resolver
	api          ssmAPI
	pollInterval time.Duration
}

func NewSSMGuestReader(cfg sdk.Config, resolver Resolver) (*SSMGuestReader, error) {
	if cfg.Region == "" || cfg.Region != resolver.Scope.Region || cfg.Credentials == nil {
		return nil, fmt.Errorf("SSM region must match resolver scope and credentials are required")
	}
	return &SSMGuestReader{Resolver: resolver, api: ssm.NewFromConfig(cfg), pollInterval: time.Second}, nil
}

var _ attachment.MachineGuestReader = (*SSMGuestReader)(nil)

var guestInterface = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`)

func observationCommand(argv []string) (document, script string, err error) {
	windows := []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", attachment.CalicoHNSReadCommand}
	if reflect.DeepEqual(argv, windows) {
		return "AWS-RunPowerShellScript", attachment.CalicoHNSReadCommand, nil
	}
	if reflect.DeepEqual(argv, []string{"cat", "/proc/sys/net/ipv4/ip_forward"}) {
		return "AWS-RunShellScript", "cat /proc/sys/net/ipv4/ip_forward", nil
	}
	if len(argv) == 9 && argv[0] == "ip" && argv[1] == "-j" && argv[2] == "route" && argv[3] == "get" && argv[5] == "from" && argv[7] == "iif" && guestInterface.MatchString(argv[8]) {
		dst, e1 := netip.ParseAddr(argv[4])
		src, e2 := netip.ParseAddr(argv[6])
		if e1 == nil && e2 == nil && dst.Is4() && src.Is4() {
			// Every variable token was validated above. Quote the NIC even though the
			// accepted character set excludes shell syntax.
			return "AWS-RunShellScript", strings.Join(argv[:8], " ") + " '" + argv[8] + "'", nil
		}
	}
	return "", "", fmt.Errorf("unsupported guest observation command")
}

func (g *SSMGuestReader) Read(ctx context.Context, m attachment.Machine, argv []string) ([]byte, error) {
	if g == nil || g.api == nil {
		return nil, fmt.Errorf("SSM guest reader is not initialized")
	}
	document, script, err := observationCommand(argv)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	target, err := g.Resolver.machine(ctx, m)
	if err != nil {
		return nil, err
	}
	result, err := g.api.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds: []string{target.InstanceID}, DocumentName: sdk.String(document), TimeoutSeconds: sdk.Int32(60),
		Parameters: map[string][]string{"commands": {script}, "executionTimeout": {"30"}},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Command == nil || sdk.ToString(result.Command.CommandId) == "" {
		return nil, fmt.Errorf("SSM returned no command identity")
	}
	id := result.Command.CommandId
	for {
		observation, err := g.api.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: id, InstanceId: sdk.String(target.InstanceID)})
		if err != nil {
			var missing *ssmtypes.InvocationDoesNotExist
			if !errors.As(err, &missing) {
				return nil, err
			}
		} else {
			if observation == nil || sdk.ToString(observation.CommandId) != *id || sdk.ToString(observation.InstanceId) != target.InstanceID || sdk.ToString(observation.DocumentName) != document {
				return nil, fmt.Errorf("SSM invocation identity mismatch")
			}
			switch observation.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				if observation.ResponseCode != 0 {
					return nil, fmt.Errorf("SSM observation returned nonzero exit code")
				}
				output := sdk.ToString(observation.StandardOutputContent)
				if utf8.RuneCountInString(output) >= 24000 {
					return nil, fmt.Errorf("SSM observation may be truncated")
				}
				after, err := g.Resolver.machine(ctx, m)
				if err != nil {
					return nil, err
				}
				if after != target {
					return nil, fmt.Errorf("guest identity changed during observation")
				}
				return []byte(output), nil
			case ssmtypes.CommandInvocationStatusPending, ssmtypes.CommandInvocationStatusInProgress, ssmtypes.CommandInvocationStatusDelayed:
			default:
				return nil, fmt.Errorf("SSM observation ended with status %s", observation.Status)
			}
		}
		interval := g.pollInterval
		if interval <= 0 {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// SSMGatewayExec binds the narrower infrastructure interface to the complete
// gateway Machine identity. It cannot run a probe on an arbitrary instance ID.
type SSMGatewayExec struct {
	Guest   *SSMGuestReader
	Machine attachment.Machine
}

var _ GuestExec = SSMGatewayExec{}

func (g SSMGatewayExec) Read(ctx context.Context, target InterfaceTarget, argv []string) ([]byte, error) {
	if g.Guest == nil {
		return nil, fmt.Errorf("gateway guest reader required")
	}
	bound, err := g.Guest.Resolver.machine(ctx, g.Machine)
	if err != nil {
		return nil, err
	}
	if bound != target {
		return nil, fmt.Errorf("gateway execution binding mismatch")
	}
	return g.Guest.Read(ctx, g.Machine, argv)
}
