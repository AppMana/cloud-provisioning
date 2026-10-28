package qualification

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/distribution/reference"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// CheckManifestPins checks explicit container images and the controller's image
// and download flags. It cannot inspect images embedded in scripts, Secrets,
// ConfigMaps or arbitrary operator configuration. Those need producer review.
func CheckManifestPins(raw []byte) error {
	reader := kyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	documents := 0
	for {
		document, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		encoded, err := yaml.YAMLToJSONStrict(document)
		if err != nil {
			return fmt.Errorf("manifest YAML: %w", err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			return err
		}
		if object == nil {
			continue
		}
		documents++
		if err := walkContainers(object); err != nil {
			return fmt.Errorf("document %d: %w", documents, err)
		}
	}
	if documents == 0 {
		return fmt.Errorf("manifest has no resources")
	}
	return nil
}

func immutableImage(value string) error {
	ref, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return fmt.Errorf("invalid image reference")
	}
	pinned, ok := ref.(reference.Digested)
	if !ok || pinned.Digest().Algorithm().String() != "sha256" || !digest.MatchString(pinned.Digest().Encoded()) {
		return fmt.Errorf("image must use a sha256 digest")
	}
	return nil
}

func walkContainers(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "containers" || key == "initContainers" || key == "ephemeralContainers" {
				// Schema definitions use maps here, rather than actual container arrays.
				if containers, ok := child.([]any); ok {
					for _, item := range containers {
						container, ok := item.(map[string]any)
						if !ok {
							return fmt.Errorf("invalid %s entry", key)
						}
						name, _ := container["name"].(string)
						image, _ := container["image"].(string)
						if err := immutableImage(image); err != nil {
							return fmt.Errorf("container %q: %w", name, err)
						}
						var argv []string
						for _, field := range []string{"command", "args"} {
							if values, ok := container[field].([]any); ok {
								for _, arg := range values {
									s, ok := arg.(string)
									if !ok {
										return fmt.Errorf("container %q: non-string argument", name)
									}
									argv = append(argv, s)
								}
							}
						}
						if err := checkArtifactFlags(argv); err != nil {
							return fmt.Errorf("container %q: %w", name, err)
						}
					}
				}
			}
			if err := walkContainers(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := walkContainers(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkArtifactFlags(argv []string) error {
	images := map[string]bool{
		"dialer-image": true, "dialer-cloud-image": true, "windows-publisher-image": true,
		"aws-vpc-cni-image": true, "aws-vpc-cni-init-image": true, "aws-windows-ipam-image": true,
	}
	urls := map[string]string{}
	for _, family := range []string{"arm64", "amd64"} {
		for _, component := range []string{"join-dialer-binary", "join-cni-plugins"} {
			urls[component+"-url-"+family] = component + "-sha256-" + family
		}
	}
	relevant := map[string]bool{}
	for flag := range images {
		relevant[flag] = true
	}
	for url, sha := range urls {
		relevant[url] = true
		relevant[sha] = true
	}
	for _, flag := range []string{"windows-tunnel-sha256", "windows-worker-sha256"} {
		relevant[flag] = true
	}
	values := map[string]string{}
	for i := 0; i < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if !relevant[name] {
			continue
		}
		if !hasValue {
			if i+1 == len(argv) || strings.HasPrefix(argv[i+1], "-") {
				return fmt.Errorf("missing value for --%s", name)
			}
			i++
			value = argv[i]
		}
		if _, exists := values[name]; exists {
			return fmt.Errorf("duplicate artifact flag --%s", name)
		}
		values[name] = value
		if images[name] && (value != "" || name == "dialer-image") {
			if err := immutableImage(value); err != nil {
				return fmt.Errorf("--%s: %w", name, err)
			}
		}
	}
	for url, sha := range urls {
		if values[url] != "" && !digest.MatchString(values[sha]) {
			return fmt.Errorf("--%s requires a canonical --%s", url, sha)
		}
	}
	for name, value := range values {
		if strings.Contains(name, "sha256") && value != "" && !digest.MatchString(value) {
			return fmt.Errorf("--%s requires a canonical SHA-256", name)
		}
	}
	return nil
}
