package awspod

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Stale Calico BGP addresses must block native allocation publication. The
// synthetic EC2 response exercises both admission and the post-read recheck;
// this does not establish CNI installation or live BGP behavior.
func TestCalicoBGPAddressBlocksNativeAllocation(t *testing.T) {
	for _, key := range []string{"projectcalico.org/IPv4Address", "projectcalico.org/IPv6Address"} {
		for _, late := range []bool{false, true} {
			name := key + "/before-read"
			if late {
				name = key + "/during-read"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				o, m, api := fixture(t)
				c := o.Reader.(client.Client)
				write := func(value string) {
					t.Helper()
					n := &corev1.Node{}
					if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, n); err != nil {
						t.Fatal(err)
					}
					if n.Annotations == nil {
						n.Annotations = map[string]string{}
					}
					if value == "" {
						delete(n.Annotations, key)
					} else {
						n.Annotations[key] = value
					}
					if err := c.Update(ctx, n); err != nil {
						t.Fatal(err)
					}
				}
				value := "10.100.0.128/32"
				if strings.Contains(key, "IPv6") {
					value = "fd00:10:100::128/128"
				}
				if late {
					api.afterRead = func() { write(value) }
				} else {
					write(value)
				}
				got, err := o.AllocationsFor(ctx, m, "worker")
				if err == nil || got != nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("conflicting CNI ownership accepted: %+v %v", got, err)
				}
				wantCalls := 0
				if late {
					wantCalls = 1
				}
				if api.calls != wantCalls {
					t.Fatalf("EC2 reads=%d, want %d", api.calls, wantCalls)
				}
				n := &corev1.Node{}
				if err := c.Get(ctx, client.ObjectKey{Name: "worker"}, n); err != nil {
					t.Fatal(err)
				}
				if n.Annotations[key] != value {
					t.Fatal("observer removed foreign CNI configuration")
				}
				api.afterRead = nil
				write("")
				got, err = o.AllocationsFor(ctx, m, "worker")
				if err != nil || got == nil || len(got.PodPrefixes) != 1 {
					t.Fatalf("corrected ownership did not recover: %+v %v", got, err)
				}
			})
		}
	}
}
