package tuple

import (
	"fmt"
	"strings"
)

// SiteProfile describes the on-prem / management cluster's distribution and CNI.
// When omitted in configuration, both are auto-detected from the live cluster
// via discover.Distro and cni.Detect.
type SiteProfile struct {
	Distribution  string `json:"distribution,omitempty"`
	CNI           string `json:"cni,omitempty"`
	Encapsulation string `json:"encapsulation,omitempty"`
	GuestOS       string `json:"guestOS,omitempty"`
}

// TargetConfig describes a cloud CAPI target provider and its pod-networking
// and guest-OS specialization within a cluster. A single cluster can run
// multiple TargetConfigs simultaneously (for example, AWS via CAPA and GCP
// via CAPG in the same cluster).
type TargetConfig struct {
	Provider         string `json:"provider"`
	CloudCNI         string `json:"cloudCNI,omitempty"`
	GuestOS          string `json:"guestOS,omitempty"`
	ManagerNamespace string `json:"managerNamespace,omitempty"`
}

// ClusterTargetConfig pairs the cluster's single SiteProfile with 1..N cloud
// CAPI TargetConfigs.
type ClusterTargetConfig struct {
	Site    SiteProfile    `json:"site"`
	Targets []TargetConfig `json:"targets"`
}

// DefaultManagerNamespace returns the conventional CAPI infrastructure provider
// manager namespace for a provider name.
func DefaultManagerNamespace(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws":
		return "capa-system"
	case "gcp":
		return "capg-system"
	case "docker":
		return "capd-system"
	default:
		return ""
	}
}

// ParseTargetConfig parses a single target config segment such as:
//   - "aws"
//   - "aws,aws-vpc"
//   - "aws,site,windows"
//   - "gcp,site"
//   - "docker"
func ParseTargetConfig(raw string, defaultGuestOS string) (TargetConfig, error) {
	tokens := splitTokens(raw)
	if len(tokens) == 0 {
		return TargetConfig{}, fmt.Errorf("target config %q is empty", raw)
	}
	guestOS := defaultGuestOS
	if guestOS == "" {
		guestOS = "mixed"
	}
	tc := TargetConfig{
		Provider: tokens[0],
		GuestOS:  guestOS,
	}
	for _, tok := range tokens[1:] {
		switch tok {
		case "aws-vpc", "awsvpc":
			tc.CloudCNI = "aws-vpc"
		case "site":
			tc.CloudCNI = "site"
		case "linux", "windows", "mixed":
			tc.GuestOS = tok
		default:
			if strings.HasPrefix(tok, "ns=") || strings.HasPrefix(tok, "namespace=") {
				_, ns, _ := strings.Cut(tok, "=")
				tc.ManagerNamespace = strings.TrimSpace(ns)
				continue
			}
			return TargetConfig{}, fmt.Errorf("unknown provider/cloud-CNI qualifier %q in target config %q", tok, raw)
		}
	}
	if tc.CloudCNI == "" {
		if tc.Provider == "aws" {
			tc.CloudCNI = "aws-vpc"
		} else {
			tc.CloudCNI = "site"
		}
	}
	if tc.ManagerNamespace == "" {
		tc.ManagerNamespace = DefaultManagerNamespace(tc.Provider)
	}
	return tc, tc.Validate()
}

// Validate checks a single TargetConfig independently of the site profile.
func (tc TargetConfig) Validate() error {
	switch tc.Provider {
	case "aws", "gcp", "docker", "labcontainers":
	default:
		return fmt.Errorf("unsupported cloud provider %q (supported: aws, gcp, docker, labcontainers)", tc.Provider)
	}
	switch tc.GuestOS {
	case "", "linux", "windows", "mixed":
	default:
		return fmt.Errorf("unsupported guest OS %q (supported: linux, windows, mixed)", tc.GuestOS)
	}
	cloudCNI := tc.CloudCNI
	if cloudCNI == "" {
		if tc.Provider == "aws" {
			cloudCNI = "aws-vpc"
		} else {
			cloudCNI = "site"
		}
	}
	if cloudCNI != "aws-vpc" && cloudCNI != "site" {
		return fmt.Errorf("unsupported cloud CNI %q (supported: aws-vpc, site)", cloudCNI)
	}
	if cloudCNI == "aws-vpc" && tc.Provider != "aws" {
		return fmt.Errorf("cloud CNI aws-vpc requires provider aws (got %q)", tc.Provider)
	}
	return nil
}

// String renders the canonical target config token string (e.g. "aws,aws-vpc" or "gcp,site").
func (tc TargetConfig) String() string {
	provPart := tc.Provider
	if tc.CloudCNI != "" {
		provPart += "," + tc.CloudCNI
	}
	if tc.GuestOS != "" && tc.GuestOS != "mixed" {
		provPart += "," + tc.GuestOS
	}
	return provPart
}

// ParseClusterTargetConfig parses either:
//  1. A target-only expression (1..N targets separated by ';' or '+'):
//     e.g. "aws,aws-vpc" or "aws,aws-vpc; gcp,site"
//  2. A full site + target(s) expression:
//     e.g. "k0s + calico,bgp + aws,aws-vpc; gcp,site"
func ParseClusterTargetConfig(raw string) (ClusterTargetConfig, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ClusterTargetConfig{}, fmt.Errorf("target config expression is empty")
	}
	parts := strings.Split(trimmed, "+")
	if len(parts) == 3 {
		dist := strings.ToLower(strings.TrimSpace(parts[0]))
		cniTokens := splitTokens(parts[1])
		if dist == "" || len(cniTokens) == 0 {
			return ClusterTargetConfig{}, fmt.Errorf("target config %q contains an empty site segment", raw)
		}
		site := SiteProfile{
			Distribution: dist,
			CNI:          cniTokens[0],
			GuestOS:      "mixed",
		}
		for _, tok := range cniTokens[1:] {
			switch tok {
			case "bgp", "bird", "native":
				site.Encapsulation = "bgp"
			case "vxlan", "ipip", "wireguard":
				site.Encapsulation = tok
			case "linux", "windows", "mixed":
				site.GuestOS = tok
			default:
				return ClusterTargetConfig{}, fmt.Errorf("unknown CNI/OS qualifier %q in %q", tok, raw)
			}
		}
		if site.Encapsulation == "" && site.CNI == "calico" {
			site.Encapsulation = "bgp"
		}
		targets, err := parseTargetList(parts[2], site.GuestOS)
		if err != nil {
			return ClusterTargetConfig{}, err
		}
		cfg := ClusterTargetConfig{Site: site, Targets: targets}
		return cfg, cfg.Validate()
	}
	if len(parts) == 1 {
		targets, err := parseTargetList(parts[0], "mixed")
		if err != nil {
			return ClusterTargetConfig{}, err
		}
		cfg := ClusterTargetConfig{Targets: targets}
		return cfg, cfg.Validate()
	}
	return ClusterTargetConfig{}, fmt.Errorf("target config %q must be either '<target>[;<target>...]' (e.g. \"aws,aws-vpc;gcp,site\") or '<distro> + <cni[,mode]> + <target>[;<target>...]' (e.g. \"k0s + calico,bgp + aws;gcp\")", raw)
}

func parseTargetList(raw string, defaultGuestOS string) ([]TargetConfig, error) {
	segments := strings.Split(raw, ";")
	var out []TargetConfig
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		tc, err := ParseTargetConfig(seg, defaultGuestOS)
		if err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no target configs specified in %q", raw)
	}
	return out, nil
}

// Validate checks the ClusterTargetConfig across all configured cloud targets.
func (c ClusterTargetConfig) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("at least one target config is required")
	}
	for _, t := range c.Targets {
		if err := t.Validate(); err != nil {
			return err
		}
		if c.Site.Distribution != "" && c.Site.CNI != "" {
			tup := Tuple{
				Distribution:  c.Site.Distribution,
				CNI:           c.Site.CNI,
				Encapsulation: c.Site.Encapsulation,
				Provider:      t.Provider,
				GuestOS:       t.GuestOS,
				CloudCNI:      t.CloudCNI,
			}
			if err := tup.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Tuples expands a ClusterTargetConfig into one Tuple per target provider
// (using defaults "k0s" and "calico,bgp" when Site is left for runtime auto-detection).
func (c ClusterTargetConfig) Tuples() []Tuple {
	dist := c.Site.Distribution
	if dist == "" {
		dist = "k0s"
	}
	cniName := c.Site.CNI
	if cniName == "" {
		cniName = "calico"
	}
	encap := c.Site.Encapsulation
	if encap == "" && cniName == "calico" {
		encap = "bgp"
	}
	out := make([]Tuple, 0, len(c.Targets))
	for _, t := range c.Targets {
		out = append(out, Tuple{
			Distribution:  dist,
			CNI:           cniName,
			Encapsulation: encap,
			Provider:      t.Provider,
			GuestOS:       t.GuestOS,
			CloudCNI:      t.CloudCNI,
		})
	}
	return out
}

// TargetForProvider returns the configured TargetConfig for provider, if present.
func (c ClusterTargetConfig) TargetForProvider(provider string) (TargetConfig, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, t := range c.Targets {
		if t.Provider == provider {
			return t, true
		}
	}
	return TargetConfig{}, false
}

// ManagerNamespaces returns the deduplicated list of CAPI provider manager
// namespaces across all configured targets.
func (c ClusterTargetConfig) ManagerNamespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.Targets {
		if t.ManagerNamespace != "" && !seen[t.ManagerNamespace] {
			seen[t.ManagerNamespace] = true
			out = append(out, t.ManagerNamespace)
		}
	}
	return out
}
