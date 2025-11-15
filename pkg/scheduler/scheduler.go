package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ismoilovdevml/firerunner/pkg/config"
	"github.com/ismoilovdevml/firerunner/pkg/firecracker"
	"github.com/ismoilovdevml/firerunner/pkg/gitlab"
)

// VMManager interface for managing VMs (allows mocking in tests)
type VMManager interface {
	CreateVM(ctx context.Context, req *firecracker.VMRequest) (*firecracker.MicroVM, error)
	DestroyVM(ctx context.Context, vmID string) error
	GetVM(vmID string) (*firecracker.MicroVM, error)
	ListVMs() []*firecracker.MicroVM
	StartCleanup(interval time.Duration)
	StopCleanup()
	Shutdown(ctx context.Context) error
}

// GitLabService interface for GitLab operations (allows mocking in tests)
type GitLabService interface {
	RegisterRunner(ctx context.Context, projectID int64, vmIP string, tags []string) (*gitlab.RunnerRegistration, error)
	UnregisterRunner(ctx context.Context, runnerID int64) error
	ProcessJobEvent(event *gitlab.JobEvent) error
	ProcessPipelineEvent(event *gitlab.PipelineEvent) error
}

// Scheduler manages job scheduling and execution
type Scheduler struct {
	config    *config.SchedulerConfig
	vmManager VMManager
	gitlabSvc GitLabService
	logger    *logrus.Logger

	jobQueue chan *Job
	workers  []*Worker
	jobs     map[int64]*Job
	jobsMu   sync.RWMutex

	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

// Job represents a scheduled job
type Job struct {
	ID         int64
	ProjectID  int64
	PipelineID int64
	Status     string
	Tags       []string
	VCPU       int64
	MemoryMB   int64
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	VMID string
	VM   *firecracker.MicroVM

	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

// Worker represents a job worker
type Worker struct {
	ID         int
	scheduler  *Scheduler
	logger     *logrus.Entry
	shutdownCh chan struct{}
}

// NewScheduler creates a new job scheduler
func NewScheduler(
	cfg *config.SchedulerConfig,
	vmManager VMManager,
	gitlabSvc GitLabService,
	logger *logrus.Logger,
) *Scheduler {
	return &Scheduler{
		config:     cfg,
		vmManager:  vmManager,
		gitlabSvc:  gitlabSvc,
		logger:     logger,
		jobQueue:   make(chan *Job, cfg.QueueSize),
		jobs:       make(map[int64]*Job),
		shutdownCh: make(chan struct{}),
	}
}

// Start starts the scheduler
func (s *Scheduler) Start() error {
	s.logger.WithField("workers", s.config.WorkerCount).Info("Starting scheduler")

	// Start workers
	s.workers = make([]*Worker, s.config.WorkerCount)
	for i := 0; i < s.config.WorkerCount; i++ {
		worker := &Worker{
			ID:         i + 1,
			scheduler:  s,
			logger:     s.logger.WithField("worker_id", i+1),
			shutdownCh: make(chan struct{}),
		}
		s.workers[i] = worker

		s.wg.Add(1)
		go worker.run()
	}

	// Start cleanup routine
	s.wg.Add(1)
	go s.cleanupRoutine()

	s.logger.Info("Scheduler started successfully")
	return nil
}

// ScheduleJob schedules a new job
func (s *Scheduler) ScheduleJob(event *gitlab.JobEvent) error {
	s.logger.WithFields(logrus.Fields{
		"job_id":     event.BuildID,
		"project_id": event.ProjectID,
		"stage":      event.BuildStage,
		"name":       event.BuildName,
	}).Info("Scheduling new job")

	// Parse VM requirements from tags
	vcpu, memoryMB := gitlab.ParseVMRequirements(event.BuildTags)

	// Create job
	ctx, cancel := context.WithTimeout(context.Background(), s.config.JobTimeout)
	job := &Job{
		ID:         event.BuildID,
		ProjectID:  event.ProjectID,
		PipelineID: event.PipelineID,
		Status:     "queued",
		Tags:       event.BuildTags,
		VCPU:       vcpu,
		MemoryMB:   memoryMB,
		CreatedAt:  time.Now(),
		ctx:        ctx,
		cancel:     cancel,
	}

	// Track job
	s.trackJob(job)

	// Queue job
	select {
	case s.jobQueue <- job:
		s.logger.WithField("job_id", job.ID).Info("Job queued successfully")
		return nil
	case <-time.After(5 * time.Second):
		cancel()
		s.untrackJob(job.ID)
		return fmt.Errorf("job queue is full, cannot schedule job %d", job.ID)
	}
}

// GetJob retrieves a job by ID
func (s *Scheduler) GetJob(jobID int64) (*Job, bool) {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	job, exists := s.jobs[jobID]
	return job, exists
}

// ListJobs returns all jobs
func (s *Scheduler) ListJobs() []*Job {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()

	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

// GetStats returns scheduler statistics
func (s *Scheduler) GetStats() map[string]interface{} {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()

	stats := map[string]interface{}{
		"total_jobs":     len(s.jobs),
		"queue_size":     len(s.jobQueue),
		"queue_capacity": s.config.QueueSize,
		"workers":        s.config.WorkerCount,
		"by_status":      make(map[string]int),
	}

	byStatus := stats["by_status"].(map[string]int)
	for _, job := range s.jobs {
		byStatus[job.Status]++
	}

	return stats
}

// Shutdown gracefully shuts down the scheduler
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.logger.Info("Shutting down scheduler")

	// Signal shutdown
	close(s.shutdownCh)

	// Close job queue
	close(s.jobQueue)

	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.Info("Scheduler shutdown completed")
	case <-ctx.Done():
		s.logger.Warn("Scheduler shutdown timeout")
		return ctx.Err()
	}

	// Cancel all remaining jobs
	s.jobsMu.Lock()
	for _, job := range s.jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
	s.jobsMu.Unlock()

	return nil
}

// Helper methods

func (s *Scheduler) trackJob(job *Job) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	s.jobs[job.ID] = job
}

func (s *Scheduler) untrackJob(jobID int64) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	delete(s.jobs, jobID)
}

func (s *Scheduler) updateJobStatus(jobID int64, status string) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if job, exists := s.jobs[jobID]; exists {
		job.Status = status
		if status == "running" && job.StartedAt.IsZero() {
			job.StartedAt = time.Now()
		} else if status == "finished" || status == "failed" {
			job.FinishedAt = time.Now()
		}
	}
}

// cleanupRoutine periodically cleans up finished jobs
func (s *Scheduler) cleanupRoutine() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanup()
		case <-s.shutdownCh:
			return
		}
	}
}

// cleanup removes old finished jobs
func (s *Scheduler) cleanup() {
	s.logger.Debug("Running job cleanup")

	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()

	maxAge := 1 * time.Hour
	now := time.Now()

	for id, job := range s.jobs {
		if (job.Status == "finished" || job.Status == "failed") &&
			!job.FinishedAt.IsZero() &&
			now.Sub(job.FinishedAt) > maxAge {

			s.logger.WithField("job_id", id).Debug("Cleaning up old job")

			// Cancel context if not already done
			if job.cancel != nil {
				job.cancel()
			}

			delete(s.jobs, id)
		}
	}
}

// Worker implementation

func (w *Worker) run() {
	defer w.scheduler.wg.Done()

	w.logger.Info("Worker started")

	for {
		select {
		case job, ok := <-w.scheduler.jobQueue:
			if !ok {
				w.logger.Info("Job queue closed, worker stopping")
				return
			}
			w.processJob(job)

		case <-w.shutdownCh:
			w.logger.Info("Worker shutdown signal received")
			return
		}
	}
}

func (w *Worker) processJob(job *Job) {
	w.logger.WithFields(logrus.Fields{
		"job_id":     job.ID,
		"project_id": job.ProjectID,
		"vcpu":       job.VCPU,
		"memory_mb":  job.MemoryMB,
	}).Info("Processing job")

	// Update status
	w.scheduler.updateJobStatus(job.ID, "running")

	// Create VM
	vm, err := w.createVM(job)
	if err != nil {
		w.logger.WithError(err).Error("Failed to create VM for job")
		w.scheduler.updateJobStatus(job.ID, "failed")
		job.err = err
		return
	}

	job.VM = vm
	job.VMID = vm.ID

	// Register GitLab runner
	if err := w.registerRunner(job); err != nil {
		w.logger.WithError(err).Error("Failed to register runner")
		w.scheduler.updateJobStatus(job.ID, "failed")
		job.err = err
		w.cleanupVM(job)
		return
	}

	// Wait for job completion or timeout
	w.waitForJobCompletion(job)

	// Cleanup
	w.cleanupVM(job)

	// Update final status
	if job.err != nil {
		w.scheduler.updateJobStatus(job.ID, "failed")
	} else {
		w.scheduler.updateJobStatus(job.ID, "finished")
	}

	w.logger.WithField("job_id", job.ID).Info("Job processing completed")
}

func (w *Worker) createVM(job *Job) (*firecracker.MicroVM, error) {
	req := &firecracker.VMRequest{
		JobID:     fmt.Sprintf("%d", job.ID),
		ProjectID: fmt.Sprintf("%d", job.ProjectID),
		VCPU:      job.VCPU,
		MemoryMB:  job.MemoryMB,
		Tags:      job.Tags,
		Metadata: map[string]string{
			"job_id":      fmt.Sprintf("%d", job.ID),
			"project_id":  fmt.Sprintf("%d", job.ProjectID),
			"pipeline_id": fmt.Sprintf("%d", job.PipelineID),
		},
	}

	ctx, cancel := context.WithTimeout(job.ctx, w.scheduler.config.VMStartTimeout)
	defer cancel()

	return w.scheduler.vmManager.CreateVM(ctx, req)
}

func (w *Worker) registerRunner(job *Job) error {
	// TODO: Implement GitLab runner registration
	// This would use the GitLab API to register a runner for the specific job
	w.logger.WithField("vm_ip", job.VM.IPAddress).Info("Registering GitLab runner")
	return nil
}

func (w *Worker) waitForJobCompletion(job *Job) {
	// TODO: Implement job completion monitoring
	// This would poll GitLab API or use webhooks to detect job completion

	// For now, simulate job execution
	select {
	case <-job.ctx.Done():
		w.logger.WithField("job_id", job.ID).Warn("Job timeout reached")
		job.err = fmt.Errorf("job timeout")
	case <-time.After(30 * time.Second):
		// Simulated job completion
		w.logger.WithField("job_id", job.ID).Info("Job completed")
	}
}

func (w *Worker) cleanupVM(job *Job) {
	if job.VMID == "" {
		return
	}

	w.logger.WithField("vm_id", job.VMID).Info("Cleaning up VM")

	ctx, cancel := context.WithTimeout(context.Background(), w.scheduler.config.VMShutdownTimeout)
	defer cancel()

	if err := w.scheduler.vmManager.DestroyVM(ctx, job.VMID); err != nil {
		w.logger.WithError(err).Error("Failed to cleanup VM")
	}
}
