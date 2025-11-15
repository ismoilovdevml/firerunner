package firecracker

// THIS FILE WILL CONTAIN REAL FLINTLOCK IMPLEMENTATION
//
// Steps to complete:
// 1. Add Flintlock protobuf dependencies:
//    go get github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1
//
// 2. Replace mock CreateMicroVM with real gRPC calls
// 3. Implement proper error handling
// 4. Add VM state polling
// 5. Implement DeleteMicroVM, GetMicroVM, ListMicroVMs

// Example real implementation:
/*
import (
    flintlockv1alpha1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
)

func (c *Client) createMicroVMReal(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
    client := flintlockv1alpha1.NewMicroVMServiceClient(c.conn)

    req := &flintlockv1alpha1.CreateMicroVMRequest{
        Microvm: &flintlockv1alpha1.MicroVMSpec{
            Id:        spec.ID,
            Namespace: spec.Namespace,
            Vcpu:      spec.VCPU,
            MemoryInMb: spec.MemoryMB,
            Kernel: &flintlockv1alpha1.Kernel{
                Image:    spec.KernelImage,
                Filename: "vmlinux",
            },
            RootVolume: &flintlockv1alpha1.Volume{
                Id:         "root",
                IsReadOnly: false,
                Source: &flintlockv1alpha1.VolumeSource{
                    ContainerSource: spec.RootFSImage,
                },
            },
            Interfaces: []*flintlockv1alpha1.NetworkInterface{
                {
                    DeviceId: "eth0",
                    Type:     flintlockv1alpha1.NetworkInterface_MACVTAP,
                },
            },
            Metadata: spec.Metadata,
        },
    }

    resp, err := client.CreateMicroVM(ctx, req)
    if err != nil {
        return nil, fmt.Errorf("flintlock CreateMicroVM failed: %w", err)
    }

    return &MicroVM{
        ID:        resp.Microvm.Spec.Id,
        Namespace: resp.Microvm.Spec.Namespace,
        State:     resp.Microvm.Status.State.String(),
        IPAddress: extractIPAddress(resp.Microvm.Status.NetworkInterfaces),
        CreatedAt: time.Now(),
        Metadata:  spec.Metadata,
        Labels:    spec.Labels,
    }, nil
}

func extractIPAddress(interfaces []*flintlockv1alpha1.NetworkInterfaceStatus) string {
    if len(interfaces) > 0 && len(interfaces[0].Addresses) > 0 {
        return interfaces[0].Addresses[0]
    }
    return ""
}
*/

// TODO: Implement above functions
