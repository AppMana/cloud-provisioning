package join

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/render"
)

// BootstrapRenderer owns the native guest payload, independently of the
// distribution's join credentials and the infrastructure's transport envelope.
// Infrastructure providers (such as CAPA) consume the returned format.
type BootstrapRenderer interface {
	Render(map[string]any) (bootstrap.Data, error)
}

type PatternBootstrap struct{ Path, Format string }

func (p PatternBootstrap) Render(values map[string]any) (bootstrap.Data, error) {
	body, err := render.Pattern(p.Path, values)
	if err != nil {
		return bootstrap.Data{}, err
	}
	data := bootstrap.Data{Format: p.Format, Value: []byte(body)}
	if err = data.Validate(); err != nil {
		return bootstrap.Data{}, err
	}
	return data, nil
}

func (r *Reconciler) bootstrapRenderer(guest string) (BootstrapRenderer, error) {
	renderer := r.BootstrapRenderers[guest]
	if renderer == nil {
		return nil, fmt.Errorf("no bootstrap renderer registered for guest OS %q", guest)
	}
	return renderer, nil
}
