// Package acceptance qualifies a real cluster for the product from a
// kubeconfig alone: no bastion, no node access, no lab.
//
// It has two modes. Preconditions is read-only by construction: its
// transport refuses every request that is not a read, so a check that
// tried to change the cluster would fail rather than change it. The
// matrix mode creates probe pods in one dedicated namespace of its own
// and deletes that namespace when it finishes.
package acceptance

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config builds a client configuration from exactly the kubeconfig file
// named, never from $KUBECONFIG, ~/.kube/config or an in-cluster
// account: a workstation's default context can be a production cluster,
// and qualifying the wrong cluster is worse than refusing to start.
// context selects a context in that file; empty uses the file's own
// current context.
func Config(kubeconfig, context string) (*rest.Config, error) {
	if strings.TrimSpace(kubeconfig) == "" {
		return nil, fmt.Errorf("an explicit kubeconfig file is required")
	}
	file, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", kubeconfig, err)
	}
	if context == "" {
		context = file.CurrentContext
	}
	if _, ok := file.Contexts[context]; !ok {
		return nil, fmt.Errorf("%s has no context %q", kubeconfig, context)
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*file, context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building a client for context %q of %s: %w", context, kubeconfig, err)
	}
	return config, nil
}

// ReadOnly returns a copy of config whose transport refuses every
// request but GET and HEAD.
func ReadOnly(config *rest.Config) *rest.Config {
	out := rest.CopyConfig(config)
	previous := out.WrapTransport
	out.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if previous != nil {
			rt = previous(rt)
		}
		return readOnlyTransport{next: rt}
	}
	return out
}

type readOnlyTransport struct{ next http.RoundTripper }

// ErrWrite is what a refused request returns.
type ErrWrite struct{ Method, URL string }

func (e ErrWrite) Error() string {
	return fmt.Sprintf("read-only client refused %s %s", e.Method, e.URL)
}

func (t readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, ErrWrite{Method: req.Method, URL: req.URL.String()}
	}
	return t.next.RoundTrip(req)
}
