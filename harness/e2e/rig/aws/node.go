// Package aws provides EC2 machine operations for the shared harness. CAPA
// owns provisioning; this adapter observes and operates on its existing VM.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// API is the AWS control-plane boundary, independent of guest execution and
// of the Kubernetes distribution installed on the instance.
type API interface {
	Call(context.Context, string, string, map[string]any) (json.RawMessage, error)
}

// InputStore stages stdin privately for SSM, which has no streaming transport.
// The returned URL must expire; remove deletes the object after the command.
type InputStore interface {
	Stage(context.Context, io.Reader) (url string, remove func(context.Context) error, err error)
}

type Node struct {
	API        API
	Input      InputStore
	NodeName   string
	InstanceID string
	// NIC is discovered from the running image, not inferred from AWS or distro.
	NIC string
	// Commands defaults to Linux for existing callers. Windows images must
	// explicitly select WindowsCommands from their image contract.
	Commands GuestCommands
}

var _ rig.Node = (*Node)(nil)

func (n *Node) Name() string { return n.NodeName }
func (n *Node) Interface(index int) string {
	if index != 0 {
		return ""
	}
	return n.NIC
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (n *Node) commands() GuestCommands {
	if n.Commands != nil {
		return n.Commands
	}
	return LinuxCommands{}
}

func (n *Node) BootstrapFailure(ctx context.Context) error {
	return n.commands().BootstrapFailure(ctx, n)
}

func (n *Node) Exec(ctx context.Context, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: empty command", n.Name())
	}
	return n.command(ctx, n.commands().Exec(argv))
}

func (n *Node) command(ctx context.Context, command string) ([]byte, error) {
	executionTimeout, err := ssmExecutionTimeout(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := n.API.Call(ctx, "ssm", "send-command", map[string]any{
		"InstanceIds": []string{n.InstanceID}, "DocumentName": n.commands().Document(),
		"Parameters": map[string][]string{"commands": {command}, "executionTimeout": {executionTimeout}},
	})
	if err != nil {
		return nil, err
	}
	var sent struct {
		Command struct {
			CommandID string `json:"CommandId"`
		}
	}
	if err := json.Unmarshal(raw, &sent); err != nil || sent.Command.CommandID == "" {
		return nil, fmt.Errorf("%s: SSM returned no command ID", n.Name())
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: observing SSM command %s: %w", n.Name(), sent.Command.CommandID, ctx.Err())
		case <-time.After(time.Second):
		}
		raw, err = n.API.Call(ctx, "ssm", "get-command-invocation", map[string]any{"InstanceId": n.InstanceID, "CommandId": sent.Command.CommandID})
		if errors.Is(err, ErrInvocationPending) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: observing SSM command %s: %w", n.Name(), sent.Command.CommandID, err)
		}
		var result struct {
			Status                                      string
			ResponseCode                                int
			StandardOutputContent, StandardErrorContent string
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		switch result.Status {
		case "Pending", "InProgress", "Delayed", "Cancelling":
			continue
		case "Success":
			// SSM inline output is bounded. Never present truncated evidence as
			// a complete command result; large transfers use the InputStore.
			if len(result.StandardOutputContent) >= 24000 || len(result.StandardErrorContent) >= 8000 {
				return nil, fmt.Errorf("%s: observing SSM command %s: inline output limit reached (stdout=%d bytes, stderr=%d bytes)", n.Name(), sent.Command.CommandID, len(result.StandardOutputContent), len(result.StandardErrorContent))
			}
			return []byte(result.StandardOutputContent), nil
		default:
			return []byte(result.StandardOutputContent), &rig.ExitError{Node: n.Name(), Code: result.ResponseCode, Stderr: []byte(result.StandardErrorContent)}
		}
	}
}

// SSM's document execution limit is separate from the caller's observation
// deadline. Long Windows layer preparation must not retain a hidden ten-minute
// execution cap. AWS Run Command permits at most 48 hours of execution.
func ssmExecutionTimeout(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	duration := 10 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		duration = time.Until(deadline)
		if duration <= 0 {
			return "", context.DeadlineExceeded
		}
	}
	if duration > 48*time.Hour {
		duration = 48 * time.Hour
	}
	seconds := int64((duration + time.Second - 1) / time.Second)
	return fmt.Sprint(seconds), nil
}

func (n *Node) Pipe(ctx context.Context, src io.Reader, argv ...string) (output []byte, resultErr error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: empty command", n.Name())
	}
	return n.staged(ctx, src, func(url string) string { return n.commands().Pipe(url, argv) })
}

func (n *Node) staged(ctx context.Context, src io.Reader, command func(string) string) (output []byte, resultErr error) {
	if n.Input == nil {
		return nil, fmt.Errorf("%s: SSM stdin requires a private input store", n.Name())
	}
	url, remove, err := n.Input.Stage(ctx, src)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := remove(cleanup); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("%s: removing private command input: %w", n.Name(), err)
		}
	}()
	return n.command(ctx, command(url))
}

func (n *Node) Put(ctx context.Context, src io.Reader, dst string, mode fs.FileMode) error {
	_, err := n.staged(ctx, src, func(url string) string { return n.commands().Put(url, dst, mode) })
	return err
}

func (n *Node) Cut(context.Context) error {
	return fmt.Errorf("%s: physical NIC cuts are unsupported through SSM", n.Name())
}
func (n *Node) Restore(context.Context) error {
	return fmt.Errorf("%s: physical NIC restoration is unsupported through SSM", n.Name())
}
func (n *Node) Kill(ctx context.Context) error {
	_, err := n.API.Call(ctx, "ec2", "stop-instances", map[string]any{"InstanceIds": []string{n.InstanceID}, "SkipOsShutdown": true})
	return err
}
func (n *Node) Boot(ctx context.Context) error {
	_, err := n.API.Call(ctx, "ec2", "start-instances", map[string]any{"InstanceIds": []string{n.InstanceID}})
	return err
}
func (n *Node) Userdata(context.Context, []byte) error {
	return fmt.Errorf("%s: CAPA owns first-boot user data; create or replace its Machine", n.Name())
}

func (n *Node) BootstrapComplete(ctx context.Context) (bool, error) {
	observer, ok := n.commands().(interface {
		BootstrapComplete(context.Context, rig.Node) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("%s: completed bootstrap observation is unsupported", n.Name())
	}
	return observer.BootstrapComplete(ctx, n)
}

// InstanceIdentity binds guest bootstrap receipts to their EC2 machine.
func (n *Node) InstanceIdentity() string { return n.InstanceID }
