package firecracker

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	flintlockv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	flintlocktypes "github.com/liquidmetal-dev/flintlock/api/types"
)

// RealFlintlockClient implements real Flintlock gRPC calls
type RealFlintlockClient struct {
	client flintlockv1.MicroVMServiceClient
	logger *logrus.Logger
}

// CreateMicroVMReal creates a MicroVM using real Flintlock gRPC
func (c *Client) CreateMicroVMReal(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("gRPC connection not established")
	}

	// Create Flintlock gRPC client
	client := flintlockv1.NewMicroVMServiceClient(c.conn)

	// Prepare the request
	req := &flintlockv1.CreateMicroVMRequest{
		Microvm: &flintlocktypes.MicroVMSpec{
			Id:         spec.ID,
			Namespace:  spec.Namespace,
			Vcpu:       spec.VCPU,
			MemoryInMb: spec.MemoryMB,

			// Kernel configuration
			Kernel: &flintlocktypes.Kernel{
				Image:            spec.KernelImage,
				Filename:         "vmlinux",
				AddVmlinuxHeader: false,
			},

			// Root volume
			RootVolume: &flintlocktypes.Volume{
				Id:         "root",
				IsReadOnly: false,
				Source: &flintlocktypes.VolumeSource{
					Container: &flintlocktypes.ContainerVolumeSource{
						Image: spec.RootFSImage,
					},
				},
			},

			// Network interfaces
			Interfaces: []*flintlocktypes.NetworkInterface{
				{
					DeviceId: "eth0",
					Type:     flintlocktypes.IfaceType_MACVTAP,
					GuestMac: "",
				},
			},

			// Metadata
			Metadata: spec.Metadata,
		},
	}

	// Make the gRPC call with retry
	var resp *flintlockv1.CreateMicroVMResponse
	var lastErr error

	for attempt := 0; attempt < c.config.RetryAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
		resp, lastErr = client.CreateMicroVM(callCtx, req)
		cancel()

		if lastErr == nil {
			break
		}

		if attempt < c.config.RetryAttempts-1 {
			backoff := c.config.RetryDelay * time.Duration(attempt+1)
			time.Sleep(backoff)
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("failed to create microVM after %d attempts: %w",
			c.config.RetryAttempts, lastErr)
	}

	// Extract IP address from response
	ipAddress := extractIPAddress(resp.Microvm.Status.NetworkInterfaces)

	// Convert to our MicroVM type
	vm := &MicroVM{
		ID:        resp.Microvm.Spec.Id,
		Namespace: resp.Microvm.Spec.Namespace,
		State:     convertState(resp.Microvm.Status.State),
		IPAddress: ipAddress,
		CreatedAt: time.Now(),
		Metadata:  spec.Metadata,
		Labels:    spec.Labels,
	}

	return vm, nil
}

// DeleteMicroVMReal deletes a MicroVM using real Flintlock gRPC
func (c *Client) DeleteMicroVMReal(ctx context.Context, namespace, id string) error {
	client := flintlockv1.NewMicroVMServiceClient(c.conn)

	req := &flintlockv1.DeleteMicroVMRequest{
		Id:        id,
		Namespace: namespace,
	}

	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	_, err := client.DeleteMicroVM(callCtx, req)
	if err != nil {
		return fmt.Errorf("failed to delete microVM %s/%s: %w", namespace, id, err)
	}

	return nil
}

// GetMicroVMReal retrieves MicroVM info using real Flintlock gRPC
func (c *Client) GetMicroVMReal(ctx context.Context, namespace, id string) (*MicroVM, error) {
	client := flintlockv1.NewMicroVMServiceClient(c.conn)

	req := &flintlockv1.GetMicroVMRequest{
		Id:        id,
		Namespace: namespace,
	}

	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	resp, err := client.GetMicroVM(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get microVM %s/%s: %w", namespace, id, err)
	}

	ipAddress := extractIPAddress(resp.Microvm.Status.NetworkInterfaces)

	vm := &MicroVM{
		ID:        resp.Microvm.Spec.Id,
		Namespace: resp.Microvm.Spec.Namespace,
		State:     convertState(resp.Microvm.Status.State),
		IPAddress: ipAddress,
		CreatedAt: time.Now(), // TODO: Get from response if available
		Metadata:  resp.Microvm.Spec.Metadata,
	}

	return vm, nil
}

// ListMicroVMsReal lists all MicroVMs in a namespace
func (c *Client) ListMicroVMsReal(ctx context.Context, namespace string) ([]*MicroVM, error) {
	client := flintlockv1.NewMicroVMServiceClient(c.conn)

	req := &flintlockv1.ListMicroVMsRequest{
		Namespace: namespace,
	}

	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	resp, err := client.ListMicroVMs(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to list microVMs in namespace %s: %w", namespace, err)
	}

	vms := make([]*MicroVM, 0, len(resp.Microvm))
	for _, mv := range resp.Microvm {
		ipAddress := extractIPAddress(mv.Status.NetworkInterfaces)

		vm := &MicroVM{
			ID:        mv.Spec.Id,
			Namespace: mv.Spec.Namespace,
			State:     convertState(mv.Status.State),
			IPAddress: ipAddress,
			CreatedAt: time.Now(),
			Metadata:  mv.Spec.Metadata,
		}
		vms = append(vms, vm)
	}

	return vms, nil
}

// Helper functions

func extractIPAddress(interfaces []*flintlocktypes.NetworkInterfaceStatus) string {
	if len(interfaces) == 0 {
		return ""
	}

	iface := interfaces[0]
	if len(iface.HostDeviceName) > 0 {
		// Try to get IP from interface status
		// This is a simplified version - in production you'd query the actual interface
		return iface.HostDeviceName // Placeholder - needs real implementation
	}

	return ""
}

func convertState(state flintlocktypes.MicroVMStatus_MicroVMState) string {
	switch state {
	case flintlocktypes.MicroVMStatus_PENDING:
		return "pending"
	case flintlocktypes.MicroVMStatus_CREATED:
		return "created"
	case flintlocktypes.MicroVMStatus_FAILED:
		return "failed"
	case flintlocktypes.MicroVMStatus_DELETING:
		return "deleting"
	case flintlocktypes.MicroVMStatus_DELETED:
		return "deleted"
	default:
		return "unknown"
	}
}

// WaitForMicroVMState waits for a MicroVM to reach a specific state
func (c *Client) WaitForMicroVMState(ctx context.Context, namespace, id, targetState string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for microVM %s/%s to reach state %s",
				namespace, id, targetState)
		case <-ticker.C:
			vm, err := c.GetMicroVMReal(ctx, namespace, id)
			if err != nil {
				// VM might not exist yet
				continue
			}

			if vm.State == targetState {
				return nil
			}

			if vm.State == "failed" {
				return fmt.Errorf("microVM %s/%s failed to start", namespace, id)
			}
		}
	}
}
