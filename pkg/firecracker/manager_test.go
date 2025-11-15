package firecracker

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

// Mock Flintlock Client
type mockFlintlockClient struct {
	createCalled bool
	deleteCalled bool
	getCalled    bool
	listCalled   bool
	createError  error
	deleteError  error
	getError     error
}

func (m *mockFlintlockClient) CreateMicroVM(ctx context.Context, spec *MicroVMSpec) (*MicroVM, error) {
	m.createCalled = true
	if m.createError != nil {
		return nil, m.createError
	}
	return &MicroVM{
		ID:        spec.ID,
		Namespace: spec.Namespace,
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
		Metadata:  spec.Metadata,
		Labels:    spec.Labels,
	}, nil
}

func (m *mockFlintlockClient) DeleteMicroVM(ctx context.Context, namespace, id string) error {
	m.deleteCalled = true
	return m.deleteError
}

func (m *mockFlintlockClient) GetMicroVM(ctx context.Context, namespace, id string) (*MicroVM, error) {
	m.getCalled = true
	if m.getError != nil {
		return nil, m.getError
	}
	return &MicroVM{
		ID:        id,
		Namespace: namespace,
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}, nil
}

func (m *mockFlintlockClient) ListMicroVMs(ctx context.Context, namespace string) ([]*MicroVM, error) {
	m.listCalled = true
	return []*MicroVM{}, nil
}

func (m *mockFlintlockClient) WaitForMicroVM(ctx context.Context, namespace, id string, state string, timeout time.Duration) error {
	return nil
}

func (m *mockFlintlockClient) Close() error {
	return nil
}

func (m *mockFlintlockClient) Health(ctx context.Context) error {
	return nil
}

func testManagerLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logger
}

func testVMConfig() *config.VMConfig {
	return &config.VMConfig{
		DefaultVCPU:      2,
		DefaultMemoryMB:  4096,
		KernelImage:      "ghcr.io/liquidmetal-dev/flintlock-kernel:5.10.77",
		RootFSImage:      "ghcr.io/ismoilovdevml/firerunner-rootfs:latest",
		NetworkInterface: "eth0",
		ExtraLabels:      map[string]string{"env": "test"},
	}
}

func TestNewManager(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	client, err := NewClient(&config.FlintlockConfig{
		Endpoint: "localhost:9090",
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	manager := NewManager(client, cfg, logger)

	if manager == nil {
		t.Fatal("NewManager returned nil")
	}

	if manager.config != cfg {
		t.Error("Manager config not set correctly")
	}

	if len(manager.vms) != 0 {
		t.Error("VMs map should be empty initially")
	}
}

func TestManager_CreateVM(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	mockClient := &mockFlintlockClient{}

	manager := &Manager{
		client:     mockClient,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	req := &VMRequest{
		JobID:     "123",
		ProjectID: "456",
		VCPU:      4,
		MemoryMB:  8192,
		Tags:      []string{"firecracker-4cpu-8gb"},
		Metadata:  map[string]string{"test": "value"},
	}

	ctx := context.Background()
	vm, err := manager.CreateVM(ctx, req)
	if err != nil {
		t.Fatalf("CreateVM() failed: %v", err)
	}

	if vm == nil {
		t.Fatal("CreateVM() returned nil VM")
	}

	// Verify VM was tracked
	tracked := manager.getVM(vm.ID)
	if tracked == nil {
		t.Error("VM should be tracked after creation")
	}

	if tracked.ID != vm.ID {
		t.Errorf("Expected VM ID %s, got %s", vm.ID, tracked.ID)
	}

	// Verify metadata
	if vm.Metadata["test"] != "value" {
		t.Error("Custom metadata should be preserved")
	}

	if vm.Metadata["firerunner.job_id"] != "123" {
		t.Error("System metadata should be added")
	}

	// Verify labels
	if vm.Labels["app"] != "firerunner" {
		t.Error("Standard labels should be added")
	}

	if vm.Labels["job_id"] != "123" {
		t.Error("Job ID label should be set")
	}

	if vm.Labels["env"] != "test" {
		t.Error("Extra labels from config should be added")
	}
}

func TestManager_DestroyVM(t *testing.T) {
	mockClient := &mockFlintlockClient{}

	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		client:     mockClient,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	// Track a VM first
	vm := &MicroVM{
		ID:        "test-vm-123",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}
	manager.trackVM(vm)

	// Destroy it
	ctx := context.Background()
	err := manager.DestroyVM(ctx, vm.ID)
	if err != nil {
		t.Fatalf("DestroyVM() failed: %v", err)
	}

	// Verify it was untracked
	tracked := manager.getVM(vm.ID)
	if tracked != nil {
		t.Error("VM should be untracked after destruction")
	}
}

func TestManager_DestroyVM_NotFound(t *testing.T) {
	mockClient := &mockFlintlockClient{}

	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		client:     mockClient,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	ctx := context.Background()
	err := manager.DestroyVM(ctx, "non-existent-vm")
	if err == nil {
		t.Error("DestroyVM() should fail for non-existent VM")
	}
}

func TestManager_GetVM(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	// Test non-existent VM
	_, err := manager.GetVM("non-existent")
	if err == nil {
		t.Error("GetVM() should return error for non-existent VM")
	}

	// Track a VM
	vm := &MicroVM{
		ID:        "test-vm-123",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}
	manager.trackVM(vm)

	// Get it
	retrieved, err := manager.GetVM(vm.ID)
	if err != nil {
		t.Fatalf("GetVM() failed: %v", err)
	}

	if retrieved.ID != vm.ID {
		t.Errorf("Expected VM ID %s, got %s", vm.ID, retrieved.ID)
	}
}

func TestManager_ListVMs(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	// Initially empty
	vms := manager.ListVMs()
	if len(vms) != 0 {
		t.Errorf("Expected 0 VMs, got %d", len(vms))
	}

	// Track some VMs
	for i := 1; i <= 3; i++ {
		vm := &MicroVM{
			ID:        fmt.Sprintf("vm-%d", i),
			Namespace: "firerunner",
			State:     "running",
			IPAddress: "10.0.0.100",
			CreatedAt: time.Now(),
		}
		manager.trackVM(vm)
	}

	vms = manager.ListVMs()
	if len(vms) != 3 {
		t.Errorf("Expected 3 VMs, got %d", len(vms))
	}
}

func TestManager_GetVMStats(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	// Track VMs with different states
	states := []string{"running", "running", "stopped", "failed"}
	for i, state := range states {
		vm := &MicroVM{
			ID:        fmt.Sprintf("vm-%d", i+1),
			Namespace: "firerunner",
			State:     state,
			IPAddress: "10.0.0.100",
			CreatedAt: time.Now(),
		}
		manager.trackVM(vm)
	}

	stats := manager.GetVMStats()

	totalVMs, ok := stats["total_vms"].(int)
	if !ok || totalVMs != 4 {
		t.Errorf("Expected total_vms = 4, got %v", stats["total_vms"])
	}

	byState, ok := stats["by_state"].(map[string]int)
	if !ok {
		t.Fatal("by_state should be a map")
	}

	if byState["running"] != 2 {
		t.Errorf("Expected 2 running VMs, got %d", byState["running"])
	}

	if byState["stopped"] != 1 {
		t.Errorf("Expected 1 stopped VM, got %d", byState["stopped"])
	}

	if byState["failed"] != 1 {
		t.Errorf("Expected 1 failed VM, got %d", byState["failed"])
	}
}

func TestManager_TrackUntrack(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	vm := &MicroVM{
		ID:        "test-vm",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}

	// Track
	manager.trackVM(vm)
	if len(manager.vms) != 1 {
		t.Error("VM should be tracked")
	}

	// Untrack
	manager.untrackVM(vm.ID)
	if len(manager.vms) != 0 {
		t.Error("VM should be untracked")
	}
}

func TestManager_Cleanup(t *testing.T) {
	mockClient := &mockFlintlockClient{}

	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		client:     mockClient,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	// Add old VM (should be cleaned up)
	oldVM := &MicroVM{
		ID:        "old-vm",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now().Add(-3 * time.Hour),
	}
	manager.trackVM(oldVM)

	// Add recent VM (should NOT be cleaned up)
	recentVM := &MicroVM{
		ID:        "recent-vm",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.101",
		CreatedAt: time.Now(),
	}
	manager.trackVM(recentVM)

	// Run cleanup
	manager.cleanup()

	// Check results - old VM should be untracked (cleanup attempts to destroy it)
	// Recent VM should still be tracked
	_, err := manager.GetVM("recent-vm")
	if err != nil {
		t.Error("Recent VM should NOT be cleaned up")
	}
}

func TestManager_PrepareMetadata(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	req := &VMRequest{
		JobID:     "123",
		ProjectID: "456",
		Metadata:  map[string]string{"custom": "value"},
	}

	metadata := manager.prepareMetadata(req)

	// Check custom metadata preserved
	if metadata["custom"] != "value" {
		t.Error("Custom metadata should be preserved")
	}

	// Check system metadata added
	if metadata["firerunner.job_id"] != "123" {
		t.Error("System metadata should be added")
	}

	if metadata["firerunner.project_id"] != "456" {
		t.Error("Project ID metadata should be added")
	}

	if metadata["firerunner.created_at"] == "" {
		t.Error("Created at metadata should be added")
	}
}

func TestManager_PrepareLabels(t *testing.T) {
	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		config: cfg,
		vms:    make(map[string]*MicroVM),
		logger: logger,
	}

	req := &VMRequest{
		JobID:     "123",
		ProjectID: "456",
	}

	labels := manager.prepareLabels(req)

	// Check standard labels
	if labels["app"] != "firerunner" {
		t.Error("Standard app label should be set")
	}

	if labels["job_id"] != "123" {
		t.Error("Job ID label should be set")
	}

	if labels["project_id"] != "456" {
		t.Error("Project ID label should be set")
	}

	// Check extra labels from config
	if labels["env"] != "test" {
		t.Error("Extra labels from config should be added")
	}
}

func TestGenerateVMID(t *testing.T) {
	jobID := "123"

	id1 := generateVMID(jobID)
	id2 := generateVMID(jobID)

	// Should contain job ID
	if !contains(id1, "123") {
		t.Error("VM ID should contain job ID")
	}

	// Should be unique
	if id1 == id2 {
		t.Error("VM IDs should be unique")
	}

	// Should have expected format
	if !contains(id1, "vm-") {
		t.Error("VM ID should start with 'vm-'")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr ||
		(len(s) > len(substr) && contains(s[1:], substr))
}

func TestManager_Shutdown(t *testing.T) {
	mockClient := &mockFlintlockClient{}

	cfg := testVMConfig()
	logger := testManagerLogger()

	manager := &Manager{
		client:     mockClient,
		config:     cfg,
		vms:        make(map[string]*MicroVM),
		logger:     logger,
		shutdownCh: make(chan struct{}),
	}

	// Track a VM
	vm := &MicroVM{
		ID:        "test-vm",
		Namespace: "firerunner",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}
	manager.trackVM(vm)

	// Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := manager.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown() failed: %v", err)
	}

	// Verify VM was destroyed (untracked)
	tracked := manager.getVM(vm.ID)
	if tracked != nil {
		t.Error("VM should be destroyed during shutdown")
	}
}
