package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	st "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type guestSSM struct {
	sends, polls  int
	output        string
	status        st.CommandInvocationStatus
	missing       bool
	wrongIdentity bool
	after         func()
	code          int32
}

func (s *guestSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	s.sends++
	if len(in.InstanceIds) != 1 || in.InstanceIds[0] != "i-worker" || sdk.ToString(in.DocumentName) != "AWS-RunPowerShellScript" || len(in.Parameters["commands"]) != 1 || in.Parameters["commands"][0] != attachment.CalicoHNSReadCommand {
		return nil, errors.New("wrong dispatch")
	}
	return &ssm.SendCommandOutput{Command: &st.Command{CommandId: sdk.String("command")}}, nil
}
func (s *guestSSM) GetCommandInvocation(_ context.Context, in *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	s.polls++
	if sdk.ToString(in.CommandId) != "command" || sdk.ToString(in.InstanceId) != "i-worker" {
		return nil, errors.New("wrong poll")
	}
	if s.missing && s.polls == 1 {
		return nil, &st.InvocationDoesNotExist{}
	}
	if s.after != nil {
		s.after()
	}
	instance := "i-worker"
	if s.wrongIdentity {
		instance = "i-other"
	}
	return &ssm.GetCommandInvocationOutput{CommandId: in.CommandId, InstanceId: sdk.String(instance), DocumentName: sdk.String("AWS-RunPowerShellScript"), Status: s.status, ResponseCode: s.code, StandardOutputContent: sdk.String(s.output)}, nil
}
func hnsArgs() []string {
	return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", attachment.CalicoHNSReadCommand}
}
func TestSSMGuestEventualConsistencyAndMachineBinding(t *testing.T) {
	resolver, record, ec2 := resolverFixture(t)
	api := &guestSSM{missing: true, status: st.CommandInvocationStatusSuccess, output: `[{"Name":"Calico","Type":"Overlay","ManagementIP":"172.29.0.21"}]`}
	guest := &SSMGuestReader{Resolver: resolver, api: api, pollInterval: time.Millisecond}
	ok, err := (attachment.WindowsCalicoObserver{Guest: guest}).Ready(context.Background(), record.Plan.Worker, "172.29.0.21")
	if err != nil || !ok {
		t.Fatalf("HNS observation: %v %v", ok, err)
	}
	if api.sends != 1 || api.polls != 2 || ec2.calls != 2 {
		t.Fatalf("sends=%d polls=%d identity reads=%d", api.sends, api.polls, ec2.calls)
	}
}
func TestSSMGuestRejectsUnsafeEvidence(t *testing.T) {
	for _, kind := range []string{"identity", "replacement", "truncated", "exit", "failed", "cancelled", "timeout", "foreign-before"} {
		t.Run(kind, func(t *testing.T) {
			resolver, record, ec2 := resolverFixture(t)
			api := &guestSSM{status: st.CommandInvocationStatusSuccess, output: "[]"}
			switch kind {
			case "identity":
				api.wrongIdentity = true
			case "replacement":
				api.after = func() { ec2.secondNIC = true }
			case "truncated":
				api.output = strings.Repeat("x", 24000)
			case "exit":
				api.code = 1
			case "failed":
				api.status = st.CommandInvocationStatusFailed
			case "cancelled":
				api.status = st.CommandInvocationStatusCancelled
			case "timeout":
				api.status = st.CommandInvocationStatusInProgress
			case "foreign-before":
				record.Plan.Worker.NodeUID = "replacement"
			}
			guest := &SSMGuestReader{Resolver: resolver, api: api, pollInterval: time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := guest.Read(ctx, record.Plan.Worker, hnsArgs()); err == nil {
				t.Fatal("accepted invalid evidence")
			}
			if kind == "foreign-before" && api.sends != 0 {
				t.Fatal("dispatched to replaced node")
			}
			if api.sends > 1 {
				t.Fatal("resent command during polling")
			}
		})
	}
}
func TestGuestObservationCommandAllowlist(t *testing.T) {
	for _, argv := range [][]string{{"cat", "/proc/sys/net/ipv4/ip_forward"}, {"ip", "-j", "route", "get", "172.29.0.21", "from", "10.10.0.11", "iif", "cldt1775f69e"}, hnsArgs()} {
		if _, _, err := observationCommand(argv); err != nil {
			t.Fatal(err)
		}
	}
	for _, argv := range [][]string{{"sh", "-c", "true"}, {"cat", "/etc/shadow"}, {"ip", "-j", "route", "get", "172.29.0.21; reboot", "from", "10.10.0.11", "iif", "ens5"}, {"ip", "-j", "route", "get", "172.29.0.21", "from", "10.10.0.11", "iif", "$(reboot)"}} {
		if _, _, err := observationCommand(argv); err == nil {
			t.Fatalf("accepted %v", argv)
		}
	}
}
func TestSSMGatewayRejectsDifferentInstance(t *testing.T) {
	resolver, record, _ := resolverFixture(t)
	api := &guestSSM{}
	guest := &SSMGuestReader{Resolver: resolver, api: api}
	exec := SSMGatewayExec{Guest: guest, Machine: record.Plan.Gateway}
	if _, err := exec.Read(context.Background(), InterfaceTarget{InterfaceID: record.Plan.Gateway.InterfaceID, InstanceID: "i-other"}, []string{"cat", "/proc/sys/net/ipv4/ip_forward"}); err == nil {
		t.Fatal("accepted other instance")
	}
	if api.sends != 0 {
		t.Fatal("sent command before binding check")
	}
}
