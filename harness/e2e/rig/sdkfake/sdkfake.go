// Package sdkfake answers a rig's Labcontainers session commands in tests,
// so what a node asks its guest to do can be asserted without a daemon.
package sdkfake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"

	labv1 "github.com/appmana/labcontainers/api/v1"
	labclient "github.com/appmana/labcontainers/pkg/client"
	"github.com/srl-labs/containerlab/core"
	"google.golang.org/grpc"
)

// Server records every command and file write it receives. A command whose
// first word is "fail" exits 7 with partial output and a diagnostic.
type Server struct {
	labv1.UnimplementedLabcontainersServer
	mu    sync.Mutex
	execs []*labv1.ExecRequest
	put   *labv1.PutRequest
}

func (s *Server) GetSession(context.Context, *labv1.SessionRef) (*labv1.Session, error) {
	return &labv1.Session{Id: "owned", ResumeToken: "token"}, nil
}

func (s *Server) DestroySession(_ context.Context, r *labv1.DestroySessionRequest) (*labv1.Empty, error) {
	if !r.PreserveKept {
		return nil, errors.New("command client tried to destroy its lab")
	}
	return &labv1.Empty{}, nil
}

func (s *Server) Exec(_ context.Context, r *labv1.ExecRequest) (*labv1.ExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs = append(s.execs, r)
	if r.Argv[0] == "fail" {
		return &labv1.ExecResponse{ExitCode: 7, Stdout: []byte("partial"), Stderr: []byte("diagnostic")}, nil
	}
	return &labv1.ExecResponse{Stdout: []byte("output")}, nil
}

func (s *Server) Put(_ context.Context, r *labv1.PutRequest) (*labv1.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put = r
	return &labv1.Empty{}, nil
}

// Requests returns the commands and the last file write received so far.
func (s *Server) Requests() ([]*labv1.ExecRequest, *labv1.PutRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*labv1.ExecRequest(nil), s.execs...), s.put
}

// Runtime reconnects to the fake's session and never deploys or destroys.
type Runtime struct {
	Socket string
	// Kind is the parent session kind a caller must ask for.
	Kind string
}

func (r *Runtime) Open(ctx context.Context, _, kind string) (*labclient.Client, *labclient.Session, error) {
	if kind != r.Kind {
		return nil, nil, fmt.Errorf("wrong parent session kind %s, want %s", kind, r.Kind)
	}
	c, err := labclient.Dial(ctx, r.Socket)
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

func (r *Runtime) Deploy(context.Context, string, string, string, *core.Config) error {
	return errors.New("the command fake does not deploy labs")
}

func (r *Runtime) Destroy(context.Context, string, string) (bool, error) { return false, nil }

// Start serves a fake session of the given kind until the test ends.
func Start(t testing.TB, kind string) (*Runtime, *Server) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	service := &Server{}
	labv1.RegisterLabcontainersServer(server, service)
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	return &Runtime{Socket: socket, Kind: kind}, service
}
