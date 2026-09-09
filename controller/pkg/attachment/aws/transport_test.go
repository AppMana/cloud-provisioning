package aws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
)

type ec2HTTP func(*http.Request) (*http.Response, error)

func (f ec2HTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func testTransport(t *testing.T, action string, check func(url.Values), body string) *EC2Transport {
	t.Helper()
	tr, err := NewEC2Transport(sdk.Config{
		Region: "us-west-2", RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient: ec2HTTP(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			values, err := url.ParseQuery(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			if values.Get("Action") != action {
				t.Fatalf("action = %s", values.Get("Action"))
			}
			if r.Header.Get("Authorization") == "" {
				t.Fatal("request was not signed")
			}
			if check != nil {
				check(values)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestEC2TransportObservationContract(t *testing.T) {
	tr := testTransport(t, "DescribeNetworkInterfaces", func(v url.Values) {
		if v.Get("Filter.1.Name") != "attachment.instance-id" || v.Get("Filter.1.Value.1") != "i-gateway" {
			t.Fatalf("wrong instance filter: %v", v)
		}
	}, `<DescribeNetworkInterfacesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><networkInterfaceSet><item><networkInterfaceId>eni-gateway</networkInterfaceId><sourceDestCheck>false</sourceDestCheck><attachment><instanceId>i-gateway</instanceId><deviceIndex>0</deviceIndex></attachment><tagSet><item><key>run</key><value>owned</value></item></tagSet></item></networkInterfaceSet></DescribeNetworkInterfacesResponse>`)
	raw, err := tr.Call(context.Background(), "ec2", "describe-network-interfaces", map[string]any{"Filters": []map[string]any{{"Name": "attachment.instance-id", "Values": []string{"i-gateway"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		NetworkInterfaces []struct {
			NetworkInterfaceId string
			SourceDestCheck    *bool
			Attachment         struct {
				InstanceId  string
				DeviceIndex int
			}
			TagSet []struct{ Key, Value string }
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.NetworkInterfaces) != 1 {
		t.Fatalf("wrong interface count: %s", raw)
	}
	nic := got.NetworkInterfaces[0]
	if nic.NetworkInterfaceId != "eni-gateway" || nic.SourceDestCheck == nil || *nic.SourceDestCheck || nic.Attachment.InstanceId != "i-gateway" || len(nic.TagSet) != 1 || nic.TagSet[0].Value != "owned" {
		t.Fatalf("lost EC2 identity or false boolean: %s", raw)
	}
}

func TestEC2TransportMutations(t *testing.T) {
	cases := []struct {
		op, action string
		input      map[string]any
		want       map[string]string
	}{
		{"create-route", "CreateRoute", map[string]any{"RouteTableId": "rtb-owned", "DestinationCidrBlock": "10.10.0.11/32", "NetworkInterfaceId": "eni-gateway"}, map[string]string{"RouteTableId": "rtb-owned", "DestinationCidrBlock": "10.10.0.11/32", "NetworkInterfaceId": "eni-gateway"}},
		{"delete-route", "DeleteRoute", map[string]any{"RouteTableId": "rtb-owned", "DestinationCidrBlock": "10.10.0.11/32"}, map[string]string{"RouteTableId": "rtb-owned", "DestinationCidrBlock": "10.10.0.11/32"}},
		{"modify-network-interface-attribute", "ModifyNetworkInterfaceAttribute", map[string]any{"NetworkInterfaceId": "eni-gateway", "SourceDestCheck": map[string]any{"Value": false}}, map[string]string{"NetworkInterfaceId": "eni-gateway", "SourceDestCheck.Value": "false"}},
		{"authorize-security-group-ingress", "AuthorizeSecurityGroupIngress", map[string]any{"GroupId": "sg-owned", "IpPermissions": []map[string]any{{"IpProtocol": "udp", "FromPort": 4789, "ToPort": 4789, "IpRanges": []map[string]any{{"CidrIp": "10.10.0.11/32"}}}}, "TagSpecifications": []map[string]any{{"ResourceType": "security-group-rule", "Tags": []map[string]any{{"Key": ingressOwnerTag, "Value": "lease-token"}}}}}, map[string]string{"GroupId": "sg-owned", "IpPermissions.1.FromPort": "4789", "IpPermissions.1.IpRanges.1.CidrIp": "10.10.0.11/32", "TagSpecification.1.Tag.1.Value": "lease-token"}},
		{"revoke-security-group-ingress", "RevokeSecurityGroupIngress", map[string]any{"GroupId": "sg-owned", "SecurityGroupRuleIds": []string{"sgr-owned"}}, map[string]string{"GroupId": "sg-owned", "SecurityGroupRuleId.1": "sgr-owned"}},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			tr := testTransport(t, tc.action, func(v url.Values) {
				for k, w := range tc.want {
					if v.Get(k) != w {
						t.Errorf("%s=%q want %q", k, v.Get(k), w)
					}
				}
			}, "<"+tc.action+"Response><return>true</return></"+tc.action+"Response>")
			if _, err := tr.Call(context.Background(), "ec2", tc.op, tc.input); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEC2TransportPagination(t *testing.T) {
	tr := testTransport(t, "DescribeSecurityGroupRules", func(v url.Values) {
		if v.Get("NextToken") != "page-2" {
			t.Fatal("pagination input lost")
		}
	}, `<DescribeSecurityGroupRulesResponse><nextToken>page-3</nextToken><securityGroupRuleSet><item><securityGroupRuleId>sgr-owned</securityGroupRuleId><groupId>sg-owned</groupId><isEgress>false</isEgress><ipProtocol>udp</ipProtocol><fromPort>4789</fromPort><toPort>4789</toPort><cidrIpv4>10.10.0.11/32</cidrIpv4></item></securityGroupRuleSet></DescribeSecurityGroupRulesResponse>`)
	raw, err := tr.Call(context.Background(), "ec2", "describe-security-group-rules", map[string]any{"NextToken": "page-2"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		NextToken          string
		SecurityGroupRules []ingressRule
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.NextToken != "page-3" || len(got.SecurityGroupRules) != 1 || got.SecurityGroupRules[0].SecurityGroupRuleId != "sgr-owned" {
		t.Fatalf("lost pagination/rule: %s", raw)
	}
}

func TestEC2TransportRejectsUnsupportedInput(t *testing.T) {
	if _, err := NewEC2Transport(sdk.Config{}); err == nil {
		t.Fatal("missing credentials/region accepted")
	}
	tr := testTransport(t, "must-not-call", nil, "")
	for _, tc := range []struct {
		service, op string
		input       map[string]any
	}{
		{"ssm", "send-command", nil}, {"ec2", "terminate-instances", nil}, {"ec2", "create-route", map[string]any{"Typo": "x"}}, {"ec2", "create-route", map[string]any{"RouteTableId": 42}},
	} {
		if _, err := tr.Call(context.Background(), tc.service, tc.op, tc.input); err == nil {
			t.Fatalf("accepted %s/%s", tc.service, tc.op)
		}
	}
}

func TestEC2TransportPreservesAPIError(t *testing.T) {
	tr, err := NewEC2Transport(sdk.Config{Region: "us-west-2", RetryMaxAttempts: 1, Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: ec2HTTP(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors><RequestID>test</RequestID></Response>`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Call(context.Background(), "ec2", "describe-route-tables", map[string]any{"RouteTableIds": []string{"rtb-owned"}})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "UnauthorizedOperation" {
		t.Fatalf("lost API error: %v", err)
	}
}

func TestEC2TransportOwnedResourceObservations(t *testing.T) {
	for _, tc := range []struct{ operation, action, body, collection string }{
		{"describe-route-tables", "DescribeRouteTables", `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>rtb-owned</routeTableId><vpcId>vpc-owned</vpcId><ownerId>123</ownerId><tagSet><item><key>run</key><value>owned</value></item></tagSet><routeSet><item><destinationCidrBlock>10.10.0.11/32</destinationCidrBlock><networkInterfaceId>eni-gateway</networkInterfaceId><state>active</state><origin>CreateRoute</origin></item></routeSet></item></routeTableSet></DescribeRouteTablesResponse>`, "RouteTables"},
		{"describe-security-groups", "DescribeSecurityGroups", `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-owned</groupId><vpcId>vpc-owned</vpcId><ownerId>123</ownerId><tagSet><item><key>run</key><value>owned</value></item></tagSet></item></securityGroupInfo></DescribeSecurityGroupsResponse>`, "SecurityGroups"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			tr := testTransport(t, tc.action, nil, tc.body)
			raw, err := tr.Call(context.Background(), "ec2", tc.operation, map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string][]struct {
				VpcId, OwnerId string
				Tags           []struct{ Key, Value string }
			}
			// Ignore SDK metadata while decoding the exact resource collection.
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "ResultMetadata")
			delete(fields, "NextToken")
			filtered, _ := json.Marshal(fields)
			if err = json.Unmarshal(filtered, &got); err != nil {
				t.Fatal(err)
			}
			rows := got[tc.collection]
			if len(rows) != 1 || rows[0].VpcId != "vpc-owned" || rows[0].OwnerId != "123" || len(rows[0].Tags) != 1 || rows[0].Tags[0].Value != "owned" {
				t.Fatalf("lost ownership: %s", raw)
			}
		})
	}
}
