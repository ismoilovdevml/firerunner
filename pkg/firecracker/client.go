package firecracker

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

type Client struct {
	conn   *grpc.ClientConn
	client mvmv1.MicroVMClient
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

	client := mvmv1.NewMicroVMClient(conn)

	return &Client{
		conn:   conn,
		client: client,
		config: cfg,
	}, nil
}

func (c *Client) CreateMicroVM(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	rootFSImage := spec.RootFSImage
	req := &mvmv1.CreateMicroVMRequest{
		Microvm: &types.MicroVMSpec{
			Id:         spec.ID,
			Namespace:  spec.Namespace,
			Vcpu:       int32(spec.VCPU),
			MemoryInMb: int32(spec.MemoryMB),
			Kernel: &types.Kernel{
				Image:            spec.KernelImage,
				AddNetworkConfig: true,
			},
			RootVolume: &types.Volume{
				Id:         fmt.Sprintf("%s-root", spec.ID),
				IsReadOnly: false,
				Source: &types.VolumeSource{
					ContainerSource: &rootFSImage,
				},
			},
			Interfaces: []*types.NetworkInterface{
				{
					DeviceId: spec.NetworkInterface,
					Type:     types.NetworkInterface_MACVTAP,
				},
			},
			Metadata: spec.Metadata,
		},
	}

	resp, err := c.client.CreateMicroVM(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create microVM: %w", err)
	}

	vm := &MicroVM{
		ID:        resp.Microvm.Spec.Id,
		Namespace: resp.Microvm.Spec.Namespace,
		State:     convertState(resp.Microvm.Status.State),
		CreatedAt: time.Now(),
		Metadata:  resp.Microvm.Spec.Metadata,
		Labels:    spec.Labels,
		IPAddress: "",
	}

	return vm, nil
}

func convertState(state types.MicroVMStatus_MicroVMState) string {
	switch state {
	case types.MicroVMStatus_PENDING:
		return "pending"
	case types.MicroVMStatus_CREATED:
		return "running"
	case types.MicroVMStatus_FAILED:
		return "failed"
	case types.MicroVMStatus_DELETING:
		return "deleting"
	default:
		return "unknown"
	}
}

func (c *Client) DeleteMicroVM(ctx context.Context, namespace, id string) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	uid := fmt.Sprintf("%s/%s", namespace, id)
	req := &mvmv1.DeleteMicroVMRequest{
		Uid: uid,
	}

	_, err := c.client.DeleteMicroVM(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to delete microVM %s: %w", uid, err)
	}

	return nil
}

func (c *Client) GetMicroVM(ctx context.Context, namespace, id string) (*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	uid := fmt.Sprintf("%s/%s", namespace, id)
	req := &mvmv1.GetMicroVMRequest{
		Uid: uid,
	}

	resp, err := c.client.GetMicroVM(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get microVM %s: %w", uid, err)
	}

	vm := &MicroVM{
		ID:        resp.Microvm.Spec.Id,
		Namespace: resp.Microvm.Spec.Namespace,
		State:     convertState(resp.Microvm.Status.State),
		Metadata:  resp.Microvm.Spec.Metadata,
		IPAddress: "",
	}

	if resp.Microvm.Spec.CreatedAt != nil {
		vm.CreatedAt = resp.Microvm.Spec.CreatedAt.AsTime()
	}

	return vm, nil
}

func (c *Client) ListMicroVMs(ctx context.Context, namespace string) ([]*MicroVM, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	req := &mvmv1.ListMicroVMsRequest{
		Namespace: namespace,
	}

	resp, err := c.client.ListMicroVMs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to list microVMs in namespace %s: %w", namespace, err)
	}

	vms := make([]*MicroVM, 0, len(resp.Microvm))
	for _, mvm := range resp.Microvm {
		vm := &MicroVM{
			ID:        mvm.Spec.Id,
			Namespace: mvm.Spec.Namespace,
			State:     convertState(mvm.Status.State),
			Metadata:  mvm.Spec.Metadata,
			IPAddress: "",
		}

		if mvm.Spec.CreatedAt != nil {
			vm.CreatedAt = mvm.Spec.CreatedAt.AsTime()
		}

		vms = append(vms, vm)
	}

	return vms, nil
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

	if c.conn == nil || c.client == nil {
		return fmt.Errorf("no gRPC connection to Flintlock")
	}

	_, err := c.client.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{
		Namespace: "health-check",
	})
	if err != nil {
		return fmt.Errorf("Flintlock health check failed: %w", err)
	}

	return nil
}
