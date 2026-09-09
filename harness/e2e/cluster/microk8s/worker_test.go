package microk8s

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type observedNode struct {
	rig.Node
	exec func([]string) ([]byte, error)
}

func (n observedNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	return n.exec(args)
}

type observedRig struct {
	rig.Rig
	node rig.Node
}

func (r observedRig) Node(string) rig.Node { return r.node }

func nativeFixture(t *testing.T) (snap, server string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/microk8s-1.34.9.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ SnapList, KubeletServer string }
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.SnapList, f.KubeletServer
}

func TestVerifyWorkerAgainstObservedSnap(t *testing.T) {
	snap, _ := nativeFixture(t)
	for _, tc := range []struct {
		name, output       string
		snapErr, socketErr error
		wantErr            bool
	}{
		{name: "native", output: snap},
		{name: "wrong revision", output: strings.Replace(snap, "9063", "9064", 1), wantErr: true},
		{name: "wrong version", output: strings.Replace(snap, "v1.34.9", "v1.35.0", 1), wantErr: true},
		{name: "wrong snap", output: strings.ReplaceAll(snap, "microk8s", "another"), wantErr: true},
		{name: "empty output", wantErr: true},
		{name: "incomplete row", output: "Name Version Rev\nmicrok8s v1.34.9\n", wantErr: true},
		{name: "extra row", output: snap + "unexpected\n", wantErr: true},
		{name: "snap failed", snapErr: errors.New("snap unavailable"), wantErr: true},
		{name: "CRI absent", output: snap, socketErr: errors.New("socket absent"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n := observedNode{exec: func(args []string) ([]byte, error) {
				calls++
				switch calls {
				case 1:
					if !reflect.DeepEqual(args, []string{"snap", "list", "microk8s"}) {
						t.Fatalf("unexpected version observation: %v", args)
					}
					return []byte(tc.output), tc.snapErr
				case 2:
					if !reflect.DeepEqual(args, []string{"test", "-S", "/var/snap/microk8s/common/run/containerd.sock"}) {
						t.Fatalf("wrong native CRI socket: %v", args)
					}
					return nil, tc.socketErr
				default:
					t.Fatal("unexpected guest mutation or retry")
					return nil, nil
				}
			}}
			if err := (Builder{}).VerifyWorker(context.Background(), n); (err != nil) != tc.wantErr {
				t.Fatalf("VerifyWorker() = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && calls != 2 {
				t.Fatal("accepted worker without checking the native socket")
			}
		})
	}
}

func TestKubeletInvariantAgainstObservedLocalAPI(t *testing.T) {
	_, server := nativeFixture(t)
	// Only the server line is a native projection. Context names and the
	// enclosing kubeconfig below are synthetic and contain no credentials.
	config := func(endpoint string) string {
		return "apiVersion: v1\nkind: Config\ncurrent-context: worker\ncontexts:\n- name: worker\n  context:\n    cluster: local\nclusters:\n- name: local\n  cluster:\n    server: " + endpoint + "\n"
	}
	native := config(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(server), "server:")))
	for _, tc := range []struct {
		name, output string
		err          error
		wantErr      bool
	}{
		{name: "observed endpoint", output: native},
		{name: "localhost", output: config("https://localhost:16443")},
		{name: "remote API", output: config("https://10.10.0.10:16443"), wantErr: true},
		{name: "wrong local port", output: config("https://127.0.0.1:6443"), wantErr: true},
		{name: "port prefix", output: config("https://127.0.0.1:164430"), wantErr: true},
		{name: "URL suffix", output: config("https://127.0.0.1:16443/other"), wantErr: true},
		{name: "comment match", output: config("https://10.10.0.10:16443") + "# https://127.0.0.1:16443\n", wantErr: true},
		{name: "inactive local endpoint", output: config("https://10.10.0.10:16443") + "- name: unused\n  cluster:\n    server: https://127.0.0.1:16443\n", wantErr: true},
		{name: "missing current context", output: strings.Replace(native, "current-context: worker", "current-context: absent", 1), wantErr: true},
		{name: "missing active cluster", output: strings.Replace(native, "cluster: local", "cluster: absent", 1), wantErr: true},
		{name: "malformed secret-bearing config", output: "users: [SENSITIVE-TEST-MARKER\n", wantErr: true},
		{name: "missing config", err: errors.New("file absent"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n := observedNode{exec: func(args []string) ([]byte, error) {
				calls++
				if !reflect.DeepEqual(args, []string{"cat", "/var/snap/microk8s/current/credentials/kubelet.config"}) {
					t.Fatalf("wrong native kubelet config: %v", args)
				}
				return []byte(tc.output), tc.err
			}}
			topo := lab.Default()
			err := (Builder{}).KubeletInvariant(context.Background(), cluster.Deps{Topology: topo, Rig: observedRig{node: n}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("KubeletInvariant() = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "SENSITIVE-TEST-MARKER") {
				t.Fatal("config decoding failure exposed credentials")
			}
			if !tc.wantErr && calls != len(cluster.SiteNodes(topo)) {
				t.Fatal("did not check every site node")
			}
		})
	}
}

func TestKubeletInvariantNativeControlPlaneAndWorkerConfigs(t *testing.T) {
	raw, err := os.ReadFile("testdata/microk8s-1.34.9-kubelet.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]struct{ Config json.RawMessage }
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cp", "worker"} {
		t.Run(name, func(t *testing.T) {
			config := fixtures[name].Config
			if len(config) == 0 {
				t.Fatal("missing native config")
			}
			n := observedNode{exec: func([]string) ([]byte, error) { return config, nil }}
			topo := lab.Default()
			topo.Nodes = []lab.Node{topo.MustNode("cp")}
			if err := (Builder{}).KubeletInvariant(context.Background(), cluster.Deps{Topology: topo, Rig: observedRig{node: n}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
