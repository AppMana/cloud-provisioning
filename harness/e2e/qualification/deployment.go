package qualification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DeploymentBundle is the artifact of the release/immutable-artifacts row.
// Paths are relative to the evidence directory, not the bundle's directory.
// Its producer must retain the rendered manifests actually qualified at each site.
type DeploymentBundle struct {
	Candidate string             `json:"candidate"`
	Targets   []DeploymentTarget `json:"targets"`
}

type DeploymentTarget struct {
	Site          string `json:"site"`
	KubeSystemUID string `json:"kubeSystemUID"`
	Manifest      string `json:"manifest"`
	SHA256        string `json:"sha256"`
}

// PreparedDeployment holds a private snapshot of verified manifest bytes.
// Callers cannot substitute bytes between the dry run and application.
type PreparedDeployment struct {
	candidate string
	target    DeploymentTarget
	manifest  []byte
}

// PrepareDeployment makes no cluster requests. No deployment can be prepared
// unless every qualification row passes and both target manifests match the
// hashes in the verified release artifact. Evidence producers remain trusted.
func PrepareDeployment(path, candidate, site string) (*PreparedDeployment, error) {
	if !commit.MatchString(candidate) {
		return nil, fmt.Errorf("expected candidate must be a full Git commit")
	}
	if site != "site-a" && site != "site-b" {
		return nil, fmt.Errorf("site must be site-a or site-b")
	}
	e, failures, err := VerifyFile(path)
	if err != nil {
		return nil, err
	}
	if e.Candidate != candidate {
		return nil, fmt.Errorf("evidence candidate does not match requested candidate")
	}
	if len(failures) != 0 {
		return nil, fmt.Errorf("qualification failed: %s", strings.Join(failures, "; "))
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var release Row
	for _, r := range e.Rows {
		if r.ID == "release/immutable-artifacts" {
			release = r
		}
	}
	// Recheck the digest against the bytes decoded, closing the gap between
	// VerifyFile's read and this read of the deployment instructions.
	raw, err := readVerified(root, release.Artifact, release.SHA256, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("release bundle: %w", err)
	}
	var bundle DeploymentBundle
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing bundle JSON")
	}
	if bundle.Candidate != candidate {
		return nil, fmt.Errorf("bundle candidate mismatch")
	}
	if len(bundle.Targets) != 2 {
		return nil, fmt.Errorf("bundle requires exactly two site targets")
	}
	seen := map[string]bool{}
	identities := map[string]bool{}
	var result *PreparedDeployment
	for _, target := range bundle.Targets {
		if (target.Site != "site-a" && target.Site != "site-b") || seen[target.Site] {
			return nil, fmt.Errorf("invalid or duplicate target site")
		}
		seen[target.Site] = true
		if strings.TrimSpace(target.KubeSystemUID) == "" || identities[target.KubeSystemUID] {
			return nil, fmt.Errorf("distinct nonempty cluster identities required")
		}
		identities[target.KubeSystemUID] = true
		manifest, err := readVerified(root, target.Manifest, target.SHA256, 64<<20)
		if err != nil {
			return nil, fmt.Errorf("%s manifest: %w", target.Site, err)
		}
		if len(bytes.TrimSpace(manifest)) == 0 {
			return nil, fmt.Errorf("%s manifest is empty", target.Site)
		}
		if err := CheckManifestPins(manifest); err != nil {
			return nil, fmt.Errorf("%s manifest: %w", target.Site, err)
		}
		if target.Site == site {
			result = &PreparedDeployment{candidate: candidate, target: target, manifest: manifest}
		}
	}
	return result, nil
}

func readVerified(root *os.Root, path, expected string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(path) || !digest.MatchString(expected) {
		return nil, fmt.Errorf("local path and SHA-256 required")
	}
	f, err := openRegular(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		return nil, fmt.Errorf("digest mismatch")
	}
	return data, nil
}

// Kubectl runs one bounded kubectl command. Manifest bytes are supplied on stdin.
// Implementations must not retry mutating commands after ambiguous failures.
type Kubectl func(args []string, stdin io.Reader) ([]byte, error)

// Execute always checks cluster identity and performs a server dry run before
// optional application. It does not prove rollout health or arm rollback guards.
func (p *PreparedDeployment) Execute(kubeconfig, context string, apply bool, run Kubectl) error {
	if p == nil || len(p.manifest) == 0 {
		return fmt.Errorf("deployment was not prepared")
	}
	if kubeconfig == "" || context == "" {
		return fmt.Errorf("explicit kubeconfig and context required")
	}
	base := []string{"--kubeconfig", kubeconfig, "--context", context, "--request-timeout=60s"}
	checkIdentity := func() error {
		args := append(append([]string{}, base...), "get", "namespace", "kube-system", "-o", "json")
		raw, err := run(args, nil)
		if err != nil {
			return fmt.Errorf("cluster identity: %w", err)
		}
		var ns struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &ns); err != nil {
			return fmt.Errorf("cluster identity: %w", err)
		}
		if ns.Metadata.UID != p.target.KubeSystemUID {
			return fmt.Errorf("cluster identity mismatch for %s", p.target.Site)
		}
		return nil
	}
	if err := checkIdentity(); err != nil {
		return err
	}
	args := append(append([]string{}, base...), "apply", "--dry-run=server", "-f", "-")
	if _, err := run(args, bytes.NewReader(p.manifest)); err != nil {
		return fmt.Errorf("server dry run: %w", err)
	}
	if !apply {
		return nil
	}
	if err := checkIdentity(); err != nil {
		return err
	}
	args = append(append([]string{}, base...), "apply", "-f", "-")
	if _, err := run(args, bytes.NewReader(p.manifest)); err != nil {
		return fmt.Errorf("apply failed or incomplete; inspect cluster state before another attempt: %w", err)
	}
	return nil
}
