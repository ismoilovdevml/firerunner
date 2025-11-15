package scheduler

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	gogitlab "github.com/xanzy/go-gitlab"

	"github.com/ismoilovdevml/firerunner/pkg/config"
	"github.com/ismoilovdevml/firerunner/pkg/firecracker"
	"github.com/ismoilovdevml/firerunner/pkg/gitlab"
)

// Mock VM Manager
type mockVMManager struct {
	mu            sync.Mutex
	createCalled  bool
	destroyCalled bool
	createError   error
	destroyError  error
}

func (m *mockVMManager) CreateVM(ctx context.Context, req *firecracker.VMRequest) (*firecracker.MicroVM, error) {
	m.mu.Lock()
	m.createCalled = true
	createError := m.createError
	m.mu.Unlock()

	if createError != nil {
		return nil, createError
	}
	return &firecracker.MicroVM{
		ID:        "test-vm-" + req.JobID,
		Namespace: "default",
		State:     "running",
		IPAddress: "10.0.0.100",
		CreatedAt: time.Now(),
	}, nil
}

func (m *mockVMManager) DestroyVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	m.destroyCalled = true
	destroyError := m.destroyError
	m.mu.Unlock()
	return destroyError
}

func (m *mockVMManager) wasCreateCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalled
}

func (m *mockVMManager) GetVM(vmID string) (*firecracker.MicroVM, error) {
	return nil, nil
}

func (m *mockVMManager) ListVMs() []*firecracker.MicroVM {
	return []*firecracker.MicroVM{}
}

func (m *mockVMManager) StartCleanup(interval time.Duration) {}
func (m *mockVMManager) StopCleanup()                        {}
func (m *mockVMManager) Shutdown(ctx context.Context) error  { return nil }

// Mock GitLab Service
type mockGitLabService struct{}

func (m *mockGitLabService) RegisterRunner(ctx context.Context, projectID int64, vmIP string, tags []string) (*gitlab.RunnerRegistration, error) {
	return &gitlab.RunnerRegistration{
		ID:    1234,
		Token: "mock-token",
	}, nil
}

func (m *mockGitLabService) UnregisterRunner(ctx context.Context, runnerID int64) error {
	return nil
}

func (m *mockGitLabService) GetJob(ctx context.Context, projectID, jobID int64) (*gogitlab.Job, error) {
	// Return a completed job for testing
	return &gogitlab.Job{
		ID:     int(jobID),
		Status: "success",
	}, nil
}

func (m *mockGitLabService) ProcessJobEvent(event *gitlab.JobEvent) error {
	return nil
}

func (m *mockGitLabService) ProcessPipelineEvent(event *gitlab.PipelineEvent) error {
	return nil
}

func newMockGitLabService() *mockGitLabService {
	return &mockGitLabService{}
}

func testLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logger
}

func testSchedulerConfig() *config.SchedulerConfig {
	return &config.SchedulerConfig{
		QueueSize:         10,
		WorkerCount:       2,
		JobTimeout:        5 * time.Minute,
		CleanupInterval:   1 * time.Minute,
		VMStartTimeout:    2 * time.Minute,
		VMShutdownTimeout: 1 * time.Minute,
	}
}

func TestNewScheduler(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	if scheduler == nil {
		t.Fatal("NewScheduler returned nil")
	}

	if scheduler.config != cfg {
		t.Error("Scheduler config not set correctly")
	}

	if len(scheduler.jobs) != 0 {
		t.Error("Jobs map should be empty initially")
	}

	if cap(scheduler.jobQueue) != cfg.QueueSize {
		t.Errorf("Job queue capacity should be %d, got %d", cfg.QueueSize, cap(scheduler.jobQueue))
	}
}

func TestScheduler_Start(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	err := scheduler.Start()
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Check workers were created
	if len(scheduler.workers) != cfg.WorkerCount {
		t.Errorf("Expected %d workers, got %d", cfg.WorkerCount, len(scheduler.workers))
	}

	// Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = scheduler.Shutdown(ctx)
}

func TestScheduler_ScheduleJob(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)
	err := scheduler.Start()
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = scheduler.Shutdown(ctx)
	}()

	event := &gitlab.JobEvent{
		BuildID:    123,
		ProjectID:  456,
		PipelineID: 789,
		BuildName:  "test-job",
		BuildStage: "test",
		BuildTags:  []string{"firecracker-2cpu-4gb"},
	}

	err = scheduler.ScheduleJob(event)
	if err != nil {
		t.Fatalf("ScheduleJob() failed: %v", err)
	}

	// Wait a bit for job to be queued
	time.Sleep(100 * time.Millisecond)

	// Check job was tracked
	job, exists := scheduler.GetJob(123)
	if !exists {
		t.Error("Job should exist after scheduling")
	}

	if job.ID != 123 {
		t.Errorf("Expected job ID 123, got %d", job.ID)
	}

	if job.Status != "queued" && job.Status != "running" {
		t.Errorf("Expected job status 'queued' or 'running', got '%s'", job.Status)
	}
}

func TestScheduler_GetJob(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	// Test non-existent job
	_, exists := scheduler.GetJob(999)
	if exists {
		t.Error("Non-existent job should not exist")
	}

	// Add a job manually for testing
	job := &Job{
		ID:        123,
		ProjectID: 456,
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	scheduler.trackJob(job)

	// Test existing job
	retrieved, exists := scheduler.GetJob(123)
	if !exists {
		t.Error("Job should exist")
	}

	if retrieved.ID != 123 {
		t.Errorf("Expected job ID 123, got %d", retrieved.ID)
	}
}

func TestScheduler_ListJobs(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	// Initially empty
	jobs := scheduler.ListJobs()
	if len(jobs) != 0 {
		t.Errorf("Expected 0 jobs, got %d", len(jobs))
	}

	// Add jobs
	for i := int64(1); i <= 3; i++ {
		job := &Job{
			ID:        i,
			ProjectID: 100,
			Status:    "queued",
			CreatedAt: time.Now(),
		}
		scheduler.trackJob(job)
	}

	jobs = scheduler.ListJobs()
	if len(jobs) != 3 {
		t.Errorf("Expected 3 jobs, got %d", len(jobs))
	}
}

func TestScheduler_GetStats(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	// Add jobs with different statuses
	statuses := []string{"queued", "running", "finished", "failed"}
	for i, status := range statuses {
		job := &Job{
			ID:        int64(i + 1),
			ProjectID: 100,
			Status:    status,
			CreatedAt: time.Now(),
		}
		scheduler.trackJob(job)
	}

	stats := scheduler.GetStats()

	totalJobs, ok := stats["total_jobs"].(int)
	if !ok || totalJobs != 4 {
		t.Errorf("Expected total_jobs = 4, got %v", stats["total_jobs"])
	}

	queueSize, ok := stats["queue_size"].(int)
	if !ok {
		t.Error("queue_size should be an int")
	}
	_ = queueSize // We don't check exact value as jobs might be processed

	byStatus, ok := stats["by_status"].(map[string]int)
	if !ok {
		t.Fatal("by_status should be a map")
	}

	if byStatus["queued"] != 1 {
		t.Errorf("Expected 1 queued job, got %d", byStatus["queued"])
	}

	if byStatus["running"] != 1 {
		t.Errorf("Expected 1 running job, got %d", byStatus["running"])
	}
}

func TestScheduler_Shutdown(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)
	err := scheduler.Start()
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	// Add a job with cancel function
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:        123,
		ProjectID: 456,
		Status:    "running",
		CreatedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}
	scheduler.trackJob(job)

	// Shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()

	err = scheduler.Shutdown(shutdownCtx)
	if err != nil {
		t.Errorf("Shutdown() failed: %v", err)
	}

	// Verify job context was canceled
	select {
	case <-ctx.Done():
		// Expected
	default:
		t.Error("Job context should be canceled after shutdown")
	}
}

func TestScheduler_TrackUntrack(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	job := &Job{
		ID:        123,
		ProjectID: 456,
		Status:    "queued",
		CreatedAt: time.Now(),
	}

	// Track
	scheduler.trackJob(job)
	if len(scheduler.jobs) != 1 {
		t.Error("Job should be tracked")
	}

	// Untrack
	scheduler.untrackJob(123)
	if len(scheduler.jobs) != 0 {
		t.Error("Job should be untracked")
	}
}

func TestScheduler_UpdateJobStatus(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	job := &Job{
		ID:        123,
		ProjectID: 456,
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	scheduler.trackJob(job)

	// Update to running
	scheduler.updateJobStatus(123, "running")
	updated, _ := scheduler.GetJob(123)
	if updated.Status != "running" {
		t.Errorf("Expected status 'running', got '%s'", updated.Status)
	}
	if updated.StartedAt.IsZero() {
		t.Error("StartedAt should be set when status changes to running")
	}

	// Update to finished
	scheduler.updateJobStatus(123, "finished")
	updated, _ = scheduler.GetJob(123)
	if updated.Status != "finished" {
		t.Errorf("Expected status 'finished', got '%s'", updated.Status)
	}
	if updated.FinishedAt.IsZero() {
		t.Error("FinishedAt should be set when status changes to finished")
	}
}

func TestScheduler_Cleanup(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	// Add old finished job (should be cleaned up)
	oldJob := &Job{
		ID:         123,
		ProjectID:  456,
		Status:     "finished",
		CreatedAt:  time.Now().Add(-3 * time.Hour),
		FinishedAt: time.Now().Add(-2 * time.Hour),
	}
	scheduler.trackJob(oldJob)

	// Add recent finished job (should NOT be cleaned up)
	recentJob := &Job{
		ID:         456,
		ProjectID:  789,
		Status:     "finished",
		CreatedAt:  time.Now().Add(-30 * time.Minute),
		FinishedAt: time.Now().Add(-10 * time.Minute),
	}
	scheduler.trackJob(recentJob)

	// Add running job (should NOT be cleaned up)
	runningJob := &Job{
		ID:        789,
		ProjectID: 101112,
		Status:    "running",
		CreatedAt: time.Now().Add(-3 * time.Hour),
		StartedAt: time.Now().Add(-2 * time.Hour),
	}
	scheduler.trackJob(runningJob)

	// Run cleanup
	scheduler.cleanup()

	// Check results
	_, exists := scheduler.GetJob(123)
	if exists {
		t.Error("Old finished job should be cleaned up")
	}

	_, exists = scheduler.GetJob(456)
	if !exists {
		t.Error("Recent finished job should NOT be cleaned up")
	}

	_, exists = scheduler.GetJob(789)
	if !exists {
		t.Error("Running job should NOT be cleaned up")
	}
}

func TestScheduler_QueueFull(t *testing.T) {
	cfg := &config.SchedulerConfig{
		QueueSize:         1, // Small queue
		WorkerCount:       0, // No workers to process
		JobTimeout:        1 * time.Minute,
		CleanupInterval:   1 * time.Minute,
		VMStartTimeout:    2 * time.Minute,
		VMShutdownTimeout: 1 * time.Minute,
	}
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)

	// Fill the queue
	event1 := &gitlab.JobEvent{
		BuildID:    1,
		ProjectID:  100,
		PipelineID: 200,
		BuildName:  "test-1",
		BuildStage: "test",
		BuildTags:  []string{"firecracker-2cpu-4gb"},
	}
	err := scheduler.ScheduleJob(event1)
	if err != nil {
		t.Fatalf("First ScheduleJob() should succeed: %v", err)
	}

	// Try to add another (should timeout since queue is full and no workers)
	event2 := &gitlab.JobEvent{
		BuildID:    2,
		ProjectID:  100,
		PipelineID: 200,
		BuildName:  "test-2",
		BuildStage: "test",
		BuildTags:  []string{"firecracker-2cpu-4gb"},
	}
	err = scheduler.ScheduleJob(event2)
	if err == nil {
		t.Error("ScheduleJob() should fail when queue is full")
	}
}

func TestJob_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:        123,
		ProjectID: 456,
		Status:    "queued",
		CreatedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}

	// Cancel the context
	cancel()

	// Verify context is done
	select {
	case <-job.ctx.Done():
		// Expected
	case <-time.After(1 * time.Second):
		t.Error("Job context should be canceled")
	}
}

func TestWorker_ProcessJob_Success(t *testing.T) {
	cfg := testSchedulerConfig()
	vmManager := &mockVMManager{}
	gitlabSvc := newMockGitLabService()
	logger := testLogger()

	scheduler := NewScheduler(cfg, vmManager, gitlabSvc, logger)
	err := scheduler.Start()
	if err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = scheduler.Shutdown(ctx)
	}()

	// Schedule a job
	event := &gitlab.JobEvent{
		BuildID:    123,
		ProjectID:  456,
		PipelineID: 789,
		BuildName:  "test-job",
		BuildStage: "test",
		BuildTags:  []string{"firecracker-4cpu-8gb"},
	}

	err = scheduler.ScheduleJob(event)
	if err != nil {
		t.Fatalf("ScheduleJob() failed: %v", err)
	}

	// Wait for job processing
	time.Sleep(2 * time.Second)

	// Verify VM was created
	if !vmManager.wasCreateCalled() {
		t.Error("VM should have been created")
	}

	// Check job status
	job, exists := scheduler.GetJob(123)
	if !exists {
		t.Fatal("Job should exist")
	}

	if job.Status != "running" && job.Status != "finished" {
		t.Errorf("Expected job status 'running' or 'finished', got '%s'", job.Status)
	}

	if job.VCPU != 4 {
		t.Errorf("Expected VCPU 4, got %d", job.VCPU)
	}

	if job.MemoryMB != 8192 {
		t.Errorf("Expected memory 8192, got %d", job.MemoryMB)
	}
}
