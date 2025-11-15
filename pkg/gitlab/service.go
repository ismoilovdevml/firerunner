package gitlab

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/xanzy/go-gitlab"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

// Service provides GitLab API operations
type Service struct {
	client *gitlab.Client
	config *config.GitLabConfig
	logger *logrus.Logger
}

// NewService creates a new GitLab service
func NewService(cfg *config.GitLabConfig, logger *logrus.Logger) (*Service, error) {
	client, err := gitlab.NewClient(cfg.Token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		return nil, fmt.Errorf("failed to create GitLab client: %w", err)
	}

	return &Service{
		client: client,
		config: cfg,
		logger: logger,
	}, nil
}

// RegisterRunner registers a new runner for a specific project
func (s *Service) RegisterRunner(ctx context.Context, projectID int64, vmIP string, tags []string) (*RunnerRegistration, error) {
	s.logger.WithFields(logrus.Fields{
		"project_id": projectID,
		"vm_ip":      vmIP,
		"tags":       tags,
	}).Info("Registering GitLab runner")

	// Merge default tags with job-specific tags
	allTags := append(s.config.RunnerTags, tags...)

	// TODO: Implement actual GitLab runner registration
	// This requires using the GitLab Runner registration API
	// For now, return a mock registration

	registration := &RunnerRegistration{
		ID:          1234,
		Token:       "mock-runner-token",
		Description: fmt.Sprintf("FireRunner VM at %s", vmIP),
		Active:      true,
		IsShared:    false,
		RunnerType:  "project_type",
		Tags:        allTags,
		Locked:      true,
	}

	return registration, nil
}

// UnregisterRunner unregisters a runner
func (s *Service) UnregisterRunner(ctx context.Context, runnerID int64) error {
	s.logger.WithField("runner_id", runnerID).Info("Unregistering GitLab runner")

	// TODO: Implement actual runner unregistration
	// Use GitLab API to delete the runner

	return nil
}

// GetJob retrieves job information
func (s *Service) GetJob(ctx context.Context, projectID, jobID int64) (*gitlab.Job, error) {
	job, _, err := s.client.Jobs.GetJob(int(projectID), int(jobID))
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	return job, nil
}

// GetProject retrieves project information
func (s *Service) GetProject(ctx context.Context, projectID int64) (*gitlab.Project, error) {
	project, _, err := s.client.Projects.GetProject(int(projectID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return project, nil
}

// ListProjectRunners lists all runners for a project
func (s *Service) ListProjectRunners(ctx context.Context, projectID int64) ([]*gitlab.Runner, error) {
	runners, _, err := s.client.Runners.ListProjectRunners(int(projectID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list project runners: %w", err)
	}

	return runners, nil
}

// Health checks GitLab API connectivity
func (s *Service) Health(ctx context.Context) error {
	_, _, err := s.client.Version.GetVersion()
	if err != nil {
		return fmt.Errorf("GitLab health check failed: %w", err)
	}

	return nil
}

// ProcessJobEvent processes a job event (implements EventProcessor)
func (s *Service) ProcessJobEvent(event *JobEvent) error {
	s.logger.WithFields(logrus.Fields{
		"job_id":     event.BuildID,
		"project_id": event.ProjectID,
		"status":     event.BuildStatus,
	}).Info("Processing job event")

	// This would be called by the webhook handler
	// The actual job scheduling is done by the scheduler

	return nil
}

// ProcessPipelineEvent processes a pipeline event (implements EventProcessor)
func (s *Service) ProcessPipelineEvent(event *PipelineEvent) error {
	s.logger.WithFields(logrus.Fields{
		"pipeline_id": event.ObjectAttributes.ID,
		"project_id":  event.Project.ID,
		"status":      event.ObjectAttributes.Status,
	}).Debug("Processing pipeline event")

	// This is primarily for logging/metrics
	// Job events are where we actually schedule VMs

	return nil
}
