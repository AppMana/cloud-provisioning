package aws

import (
	"context"
	"fmt"
	"sync"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// SDK builds AWS clients from the SDK's default credential chain:
// AWS_* environment variables, or the shared config and credentials files
// AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE name. A profile with
// role_arn and source_profile has the SDK assume that role and refresh it
// before it expires, so nothing outside the controller renews credentials.
type SDK struct {
	mu      sync.Mutex
	configs map[string]sdk.Config
	apis    map[string]attachmentaws.API
	account string
}

func (s *SDK) config(ctx context.Context, region string) (sdk.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg, ok := s.configs[region]; ok {
		return cfg, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region), config.WithRetryMaxAttempts(3))
	if err != nil {
		return sdk.Config{}, fmt.Errorf("loading AWS credentials: %w", err)
	}
	if s.configs == nil {
		s.configs = map[string]sdk.Config{}
	}
	s.configs[region] = cfg
	return cfg, nil
}

// API is an EC2 API in a region.
func (s *SDK) API(ctx context.Context, region string) (attachmentaws.API, error) {
	cfg, err := s.config(ctx, region)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if api, ok := s.apis[region]; ok {
		return api, nil
	}
	api, err := attachmentaws.NewEC2Transport(cfg)
	if err != nil {
		return nil, err
	}
	if s.apis == nil {
		s.apis = map[string]attachmentaws.API{}
	}
	s.apis[region] = api
	return api, nil
}

// Account is the account the credentials act in.
func (s *SDK) Account(ctx context.Context, region string) (string, error) {
	s.mu.Lock()
	account := s.account
	s.mu.Unlock()
	if account != "" {
		return account, nil
	}
	cfg, err := s.config(ctx, region)
	if err != nil {
		return "", err
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	if out.Account == nil || *out.Account == "" {
		return "", fmt.Errorf("STS named no account")
	}
	s.mu.Lock()
	s.account = *out.Account
	s.mu.Unlock()
	return *out.Account, nil
}
