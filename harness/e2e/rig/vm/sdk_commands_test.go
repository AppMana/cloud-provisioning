package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
	labv1 "github.com/appmana/labcontainers/api/v1"
	labclient "github.com/appmana/labcontainers/pkg/client"
	"google.golang.org/grpc"
)

type commandSessionRuntime struct {
	missingSessionRuntime
	socket string
	kind   string
}

func (r *commandSessionRuntime) Open(ctx context.Context, _, kind string) (*labclient.Client, *labclient.Session, error) {
	if kind != r.kind {
		return nil, nil, fmt.Errorf("wrong parent session kind %s, want %s", kind, r.kind)
	}
	c, err := labclient.Dial(ctx, r.socket)
	if err != nil {
		return nil, nil, err
	}
	s, err := c.Resume(ctx, "owned")
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, s, nil
}

type commandSessionServer struct {
	labv1.UnimplementedLabcontainersServer
	mu    sync.Mutex
	execs []*labv1.ExecRequest
	put   *labv1.PutRequest
}

func (s *commandSessionServer) GetSession(context.Context, *labv1.SessionRef) (*labv1.Session, error) {
	return &labv1.Session{Id: "owned", ResumeToken: "token"}, nil
}
func (s *commandSessionServer) DestroySession(_ context.Context, r *labv1.DestroySessionRequest) (*labv1.Empty, error) {
	if !r.PreserveKept {
		return nil, errors.New("command client tried to destroy its lab")
	}
	return &labv1.Empty{}, nil
}
func (s *commandSessionServer) Exec(_ context.Context, r *labv1.ExecRequest) (*labv1.ExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs = append(s.execs, r)
	if r.Argv[0] == "fail" {
		return &labv1.ExecResponse{ExitCode: 7, Stdout: []byte("partial"), Stderr: []byte("diagnostic")}, nil
	}
	return &labv1.ExecResponse{Stdout: []byte("output")}, nil
}
func (s *commandSessionServer) Put(_ context.Context, r *labv1.PutRequest) (*labv1.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put = r
	return &labv1.Empty{}, nil
}

func TestSDKGuestCommandsPreserveNativeRequests(t *testing.T) {
	for _, mode := range []string{"vm", "vm-appliance", "container"} {
		t.Run(mode, func(t *testing.T) { testSDKCommands(t, mode) })
	}
}

func testSDKCommands(t *testing.T, mode string) {
	socket := filepath.Join(t.TempDir(), "s.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	service := &commandSessionServer{}
	labv1.RegisterLabcontainersServer(server, service)
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	r := New(lab.Default(), t.TempDir())
	r.Runtime = &commandSessionRuntime{socket: socket, kind: r.Kind()}
	r.Run = func(context.Context, io.Reader, ...string) ([]byte, []byte, int, error) {
		t.Fatal("SDK guest control fell back to host CLI")
		return nil, nil, 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	node := r.Node("remote1")
	if mode == "vm-appliance" {
		node = r.Node("bastion")
	}
	if mode == "container" {
		containers := container.New(r.Topology, r.WorkDir)
		containers.Runtime = &commandSessionRuntime{socket: socket, kind: containers.Kind()}
		containers.Run = container.Runner(r.Run)
		node = containers.Node("bastion")
	}
	argv := []string{"tool", "space and ; shell characters"}
	out, err := node.Pipe(ctx, strings.NewReader("input\x00bytes"), argv...)
	if err != nil || string(out) != "output" {
		t.Fatalf("pipe: %q %v", out, err)
	}
	out, err = node.Exec(ctx, "fail")
	var exit *rig.ExitError
	if !errors.As(err, &exit) || exit.Code != 7 || string(out) != "partial" || string(exit.Stderr) != "diagnostic" {
		t.Fatalf("exit: %q %v", out, err)
	}
	if err := node.Put(ctx, strings.NewReader("file\x00bytes"), "/tmp/guest file", 0600); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	first := service.execs[0]
	if first.Node.SessionId != "owned" || first.Node.Node != node.Name() || !reflect.DeepEqual(first.Argv, argv) || string(first.Stdin) != "input\x00bytes" {
		t.Fatalf("request changed: %v", first)
	}
	if service.put == nil || service.put.Path != "/tmp/guest file" || service.put.Mode != 0600 || string(service.put.Content) != "file\x00bytes" {
		t.Fatalf("put changed: %v", service.put)
	}
}
