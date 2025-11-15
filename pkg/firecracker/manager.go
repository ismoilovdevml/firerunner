package firecracker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

// Manager manages the lifecycle of MicroVMs
type Manager struct {
	client     *Client
	config     *config.VMConfig
	vms        map[string]*MicroVM
	mu         sync.RWMutex
	logger     *logrus.Logger
	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

// NewManager creates a new VM manager
func NewManager(client *Client, cfg *config.VMConfig, logger *logrus.Logger) *Manager {
	return &Manager{
		client:     client,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}
}

// VMRequest represents a request to create a VM
type VMRequest struct {
	JobID     string
	ProjectID string
	VCPU      int64
	MemoryMB  int64
	Tags      []string
	Metadata  map[string]string
}

// CreateVM creates a new MicroVM for a GitLab job
func (m *Manager) CreateVM(ctx context.Context, req *VMRequest) (*MicroVM, error) {
	m.logger.WithFields(logrus.Fields{
		"job_id":     req.JobID,
		"project_id": req.ProjectID,
		"vcpu":       req.VCPU,
		"memory_mb":  req.MemoryMB,
	}).Info("Creating MicroVM for job")

	// Generate unique VM ID
	vmID := generateVMID(req.JobID)

	// Prepare VM spec
	spec := &MicroVMSpec{
		ID:               vmID,
		Namespace:        "firerunner",
		VCPU:             req.VCPU,
		MemoryMB:         req.MemoryMB,
		KernelImage:      m.config.KernelImage,
		RootFSImage:      m.config.RootFSImage,
		NetworkInterface: m.config.NetworkInterface,
		Metadata:         m.prepareMetadata(req),
		Labels:           m.prepareLabels(req),
	}

	// Create VM via Flintlock
	startTime := time.Now()
	vm, err := m.client.CreateMicroVM(ctx, spec)
	if err != nil {
		m.logger.WithError(err).Error("Failed to create MicroVM")
		return nil, fmt.Errorf("failed to create microVM: %w", err)
	}

	duration := time.Since(startTime)
	m.logger.WithFields(logrus.Fields{
		"vm_id":      vm.ID,
		"duration":   duration,
		"ip_address": vm.IPAddress,
	}).Info("MicroVM created successfully")

	// Track VM
	m.trackVM(vm)

	return vm, nil
}

// DestroyVM destroys a MicroVM
func (m *Manager) DestroyVM(ctx context.Context, vmID string) error {
	m.logger.WithField("vm_id", vmID).Info("Destroying MicroVM")

	// Get VM info
	vm := m.getVM(vmID)
	if vm == nil {
		return fmt.Errorf("VM %s not found", vmID)
	}

	// Delete VM via Flintlock
	startTime := time.Now()
	if err := m.client.DeleteMicroVM(ctx, vm.Namespace, vm.ID); err != nil {
		m.logger.WithError(err).Error("Failed to delete MicroVM")
		return fmt.Errorf("failed to delete microVM: %w", err)
	}

	duration := time.Since(startTime)
	m.logger.WithFields(logrus.Fields{
		"vm_id":    vmID,
		"duration": duration,
	}).Info("MicroVM destroyed successfully")

	// Untrack VM
	m.untrackVM(vmID)

	return nil
}

// GetVM retrieves a VM by ID
func (m *Manager) GetVM(vmID string) (*MicroVM, error) {
	vm := m.getVM(vmID)
	if vm == nil {
		return nil, fmt.Errorf("VM %s not found", vmID)
	}
	return vm, nil
}

// ListVMs returns all tracked VMs
func (m *Manager) ListVMs() []*MicroVM {
	m.mu.RLock()
	defer m.mu.RUnlock()

	vms := make([]*MicroVM, 0, len(m.vms))
	for _, vm := range m.vms {
		vms = append(vms, vm)
	}

	return vms
}

// StartCleanup starts the background cleanup routine
func (m *Manager) StartCleanup(interval time.Duration) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.cleanup()
			case <-m.shutdownCh:
				return
			}
		}
	}()

	m.logger.WithField("interval", interval).Info("Started VM cleanup routine")
}

// cleanup removes stale VMs
func (m *Manager) cleanup() {
	m.logger.Debug("Running VM cleanup")

	m.mu.RLock()
	staleVMs := make([]string, 0)
	maxAge := 2 * time.Hour // VMs older than 2 hours are considered stale

	for id, vm := range m.vms {
		if time.Since(vm.CreatedAt) > maxAge {
			staleVMs = append(staleVMs, id)
		}
	}
	m.mu.RUnlock()

	if len(staleVMs) > 0 {
		m.logger.WithField("count", len(staleVMs)).Warn("Found stale VMs")

		for _, vmID := range staleVMs {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := m.DestroyVM(ctx, vmID); err != nil {
				m.logger.WithError(err).WithField("vm_id", vmID).Error("Failed to cleanup stale VM")
			}
			cancel()
		}
	}
}

// Shutdown gracefully shuts down the manager
func (m *Manager) Shutdown(ctx context.Context) error {
	m.logger.Info("Shutting down VM manager")

	// Signal cleanup routine to stop
	close(m.shutdownCh)

	// Wait for cleanup routine with timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		m.logger.Info("VM manager cleanup routine stopped")
	case <-ctx.Done():
		m.logger.Warn("VM manager shutdown timeout")
		return ctx.Err()
	}

	// Destroy all remaining VMs
	vms := m.ListVMs()
	m.logger.WithField("count", len(vms)).Info("Destroying remaining VMs")

	for _, vm := range vms {
		if err := m.DestroyVM(ctx, vm.ID); err != nil {
			m.logger.WithError(err).WithField("vm_id", vm.ID).Error("Failed to destroy VM during shutdown")
		}
	}

	return nil
}

// Helper methods

func (m *Manager) trackVM(vm *MicroVM) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vms[vm.ID] = vm
}

func (m *Manager) untrackVM(vmID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vms, vmID)
}

func (m *Manager) getVM(vmID string) *MicroVM {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.vms[vmID]
}

func (m *Manager) prepareMetadata(req *VMRequest) map[string]string {
	metadata := make(map[string]string)

	// Copy user-provided metadata
	for k, v := range req.Metadata {
		metadata[k] = v
	}

	// Add system metadata
	metadata["firerunner.job_id"] = req.JobID
	metadata["firerunner.project_id"] = req.ProjectID
	metadata["firerunner.created_at"] = time.Now().Format(time.RFC3339)

	return metadata
}

func (m *Manager) prepareLabels(req *VMRequest) map[string]string {
	labels := make(map[string]string)

	// Add standard labels
	labels["app"] = "firerunner"
	labels["job_id"] = req.JobID
	labels["project_id"] = req.ProjectID

	// Add custom labels from config
	for k, v := range m.config.ExtraLabels {
		labels[k] = v
	}

	return labels
}

func generateVMID(jobID string) string {
	// Generate a unique VM ID based on job ID and UUID
	return fmt.Sprintf("vm-%s-%s", jobID, uuid.New().String()[:8])
}

// GetVMStats returns statistics about managed VMs
func (m *Manager) GetVMStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := map[string]interface{}{
		"total_vms": len(m.vms),
		"by_state":  make(map[string]int),
	}

	byState := stats["by_state"].(map[string]int)
	for _, vm := range m.vms {
		byState[vm.State]++
	}

	return stats
}
