// Package flintlock is a small client for the flintlock microVM gRPC API.
package flintlock

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

type Client struct {
	conn      *grpc.ClientConn
	api       mvmv1.MicroVMClient
	namespace string

	// OnError, if set, is called with the RPC name and the error of every
	// failed call, each failed try of a retried listing included (the daemon
	// counts them). Set it before the first call.
	OnError func(op string, err error)

	// listed, when given, is what every listing answers instead of a call:
	// see Listed. A flag, because an empty listing is a listing too.
	listed []*types.MicroVM
	given  bool
}

func (c *Client) failed(op string, err error) {
	if err != nil && c.OnError != nil {
		c.OnError(op, err)
	}
}

// basicAuth sends flintlockd's --basic-auth-token as "authorization: basic base64(token)".
type basicAuth string

func (t basicAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "basic " + base64.StdEncoding.EncodeToString([]byte(t))}, nil
}

// Plaintext stays possible for flintlockd's `insecure: true` on localhost;
// with flintlock.tls_ca_file the token only goes to a verified flintlockd.
func (basicAuth) RequireTransportSecurity() bool { return false }

func Dial(cfg config.Flintlock) (*Client, error) {
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading flintlock token: %w", err)
	}
	creds := insecure.NewCredentials()
	if cfg.TLSCAFile != "" {
		tc, err := tlsConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("flintlock TLS: %w", err)
		}
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(basicAuth(strings.TrimSpace(string(token)))),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to flintlock at %s: %w", cfg.Endpoint, err)
	}
	return newClient(conn, cfg.Namespace), nil
}

// tlsConfig trusts only flintlock.tls_ca_file for flintlockd's certificate
// and presents the client certificate, if any.
func tlsConfig(cfg config.Flintlock) (*tls.Config, error) {
	ca, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificate in " + cfg.TLSCAFile)
	}
	tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cfg.TLSCertFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{pair}
	}
	return tc, nil
}

// newClient wraps an established connection; split from Dial so tests can
// hand in an in-memory (bufconn) connection.
func newClient(conn *grpc.ClientConn, namespace string) *Client {
	return &Client{conn: conn, api: mvmv1.NewMicroVMClient(conn), namespace: namespace}
}

func (c *Client) Close() error { return c.conn.Close() }

// Create submits the spec and returns the UID flintlock assigned to the microVM.
func (c *Client) Create(ctx context.Context, spec *types.MicroVMSpec) (string, error) {
	spec.Namespace = c.namespace
	resp, err := c.api.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{Microvm: spec})
	if err != nil {
		c.failed("CreateMicroVM", err)
		return "", err
	}
	if resp.GetMicrovm().GetSpec().GetUid() == "" {
		return "", fmt.Errorf("flintlock returned no uid for %s", spec.Id)
	}
	return resp.GetMicrovm().GetSpec().GetUid(), nil
}

func (c *Client) Delete(ctx context.Context, uid string) error {
	_, err := c.api.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid})
	c.failed("DeleteMicroVM", err)
	return err
}

// List returns every microVM in the namespace. flintlockd can fail a list
// while it garbage-collects the spec of a VM being deleted ("failed reading
// from content store"); that is transient, so it is retried.
func (c *Client) List(ctx context.Context) ([]*types.MicroVM, error) {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		var vms []*types.MicroVM
		if vms, err = c.ListOnce(ctx); err == nil {
			return vms, nil
		}
		if !IsTransient(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 300 * time.Millisecond):
		}
	}
	return nil, err
}

// ListOnce is one try of List: a transient error is returned, not waited out.
// It is for callers holding a lock that other processes wait for (the memory
// admission): they give the lock back and ask again later instead.
func (c *Client) ListOnce(ctx context.Context) ([]*types.MicroVM, error) {
	if c.given {
		return c.listed, nil
	}
	resp, err := c.api.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: c.namespace})
	if err != nil {
		c.failed("ListMicroVMs", err)
		return nil, err
	}
	return resp.GetMicrovm(), nil
}

// Listed returns a client whose List, ListOnce and Find answer vms, a listing
// the caller already took, without asking flintlockd again. Its other calls go
// to flintlockd over c's connection, which only c closes. The memory
// admission hands it to a caller's fits check, which must not list a second
// time while the admission lock is held.
func (c *Client) Listed(vms []*types.MicroVM) *Client {
	given := *c
	given.listed, given.given = vms, true
	return &given
}

// IsTransient reports errors flintlockd returns while a concurrent delete is in progress.
func IsTransient(err error) bool {
	return err != nil && strings.Contains(err.Error(), "failed reading from content store")
}

// Find returns the microVM whose id or uid matches ref.
func (c *Client) Find(ctx context.Context, ref string) (*types.MicroVM, error) {
	vms, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		if vm.GetSpec().GetId() == ref || vm.GetSpec().GetUid() == ref {
			return vm, nil
		}
	}
	return nil, fmt.Errorf("no microVM %q in namespace %s", ref, c.namespace)
}
