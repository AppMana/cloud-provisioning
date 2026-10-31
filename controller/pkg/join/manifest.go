package join

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultDialerManifestPath is the multi-platform dialer manifest baked
	// into the endpoint-controller image alongside /join-patterns.
	DefaultDialerManifestPath = "/join-patterns/dialer-manifest.json"
	// DefaultReleaseRepository is the GitHub repository publishing dialer releases.
	DefaultReleaseRepository = "AppMana/cloud-provisioning"
	// DefaultLatestManifestURL is the "latest" GitHub release multi-platform manifest URL.
	DefaultLatestManifestURL = "https://github.com/AppMana/cloud-provisioning/releases/latest/download/manifest.json"
)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DialerArtifact describes a single-arch or universal multi-arch dialer binary
// in a DialerManifest.
type DialerArtifact struct {
	OS            string   `json:"os"`
	Architecture  string   `json:"architecture,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
	Artifact      string   `json:"artifact"`
	URL           string   `json:"url"`
	LatestURL     string   `json:"latestURL,omitempty"`
	SHA256        string   `json:"sha256"`
}

// Validate checks that the artifact entry has both a URL and a valid 64-hex SHA256.
func (a DialerArtifact) Validate(label string) error {
	if strings.TrimSpace(a.URL) == "" && strings.TrimSpace(a.LatestURL) == "" {
		return fmt.Errorf("%s: missing download URL", label)
	}
	sha := strings.ToLower(strings.TrimSpace(a.SHA256))
	if !sha256HexPattern.MatchString(sha) {
		return fmt.Errorf("%s: invalid sha256 %q (expected 64 lowercase hex characters)", label, a.SHA256)
	}
	return nil
}

// EffectiveURL returns the download URL for the artifact, preferring LatestURL
// when useLatest is true and LatestURL is present.
func (a DialerArtifact) EffectiveURL(useLatest bool) string {
	if useLatest && strings.TrimSpace(a.LatestURL) != "" {
		return strings.TrimSpace(a.LatestURL)
	}
	if strings.TrimSpace(a.URL) != "" {
		return strings.TrimSpace(a.URL)
	}
	return strings.TrimSpace(a.LatestURL)
}

// DialerManifest is a multi-platform / multi-arch release manifest mapping
// platforms ("linux/amd64", "linux/arm64", "windows/amd64") and an optional
// universal fat Linux binary ("universal") to their download URLs and SHA256 digests.
type DialerManifest struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Version       string                    `json:"version"`
	Repository    string                    `json:"repository,omitempty"`
	Universal     *DialerArtifact           `json:"universal,omitempty"`
	Platforms     map[string]DialerArtifact `json:"platforms"`
}

// ParseDialerManifest parses and validates a JSON multi-platform dialer manifest.
func ParseDialerManifest(raw []byte) (*DialerManifest, error) {
	var m DialerManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing dialer manifest: %w", err)
	}
	if m.Universal != nil {
		if err := m.Universal.Validate("universal"); err != nil {
			return nil, err
		}
	}
	for k, p := range m.Platforms {
		if err := p.Validate(k); err != nil {
			return nil, err
		}
	}
	if m.Universal == nil && len(m.Platforms) == 0 {
		return nil, fmt.Errorf("dialer manifest contains neither a universal binary nor platform entries")
	}
	return &m, nil
}

// LoadDialerManifest reads and parses a DialerManifest from a local file path.
func LoadDialerManifest(path string) (*DialerManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseDialerManifest(raw)
}

// FetchDialerManifest downloads and parses a DialerManifest from an HTTP/HTTPS URL
// (or resolves "latest" to DefaultLatestManifestURL).
func FetchDialerManifest(ctx context.Context, manifestURL string) (*DialerManifest, error) {
	u := strings.TrimSpace(manifestURL)
	if u == "" || u == "latest" {
		u = DefaultLatestManifestURL
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching dialer manifest %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching dialer manifest %s: HTTP %d", u, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return ParseDialerManifest(raw)
}

// ResolveDialerManifest loads a multi-platform dialer manifest.
//
// Resolution order:
//  1. If manifestURL is an explicit URL other than "" or "latest", fetch it directly.
//  2. If manifestPath (default /join-patterns/dialer-manifest.json) exists on disk, load it.
//  3. If manifestURL is "latest" (or manifestPath was absent and manifestURL != "none"),
//     fetch DefaultLatestManifestURL.
func ResolveDialerManifest(ctx context.Context, manifestPath, manifestURL string) (*DialerManifest, error) {
	urlTrimmed := strings.TrimSpace(manifestURL)
	pathTrimmed := strings.TrimSpace(manifestPath)
	if urlTrimmed != "" && urlTrimmed != "latest" && urlTrimmed != "auto" {
		return FetchDialerManifest(ctx, urlTrimmed)
	}
	if pathTrimmed != "" {
		if m, err := LoadDialerManifest(pathTrimmed); err == nil {
			return m, nil
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %s: %w", pathTrimmed, err)
		}
	}
	if urlTrimmed == "latest" || urlTrimmed == "auto" {
		return FetchDialerManifest(ctx, DefaultLatestManifestURL)
	}
	return nil, fmt.Errorf("dialer manifest not found at %q and no manifest URL configured", pathTrimmed)
}

// LinuxBinaryForArch resolves the download URL and SHA256 for a Linux architecture
// ("amd64" or "arm64"). When preferUniversal is true and the manifest includes a
// universal fat Linux binary covering that architecture, the universal binary is
// returned so a single artifact URL and SHA256 serves both architectures.
func (m *DialerManifest) LinuxBinaryForArch(arch string, preferUniversal, useLatestURL bool) (string, string, error) {
	if m == nil {
		return "", "", fmt.Errorf("nil dialer manifest")
	}
	arch = strings.ToLower(strings.TrimSpace(arch))
	if preferUniversal && m.Universal != nil && m.Universal.SupportsArch(arch) {
		return m.Universal.EffectiveURL(useLatestURL), strings.ToLower(strings.TrimSpace(m.Universal.SHA256)), nil
	}
	key := "linux/" + arch
	if entry, ok := m.Platforms[key]; ok {
		return entry.EffectiveURL(useLatestURL), strings.ToLower(strings.TrimSpace(entry.SHA256)), nil
	}
	if m.Universal != nil && m.Universal.SupportsArch(arch) {
		return m.Universal.EffectiveURL(useLatestURL), strings.ToLower(strings.TrimSpace(m.Universal.SHA256)), nil
	}
	return "", "", fmt.Errorf("dialer manifest %q has no entry for %s", m.Version, key)
}

// SupportsArch reports whether a universal artifact supports the target architecture.
func (a DialerArtifact) SupportsArch(arch string) bool {
	if len(a.Architectures) == 0 {
		return a.Architecture == "" || a.Architecture == arch
	}
	for _, candidate := range a.Architectures {
		if strings.EqualFold(strings.TrimSpace(candidate), arch) {
			return true
		}
	}
	return false
}

// WindowsTunnelSHA256 returns the SHA256 digest of the Windows tunnel binary
// ("windows/amd64") when present in the manifest, or "".
func (m *DialerManifest) WindowsTunnelSHA256() string {
	if m == nil {
		return ""
	}
	if entry, ok := m.Platforms["windows/amd64"]; ok {
		return strings.ToLower(strings.TrimSpace(entry.SHA256))
	}
	return ""
}
