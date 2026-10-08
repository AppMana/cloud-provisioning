package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var ErrInvocationPending = errors.New("SSM invocation has not propagated")

// CLI reloads a private STS session for every call, permitting renewal without
// reconstructing machines. It never inherits source account credentials.
type CLI struct {
	Region      string
	SessionPath string
	// throttleBackoff is the first wait after a throttled call; zero is
	// one second. It doubles to at most 30 seconds.
	throttleBackoff time.Duration
}

func (c *CLI) environment() ([]string, error) {
	raw, err := os.ReadFile(c.SessionPath)
	if err != nil {
		return nil, fmt.Errorf("read AWS test session: %w", err)
	}
	var session struct {
		Credentials struct {
			AccessKeyID                   string `json:"AccessKeyId"`
			SecretAccessKey, SessionToken string
		}
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("invalid AWS test session JSON")
	}
	v := session.Credentials
	if v.AccessKeyID == "" || v.SecretAccessKey == "" || v.SessionToken == "" {
		return nil, fmt.Errorf("AWS test operations require a complete temporary session")
	}
	env := make([]string, 0)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "AWS_") {
			env = append(env, value)
		}
	}
	return append(env, "AWS_ACCESS_KEY_ID="+v.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+v.SecretAccessKey, "AWS_SESSION_TOKEN="+v.SessionToken, "AWS_EC2_METADATA_DISABLED=true", "AWS_PAGER="), nil
}

func (c *CLI) Call(ctx context.Context, service, operation string, input map[string]any) (json.RawMessage, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return c.run(ctx, service, operation, "--cli-input-json", string(data), "--output", "json")
}

// run calls the AWS CLI, retrying a throttled call until it goes through
// or ctx ends: SSM throttles SendCommand per account, and a row sends
// hundreds of commands.
func (c *CLI) run(ctx context.Context, service, operation string, args ...string) ([]byte, error) {
	backoff := c.throttleBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		out, err := c.runOnce(ctx, service, operation, args...)
		if !errors.Is(err, errThrottled) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("AWS %s/%s failed: ThrottlingException until %w", service, operation, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

var errThrottled = errors.New("throttled")

func (c *CLI) runOnce(ctx context.Context, service, operation string, args ...string) ([]byte, error) {
	env, err := c.environment()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "aws", append([]string{"--region", c.Region, service, operation}, args...)...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var failed *exec.ExitError
		if errors.As(err, &failed) && operation == "get-command-invocation" && strings.Contains(string(failed.Stderr), "InvocationDoesNotExist") {
			return nil, ErrInvocationPending
		}
		// AWS errors can echo credential-bearing command parameters, so
		// only the error code is reported.
		code := ""
		if errors.As(err, &failed) {
			code = errorCode(string(failed.Stderr))
		}
		if code == "ThrottlingException" || code == "Throttling" || code == "RequestLimitExceeded" {
			return nil, errThrottled
		}
		if code != "" {
			return nil, fmt.Errorf("AWS %s/%s failed: %s", service, operation, code)
		}
		return nil, fmt.Errorf("AWS %s/%s failed", service, operation)
	}
	return json.RawMessage(out), nil
}

var awsErrorCode = regexp.MustCompile(`An error occurred \(([A-Za-z0-9.]+)\)`)

// errorCode is the AWS error code in a CLI error message, or empty.
func errorCode(stderr string) string {
	if m := awsErrorCode.FindStringSubmatch(stderr); m != nil {
		return m[1]
	}
	return ""
}
