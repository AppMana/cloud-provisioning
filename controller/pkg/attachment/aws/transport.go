package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// EC2Transport uses the SDK credential provider supplied by the caller, including
// refreshable role credentials. It neither invokes a shell nor stores credentials
// in attachment journals. Ownership and lease checks remain in the adapters.
type EC2Transport struct{ client *ec2sdk.Client }

func NewEC2Transport(cfg sdk.Config) (*EC2Transport, error) {
	if cfg.Region == "" || cfg.Credentials == nil {
		return nil, fmt.Errorf("explicit AWS region and credential provider required")
	}
	return &EC2Transport{client: ec2sdk.NewFromConfig(cfg)}, nil
}

var _ API = (*EC2Transport)(nil)

// Call preserves the AWS CLI JSON field contract used by the VM harness. Only
// the EC2 operations needed by attachment resources are dispatched. Pagination
// tokens are returned unchanged; resource observers own pagination semantics.
func (t *EC2Transport) Call(ctx context.Context, service, operation string, input map[string]any) (json.RawMessage, error) {
	if t == nil || t.client == nil {
		return nil, fmt.Errorf("EC2 transport is not initialized")
	}
	if service != "ec2" {
		return nil, fmt.Errorf("unsupported attachment service %q", service)
	}
	switch operation {
	case "describe-network-interfaces":
		return sdkCall(ctx, input, t.client.DescribeNetworkInterfaces)
	case "describe-route-tables":
		return sdkCall(ctx, input, t.client.DescribeRouteTables)
	case "create-route":
		return sdkCall(ctx, input, t.client.CreateRoute)
	case "delete-route":
		return sdkCall(ctx, input, t.client.DeleteRoute)
	case "modify-network-interface-attribute":
		return sdkCall(ctx, input, t.client.ModifyNetworkInterfaceAttribute)
	case "describe-security-groups":
		return sdkCall(ctx, input, t.client.DescribeSecurityGroups)
	case "describe-security-group-rules":
		return sdkCall(ctx, input, t.client.DescribeSecurityGroupRules)
	case "authorize-security-group-ingress":
		return sdkCall(ctx, input, t.client.AuthorizeSecurityGroupIngress)
	case "revoke-security-group-ingress":
		return sdkCall(ctx, input, t.client.RevokeSecurityGroupIngress)
	default:
		return nil, fmt.Errorf("unsupported attachment EC2 operation %q", operation)
	}
}

func sdkCall[I, O any](ctx context.Context, input map[string]any, call func(context.Context, *I, ...func(*ec2sdk.Options)) (*O, error)) (json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode EC2 input: %w", err)
	}
	var request I
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("decode EC2 input: %w", err)
	}
	response, err := call(ctx, &request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}
