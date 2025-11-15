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

// Client is a wrapper around Flintlock gRPC client
type Client struct {
	conn    *grpc.ClientConn
	config  *config.FlintlockConfig
	useMock bool // Set to false to use real Flintlock implementation
}

// MicroVMSpec defines the specification for creating a MicroVM
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

// MicroVM represents a running MicroVM instance
type MicroVM struct {
	ID        string
	Namespace string
	State     string
	IPAddress string
	CreatedAt time.Time
	Metadata  map[string]string
	Labels    map[string]string
}

// NewClient creates a new Flintlock client
func NewClient(cfg *config.FlintlockConfig) (*Client, error) {
	return NewClientWithMode(cfg, false) // Default to real implementation
}

// NewClientWithMode creates a new Flintlock client with specified mode
func NewClientWithMode(cfg *config.FlintlockConfig, useMock bool) (*Client, error) {
	var opts []grpc.DialOption

	if cfg.TLSEnabled {
		// TODO: Implement TLS credentials
		return nil, fmt.Errorf("TLS not yet implemented")
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add keepalive parameters
	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             3 * time.Second,
		PermitWithoutStream: true,
	}))

	conn, err := grpc.Dial(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Flintlock: %w", err)
	}

	return &Client{
		conn:    conn,
		config:  cfg,
		useMock: useMock,
	}, nil
}

// CreateMicroVM creates a new MicroVM with the given specification
func (c *Client) CreateMicroVM(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	if c.useMock {
		// Use mock implementation for testing
		return c.createMicroVMWithTimeout(ctx, spec)
	}

	// Use real Flintlock implementation
	return c.CreateMicroVMReal(ctx, spec)
}

// createMicroVMWithTimeout creates a MicroVM with timeout
func (c *Client) createMicroVMWithTimeout(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	// TODO: Implement actual Flintlock gRPC call
	// This is a placeholder for the real implementation

	// For now, simulate VM creation
	vm := &MicroVM{
		ID:        spec.ID,
		Namespace: spec.Namespace,
		State:     "running",
		IPAddress: "10.0.0.100", // Placeholder
		CreatedAt: time.Now(),
		Metadata:  spec.Metadata,
		Labels:    spec.Labels,
	}

	return vm, nil
}

// DeleteMicroVM deletes a MicroVM by ID
func (c *Client) DeleteMicroVM(ctx context.Context, namespace, id string) error {
	if c.useMock {
		// Mock implementation - just return success
		return nil
	}

	// Use real Flintlock implementation
	return c.DeleteMicroVMReal(ctx, namespace, id)
}

// GetMicroVM retrieves information about a specific MicroVM
func (c *Client) GetMicroVM(ctx context.Context, namespace, id string) (*MicroVM, error) {
	if c.useMock {
		// Mock implementation
		return nil, fmt.Errorf("mock: VM not found")
	}

	// Use real Flintlock implementation
	return c.GetMicroVMReal(ctx, namespace, id)
}

// ListMicroVMs lists all MicroVMs in a namespace
func (c *Client) ListMicroVMs(ctx context.Context, namespace string) ([]*MicroVM, error) {
	if c.useMock {
		// Mock implementation - return empty list
		return []*MicroVM{}, nil
	}

	// Use real Flintlock implementation
	return c.ListMicroVMsReal(ctx, namespace)
}

// WaitForMicroVM waits for a MicroVM to reach a specific state
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

// Close closes the gRPC connection
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Health checks the health of the Flintlock service
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// TODO: Implement actual health check gRPC call
	// This is a placeholder for the real implementation

	return nil
}
