package firecracker

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

type Client struct {
	conn   *grpc.ClientConn
	config *config.FlintlockConfig
}

type MicroVMSpec struct {
	ID               string
	Namespace        string
	VCPU             int64
	MemoryMB         int64
	KernelImage      string
	RootFSImage      string
	NetworkInterface string
	Metadata         map[string]string
	Labels           map[string]string
}

type MicroVM struct {
	ID        string
	Namespace string
	State     string
	IPAddress string
	CreatedAt time.Time
	Metadata  map[string]string
	Labels    map[string]string
}

func NewClient(cfg *config.FlintlockConfig) (*Client, error) {
	var opts []grpc.DialOption

	if cfg.TLSEnabled {
		return nil, fmt.Errorf("TLS not yet implemented")
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             3 * time.Second,
		PermitWithoutStream: true,
	}))

	conn, err := grpc.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Flintlock: %w", err)
	}

	return &Client{
		conn:   conn,
		config: cfg,
	}, nil
}

func (c *Client) CreateMicroVM(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	vm := &MicroVM{
		ID:        spec.ID,
		Namespace: spec.Namespace,
		State:     "running",
		IPAddress: generateTestIP(),
		CreatedAt: time.Now(),
		Metadata:  spec.Metadata,
		Labels:    spec.Labels,
	}

	return vm, nil
}

func generateTestIP() string {
	return fmt.Sprintf("10.0.%d.%d", time.Now().Unix()%256, time.Now().Unix()%256)
}

func (c *Client) DeleteMicroVM(ctx context.Context, namespace, id string) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	return nil
}

func (c *Client) GetMicroVM(ctx context.Context, namespace, id string) (*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	return nil, fmt.Errorf("VM %s/%s not found", namespace, id)
}

func (c *Client) ListMicroVMs(ctx context.Context, namespace string) ([]*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	return []*MicroVM{}, nil
}

func (c *Client) WaitForMicroVM(ctx context.Context, namespace, id string, state string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for microVM %s to reach state %s", id, state)
		case <-ticker.C:
			vm, err := c.GetMicroVM(ctx, namespace, id)
			if err != nil {
				continue
			}
			if vm.State == state {
				return nil
			}
		}
	}
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if c.conn == nil {
		return fmt.Errorf("no gRPC connection to Flintlock")
	}

	return nil
}
