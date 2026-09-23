// Package flintlock is a small client for the flintlock microVM gRPC API.
package flintlock

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ismoilovdevml/firerunner/internal/config"
)

type Client struct {
	conn      *grpc.ClientConn
	api       mvmv1.MicroVMClient
	namespace string
}

// basicAuth sends flintlockd's --basic-auth-token as "authorization: basic base64(token)".
type basicAuth string

func (t basicAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "basic " + base64.StdEncoding.EncodeToString([]byte(t))}, nil
}

// flintlockd listens on localhost only, so plaintext is acceptable.
func (basicAuth) RequireTransportSecurity() bool { return false }

func Dial(cfg config.Flintlock) (*Client, error) {
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading flintlock token: %w", err)
	}
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(basicAuth(strings.TrimSpace(string(token)))),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to flintlock at %s: %w", cfg.Endpoint, err)
	}
	return &Client{conn: conn, api: mvmv1.NewMicroVMClient(conn), namespace: cfg.Namespace}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Namespace() string { return c.namespace }

// Create submits the spec and returns the UID flintlock assigned to the microVM.
func (c *Client) Create(ctx context.Context, spec *types.MicroVMSpec) (string, error) {
	spec.Namespace = c.namespace
	resp, err := c.api.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{Microvm: spec})
	if err != nil {
		return "", err
	}
	if resp.GetMicrovm().GetSpec().GetUid() == "" {
		return "", fmt.Errorf("flintlock returned no uid for %s", spec.Id)
	}
	return resp.GetMicrovm().GetSpec().GetUid(), nil
}

func (c *Client) Delete(ctx context.Context, uid string) error {
	_, err := c.api.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid})
	return err
}

func (c *Client) List(ctx context.Context) ([]*types.MicroVM, error) {
	resp, err := c.api.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: c.namespace})
	if err != nil {
		return nil, err
	}
	return resp.GetMicrovm(), nil
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
