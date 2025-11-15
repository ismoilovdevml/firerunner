package gitlab

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/xanzy/go-gitlab"

	"github.com/ismoilovdevml/firerunner/pkg/config"
)

type Service struct {
	client *gitlab.Client
	config *config.GitLabConfig
	logger *logrus.Logger
}

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

func (s *Service) RegisterRunner(ctx context.Context, projectID int64, vmIP string, tags []string) (*RunnerRegistration, error) {
	s.logger.WithFields(logrus.Fields{
		"project_id": projectID,
		"vm_ip":      vmIP,
		"tags":       tags,
	}).Info("Registering ephemeral GitLab runner")

	allTags := append(s.config.RunnerTags, tags...)

	opts := &gitlab.RegisterNewRunnerOptions{
		Token:       gitlab.Ptr(s.config.Token),
		Description: gitlab.Ptr(fmt.Sprintf("FireRunner-VM-%s", vmIP)),
		Active:      gitlab.Ptr(true),
		Locked:      gitlab.Ptr(true),
		RunUntagged: gitlab.Ptr(false),
		TagList:     &allTags,
	}

	runner, _, err := s.client.Runners.RegisterNewRunner(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to register runner via GitLab API: %w", err)
	}

	s.logger.WithFields(logrus.Fields{
		"runner_id":    runner.ID,
		"runner_token": "***",
		"tags":         allTags,
	}).Info("Runner registered successfully")

	registration := &RunnerRegistration{
		ID:          int64(runner.ID),
		Token:       runner.Token,
		Description: fmt.Sprintf("FireRunner-VM-%s", vmIP),
		Active:      true,
		IsShared:    false,
		RunnerType:  "project_type",
		Tags:        allTags,
		Locked:      true,
	}

	return registration, nil
}

func (s *Service) UnregisterRunner(ctx context.Context, runnerID int64) error {
	s.logger.WithField("runner_id", runnerID).Info("Unregistering GitLab runner")

	_, err := s.client.Runners.RemoveRunner(int(runnerID))
	if err != nil {
		return fmt.Errorf("failed to unregister runner %d: %w", runnerID, err)
	}

	s.logger.WithField("runner_id", runnerID).Info("Runner unregistered successfully")
	return nil
}

func (s *Service) GetJob(ctx context.Context, projectID, jobID int64) (*gitlab.Job, error) {
	job, _, err := s.client.Jobs.GetJob(int(projectID), int(jobID))
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	return job, nil
}

func (s *Service) GetProject(ctx context.Context, projectID int64) (*gitlab.Project, error) {
	project, _, err := s.client.Projects.GetProject(int(projectID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	return project, nil
}

func (s *Service) ListProjectRunners(ctx context.Context, projectID int64) ([]*gitlab.Runner, error) {
	runners, _, err := s.client.Runners.ListProjectRunners(int(projectID), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list project runners: %w", err)
	}

	return runners, nil
}

func (s *Service) Health(ctx context.Context) error {
	_, _, err := s.client.Version.GetVersion()
	if err != nil {
		return fmt.Errorf("GitLab health check failed: %w", err)
	}

	return nil
}

func (s *Service) ProcessJobEvent(event *JobEvent) error {
	s.logger.WithFields(logrus.Fields{
		"job_id":     event.BuildID,
		"project_id": event.ProjectID,
		"status":     event.BuildStatus,
	}).Info("Processing job event")

	return nil
}

func (s *Service) ProcessPipelineEvent(event *PipelineEvent) error {
	s.logger.WithFields(logrus.Fields{
		"pipeline_id": event.ObjectAttributes.ID,
		"project_id":  event.Project.ID,
		"status":      event.ObjectAttributes.Status,
	}).Debug("Processing pipeline event")

	return nil
}
