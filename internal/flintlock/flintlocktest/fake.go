// Package flintlocktest provides an in-memory flintlock MicroVM gRPC server
// for tests. It is imported only from _test.go files and never reaches the
// firerunner binary.
package flintlocktest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

// Token is the basic-auth token StartUnix writes to its token file.
const Token = "s3cret-token"

// Server is a fake flintlockd. The zero value is ready to use; configure it
// with the setters before issuing calls.
type Server struct {
	mvmv1.UnimplementedMicroVMServer

	mu        sync.Mutex
	vms       []*types.MicroVM
	createUID string
	listErrs  []error
	listCalls int
	created   []*types.MicroVMSpec
	deleted   []string
	auth      []string
	deletedCh chan string
	hangDel   bool
}

// NewServer returns a server whose CreateMicroVM answers with createUID.
func NewServer(createUID string) *Server {
	return &Server{createUID: createUID, deletedCh: make(chan string, 64)}
}

// SetVMs replaces what ListMicroVMs returns.
func (s *Server) SetVMs(vms ...*types.MicroVM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms = vms
}

// HangDeletes makes DeleteMicroVM block until the caller gives up, like a
// flintlockd stuck on containerd or device-mapper.
func (s *Server) HangDeletes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangDel = true
}

// FailList makes the next len(errs) ListMicroVMs calls return errs in order.
func (s *Server) FailList(errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErrs = append(s.listErrs, errs...)
}

// ListCalls reports how many ListMicroVMs calls arrived.
func (s *Server) ListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls
}

// Created returns the specs received by CreateMicroVM.
func (s *Server) Created() []*types.MicroVMSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*types.MicroVMSpec(nil), s.created...)
}

// Deleted returns the uids received by DeleteMicroVM.
func (s *Server) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// Auth returns the authorization metadata of every call received.
func (s *Server) Auth() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// WaitDeleted blocks until uid is deleted or the timeout expires; it is for
// deletes the code under test issues from its own goroutines.
func (s *Server) WaitDeleted(t testing.TB, uid string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		for _, d := range s.Deleted() {
			if d == uid {
				return
			}
		}
		select {
		case <-s.deletedCh:
		case <-deadline:
			t.Fatalf("uid %s not deleted within %s; deleted: %v", uid, timeout, s.Deleted())
		}
	}
}

// CreateMicroVM records the spec and echoes it back with the configured uid.
func (s *Server) CreateMicroVM(_ context.Context, req *mvmv1.CreateMicroVMRequest) (*mvmv1.CreateMicroVMResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, req.GetMicrovm())
	spec := req.GetMicrovm()
	spec.Uid = &s.createUID
	return &mvmv1.CreateMicroVMResponse{Microvm: &types.MicroVM{Spec: spec}}, nil
}

// DeleteMicroVM records the uid.
func (s *Server) DeleteMicroVM(ctx context.Context, req *mvmv1.DeleteMicroVMRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	hang := s.hangDel
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.mu.Lock()
	s.deleted = append(s.deleted, req.GetUid())
	s.mu.Unlock()
	select {
	case s.deletedCh <- req.GetUid():
	default: // nobody waiting; Deleted() still has it
	}
	return &emptypb.Empty{}, nil
}

// ListMicroVMs returns the next queued error, or the configured VMs.
func (s *Server) ListMicroVMs(_ context.Context, _ *mvmv1.ListMicroVMsRequest) (*mvmv1.ListMicroVMsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if len(s.listErrs) > 0 {
		err := s.listErrs[0]
		s.listErrs = s.listErrs[1:]
		return nil, err
	}
	return &mvmv1.ListMicroVMsResponse{Microvm: s.vms}, nil
}

func (s *Server) recordAuth(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.auth = append(s.auth, md.Get("authorization")...)
	s.mu.Unlock()
	return h(ctx, req)
}

func (s *Server) serve(t testing.TB, l net.Listener) {
	gs := grpc.NewServer(grpc.UnaryInterceptor(s.recordAuth))
	mvmv1.RegisterMicroVMServer(gs, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = gs.Serve(l)
	}()
	t.Cleanup(func() {
		gs.Stop()
		<-done
	})
}

// StartBufconn serves s in memory and returns a client connection to it.
func StartBufconn(t testing.TB, s *Server) *grpc.ClientConn {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	s.serve(t, l)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// StartUnix serves s on a unix socket and returns a flintlock config (with a
// token file holding Token) that flintlock.Dial can use unchanged.
func StartUnix(t testing.TB, s *Server) config.Flintlock {
	t.Helper()
	// t.TempDir paths can exceed the 104-byte sun_path limit on macOS.
	dir, err := os.MkdirTemp("", "fl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s.serve(t, l)
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte(Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Flintlock{Endpoint: "unix://" + sock, TokenFile: token, Namespace: "test"}
}
