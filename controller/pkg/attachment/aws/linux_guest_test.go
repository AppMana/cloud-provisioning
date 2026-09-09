package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

type guestRead struct {
	off, wrong bool
	calls      int
}

func (e *guestRead) Read(_ context.Context, target InterfaceTarget, args []string) ([]byte, error) {
	if target.InstanceID != "i-gateway" {
		return nil, fmt.Errorf("wrong target")
	}
	e.calls++
	if args[0] == "cat" {
		if e.off {
			return []byte("0\n"), nil
		}
		return []byte("1\n"), nil
	}
	if len(args) != 9 || args[5] != "from" || args[7] != "iif" {
		return nil, fmt.Errorf("lookup must model forwarded packet: %v", args)
	}
	dev := "ens5"
	if args[8] == "ens5" {
		dev = "cldt1775f69e"
	}
	if e.wrong {
		dev = args[8]
	}
	return json.Marshal([]map[string]string{{"dst": args[4], "dev": dev}})
}
func TestLinuxGuestObservesForwardedPathsAndDoesNotMutateOnRelease(t *testing.T) {
	r, record, _ := resolverFixture(t)
	binding, err := r.Resolve(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	exec := &guestRead{}
	g := LinuxGatewayGuest{Exec: exec, NativeInterface: "ens5", TunnelInterface: "cldt1775f69e"}
	if ok, err := g.Ensure(context.Background(), record, binding); err != nil || !ok {
		t.Fatalf("observe %v %v", ok, err)
	}
	if exec.calls != 3 {
		t.Fatal("both forwarding directions not observed")
	}
	exec.off = true
	if ok, err := g.Ensure(context.Background(), record, binding); err != nil || ok {
		t.Fatal("accepted disabled forwarding")
	}
	exec.off = false
	exec.wrong = true
	if ok, err := g.Ensure(context.Background(), record, binding); err != nil || ok {
		t.Fatal("accepted route back to ingress NIC")
	}
	calls := exec.calls
	if ok, err := g.Release(context.Background(), record, binding); err != nil || !ok || exec.calls != calls {
		t.Fatal("release touched externally managed guest state")
	}
}
