package claim

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Delete asks the product to remove a claim, while the infrastructure provider
// reconciles instance termination. It never removes finalizers or product state.
func (c *Claimer) Delete(ctx context.Context, name, node string) error {
	return (&Remover{Kube: c.Kube, Reconcile: func(ctx context.Context) error {
		_, err := c.Provider.Reconcile(ctx, Namespace)
		return err
	}}).Delete(ctx, name, node)
}

// Remover observes cleanup through the Machine's actual infrastructure reference.
// Reconcile is optional: external providers such as CAPA run their own controllers.
type Remover struct {
	Kube      *kube.Client
	Reconcile func(context.Context) error
}

func (c *Remover) Delete(ctx context.Context, name, node string) error {
	raw, err := c.Kube.Run(ctx, "-n", Namespace, "get", "machine", name, "-o", "json")
	if err != nil {
		return err
	}
	var machine struct {
		Spec struct {
			InfrastructureRef struct{ Kind, Name, APIGroup, APIVersion string }
		}
	}
	if err := json.Unmarshal(raw, &machine); err != nil {
		return err
	}
	ref := machine.Spec.InfrastructureRef
	group := ref.APIGroup
	if group == "" {
		group = strings.Split(ref.APIVersion, "/")[0]
	}
	if ref.Kind == "" || ref.Name == "" || group == "" || node == "" {
		return fmt.Errorf("removal requires infrastructure identity and associated Node")
	}
	if _, err := c.Kube.Run(ctx, "-n", Namespace, "delete", "provisionednodeclaim", name, "--wait=false"); err != nil {
		return err
	}
	return wait.Until(ctx, 10*time.Minute, "claim removal did not clean up "+name, func(ctx context.Context) error {
		if c.Reconcile != nil {
			if err := c.Reconcile(ctx); err != nil {
				return err
			}
		}
		for _, obj := range []struct{ kind, name, namespace string }{
			{"provisionednodeclaim", name, Namespace}, {"machine", name, Namespace},
			{ref.Kind + "." + group, ref.Name, Namespace}, {"secret", name + "-bootstrap", Namespace}, {"secret", tunnel.AdoptionSecretName(name), Namespace}, {"node", node, ""},
		} {
			args := []string{"get", obj.kind, obj.name, "--ignore-not-found", "-o", "name"}
			if obj.namespace != "" {
				args = append([]string{"-n", obj.namespace}, args...)
			}
			out, err := c.Kube.Run(ctx, args...)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(out)) != "" {
				return fmt.Errorf("%s/%s still exists", obj.kind, obj.name)
			}
		}
		for _, field := range []string{"peer-endpoint-", "peer-public-key-", "peer-allowed-ips-", "peer-route-hosts-", "peer-route-host-"} {
			value, err := c.Kube.Get(ctx, Namespace, "secret", Namespace+"-peers", "{.data."+field+name+"}")
			if err != nil {
				return err
			}
			if value != "" {
				return fmt.Errorf("peer field %s%s remains", field, name)
			}
		}
		return nil
	})
}
