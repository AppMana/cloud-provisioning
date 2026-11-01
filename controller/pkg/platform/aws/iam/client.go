package iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	defaultIAMEndpoint = "https://iam.amazonaws.com/"
	iamAPIVersion      = "2010-05-08"
	iamSigningRegion   = "us-east-1"
)

// Client implements the IAM API interface using AWS SDK v2 credentials and SigV4
// against the standard AWS IAM Query API.
type Client struct {
	cfg      sdk.Config
	endpoint string
	signer   *v4.Signer
}

var _ API = (*Client)(nil)

// NewClientFromDefaultConfig loads the default AWS credential chain (developer
// environment, shared profile, SSO, or mounted secret) and returns an IAM Client
// plus the resolved 12-digit AWS account ID.
func NewClientFromDefaultConfig(ctx context.Context, region string) (*Client, string, error) {
	if region == "" {
		region = iamSigningRegion
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region), config.WithRetryMaxAttempts(3))
	if err != nil {
		return nil, "", fmt.Errorf("loading AWS configuration: %w", err)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, "", fmt.Errorf("calling sts:GetCallerIdentity: %w", err)
	}
	accountID := ""
	if out.Account != nil {
		accountID = *out.Account
	}
	return NewClient(cfg), accountID, nil
}

// NewClient constructs an IAM Client from an existing AWS SDK v2 Config.
func NewClient(cfg sdk.Config) *Client {
	return &Client{
		cfg:      cfg,
		endpoint: defaultIAMEndpoint,
		signer:   v4.NewSigner(),
	}
}

func (c *Client) call(ctx context.Context, action string, params map[string]string, out any) (int, string, error) {
	form := url.Values{}
	form.Set("Action", action)
	form.Set("Version", iamAPIVersion)
	for k, v := range params {
		form.Set(k, v)
	}
	body := form.Encode()
	sum := sha256.Sum256([]byte(body))
	payloadHash := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	creds, err := c.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("retrieving AWS credentials for IAM %s: %w", action, err)
	}
	if err := c.signer.SignHTTP(ctx, creds, req, payloadHash, "iam", iamSigningRegion, time.Now().UTC()); err != nil {
		return 0, "", fmt.Errorf("signing IAM %s request: %w", action, err)
	}

	httpClient := c.cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		}
		_ = xml.Unmarshal(raw, &errResp)
		if errResp.Error.Code == "NoSuchEntity" {
			return resp.StatusCode, "NoSuchEntity", nil
		}
		if errResp.Error.Code != "" {
			return resp.StatusCode, errResp.Error.Code, fmt.Errorf("IAM %s failed (%s): %s", action, errResp.Error.Code, errResp.Error.Message)
		}
		return resp.StatusCode, "", fmt.Errorf("IAM %s HTTP %d: %s", action, resp.StatusCode, string(raw))
	}
	if out != nil {
		if err := xml.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, "", fmt.Errorf("decoding IAM %s response: %w", action, err)
		}
	}
	return resp.StatusCode, "", nil
}

func (c *Client) GetPolicy(ctx context.Context, arn string) (bool, string, error) {
	var resp struct {
		Policy struct {
			Arn              string `xml:"Arn"`
			DefaultVersionId string `xml:"DefaultVersionId"`
		} `xml:"GetPolicyResult>Policy"`
	}
	_, code, err := c.call(ctx, "GetPolicy", map[string]string{"PolicyArn": arn}, &resp)
	if code == "NoSuchEntity" {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, resp.Policy.DefaultVersionId, nil
}

func (c *Client) GetPolicyVersionDocument(ctx context.Context, arn, versionID string) (string, error) {
	var resp struct {
		Document string `xml:"GetPolicyVersionResult>PolicyVersion>Document"`
	}
	_, _, err := c.call(ctx, "GetPolicyVersion", map[string]string{
		"PolicyArn": arn,
		"VersionId": versionID,
	}, &resp)
	if err != nil {
		return "", err
	}
	decoded, err := url.QueryUnescape(resp.Document)
	if err != nil {
		return resp.Document, nil
	}
	return decoded, nil
}

func (c *Client) CreatePolicy(ctx context.Context, name, description, document string) (string, error) {
	var resp struct {
		Arn string `xml:"CreatePolicyResult>Policy>Arn"`
	}
	_, _, err := c.call(ctx, "CreatePolicy", map[string]string{
		"PolicyName":     name,
		"Description":    description,
		"PolicyDocument": document,
	}, &resp)
	return resp.Arn, err
}

func (c *Client) ListPolicyVersions(ctx context.Context, arn string) ([]PolicyVersion, error) {
	var resp struct {
		Versions []struct {
			VersionId        string `xml:"VersionId"`
			IsDefaultVersion bool   `xml:"IsDefaultVersion"`
			CreateDate       string `xml:"CreateDate"`
		} `xml:"ListPolicyVersionsResult>Versions>member"`
	}
	_, _, err := c.call(ctx, "ListPolicyVersions", map[string]string{"PolicyArn": arn}, &resp)
	if err != nil {
		return nil, err
	}
	out := make([]PolicyVersion, 0, len(resp.Versions))
	for _, v := range resp.Versions {
		out = append(out, PolicyVersion{
			VersionID:        v.VersionId,
			IsDefaultVersion: v.IsDefaultVersion,
			CreateDate:       v.CreateDate,
		})
	}
	return out, nil
}

func (c *Client) DeletePolicyVersion(ctx context.Context, arn, versionID string) error {
	_, _, err := c.call(ctx, "DeletePolicyVersion", map[string]string{
		"PolicyArn": arn,
		"VersionId": versionID,
	}, nil)
	return err
}

func (c *Client) CreatePolicyVersion(ctx context.Context, arn, document string, setAsDefault bool) (string, error) {
	var resp struct {
		VersionId string `xml:"CreatePolicyVersionResult>PolicyVersion>VersionId"`
	}
	setDef := "false"
	if setAsDefault {
		setDef = "true"
	}
	_, _, err := c.call(ctx, "CreatePolicyVersion", map[string]string{
		"PolicyArn":      arn,
		"PolicyDocument": document,
		"SetAsDefault":   setDef,
	}, &resp)
	return resp.VersionId, err
}

func (c *Client) GetRole(ctx context.Context, name string) (bool, string, error) {
	var resp struct {
		Role struct {
			RoleName                 string `xml:"RoleName"`
			AssumeRolePolicyDocument string `xml:"AssumeRolePolicyDocument"`
		} `xml:"GetRoleResult>Role"`
	}
	_, code, err := c.call(ctx, "GetRole", map[string]string{"RoleName": name}, &resp)
	if code == "NoSuchEntity" {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	doc, err := url.QueryUnescape(resp.Role.AssumeRolePolicyDocument)
	if err != nil {
		doc = resp.Role.AssumeRolePolicyDocument
	}
	return true, doc, nil
}

func (c *Client) CreateRole(ctx context.Context, name, description, assumeRoleDoc string) (string, error) {
	var resp struct {
		Arn string `xml:"CreateRoleResult>Role>Arn"`
	}
	_, _, err := c.call(ctx, "CreateRole", map[string]string{
		"RoleName":                 name,
		"Description":              description,
		"AssumeRolePolicyDocument": assumeRoleDoc,
	}, &resp)
	return resp.Arn, err
}

func (c *Client) UpdateAssumeRolePolicy(ctx context.Context, name, assumeRoleDoc string) error {
	_, _, err := c.call(ctx, "UpdateAssumeRolePolicy", map[string]string{
		"RoleName":       name,
		"PolicyDocument": assumeRoleDoc,
	}, nil)
	return err
}

func (c *Client) ListAttachedRolePolicies(ctx context.Context, roleName string) ([]string, error) {
	var resp struct {
		Policies []struct {
			PolicyArn string `xml:"PolicyArn"`
		} `xml:"ListAttachedRolePoliciesResult>AttachedPolicies>member"`
	}
	_, _, err := c.call(ctx, "ListAttachedRolePolicies", map[string]string{"RoleName": roleName}, &resp)
	if err != nil {
		return nil, err
	}
	var arns []string
	for _, p := range resp.Policies {
		arns = append(arns, p.PolicyArn)
	}
	return arns, nil
}

func (c *Client) AttachRolePolicy(ctx context.Context, roleName, policyARN string) error {
	_, _, err := c.call(ctx, "AttachRolePolicy", map[string]string{
		"RoleName":  roleName,
		"PolicyArn": policyARN,
	}, nil)
	return err
}

func (c *Client) GetInstanceProfile(ctx context.Context, name string) (*InstanceProfile, error) {
	var resp struct {
		Profile struct {
			InstanceProfileName string `xml:"InstanceProfileName"`
			Arn                 string `xml:"Arn"`
			Roles               []struct {
				RoleName string `xml:"RoleName"`
			} `xml:"Roles>member"`
		} `xml:"GetInstanceProfileResult>InstanceProfile"`
	}
	_, code, err := c.call(ctx, "GetInstanceProfile", map[string]string{"InstanceProfileName": name}, &resp)
	if code == "NoSuchEntity" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prof := &InstanceProfile{
		Name: resp.Profile.InstanceProfileName,
		ARN:  resp.Profile.Arn,
	}
	for _, r := range resp.Profile.Roles {
		prof.RoleNames = append(prof.RoleNames, r.RoleName)
	}
	return prof, nil
}

func (c *Client) CreateInstanceProfile(ctx context.Context, name string) (string, error) {
	var resp struct {
		Arn string `xml:"CreateInstanceProfileResult>InstanceProfile>Arn"`
	}
	_, _, err := c.call(ctx, "CreateInstanceProfile", map[string]string{"InstanceProfileName": name}, &resp)
	return resp.Arn, err
}

func (c *Client) AddRoleToInstanceProfile(ctx context.Context, profileName, roleName string) error {
	_, _, err := c.call(ctx, "AddRoleToInstanceProfile", map[string]string{
		"InstanceProfileName": profileName,
		"RoleName":            roleName,
	}, nil)
	return err
}
