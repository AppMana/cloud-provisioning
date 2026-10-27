package aws

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ArtifactURLLifetime is how long a signed artifact URL works: as long as
// the join token in the same bootstrap document, past which the document
// cannot join a node anyway.
const ArtifactURLLifetime = 2 * time.Hour

// SignS3 signs s3://bucket/key artifact sources in a region for
// ArtifactURLLifetime. A URL stops working when the credentials that signed
// it expire, so sign with credentials that outlive it: a role session
// shorter than the lifetime ends the URL early.
func (s *SDK) SignS3(region string) func(ctx context.Context, source string) (string, error) {
	return func(ctx context.Context, source string) (string, error) {
		u, err := url.Parse(source)
		if err != nil || u.Scheme != "s3" || u.Host == "" || strings.Trim(u.Path, "/") == "" || u.RawQuery != "" {
			return "", fmt.Errorf("artifact source %q is not s3://bucket/key", source)
		}
		if region == "" {
			return "", fmt.Errorf("an s3:// artifact source needs the bucket's region")
		}
		cfg, err := s.config(ctx, region)
		if err != nil {
			return "", err
		}
		request, err := s3.NewPresignClient(s3.NewFromConfig(cfg)).PresignGetObject(ctx,
			&s3.GetObjectInput{Bucket: &u.Host, Key: ptr(strings.TrimPrefix(u.Path, "/"))},
			s3.WithPresignExpires(ArtifactURLLifetime))
		if err != nil {
			return "", fmt.Errorf("signing %s: %w", source, err)
		}
		return request.URL, nil
	}
}

func ptr[T any](v T) *T { return &v }
