package gitlab

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xanzy/go-gitlab"
)

// GitLabJobService interface for job operations
type GitLabJobService interface {
	GetJob(ctx context.Context, projectID, jobID int64) (*gitlab.Job, error)
}

// JobMonitor monitors GitLab job status
type JobMonitor struct {
	service GitLabJobService
	logger  *logrus.Logger
}

// NewJobMonitor creates a new job monitor
func NewJobMonitor(service GitLabJobService, logger *logrus.Logger) *JobMonitor {
	return &JobMonitor{
		service: service,
		logger:  logger,
	}
}

// WaitForJobCompletion waits for a job to complete
func (jm *JobMonitor) WaitForJobCompletion(ctx context.Context, projectID, jobID int64, pollInterval time.Duration) (*gitlab.Job, error) {
	jm.logger.WithFields(logrus.Fields{
		"project_id": projectID,
		"job_id":     jobID,
	}).Info("Starting job completion monitoring")

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			jm.logger.WithField("job_id", jobID).Warn("Job monitoring context cancelled")
			return nil, ctx.Err()

		case <-ticker.C:
			job, err := jm.service.GetJob(ctx, projectID, jobID)
			if err != nil {
				jm.logger.WithError(err).WithField("job_id", jobID).Error("Failed to get job status")
				continue
			}

			jm.logger.WithFields(logrus.Fields{
				"job_id": jobID,
				"status": job.Status,
				"stage":  job.Stage,
			}).Debug("Job status check")

			// Check if job has completed
			if jm.isJobComplete(job.Status) {
				jm.logger.WithFields(logrus.Fields{
					"job_id":   jobID,
					"status":   job.Status,
					"duration": job.Duration,
				}).Info("Job completed")
				return job, nil
			}

			// Check for job failure
			if jm.isJobFailed(job.Status) {
				jm.logger.WithFields(logrus.Fields{
					"job_id": jobID,
					"status": job.Status,
				}).Warn("Job failed")
				return job, fmt.Errorf("job %d failed with status: %s", jobID, job.Status)
			}
		}
	}
}

// isJobComplete checks if job is in a completed state
func (jm *JobMonitor) isJobComplete(status string) bool {
	completedStatuses := []string{
		"success",
		"failed",
		"canceled",
		"skipped",
	}

	for _, s := range completedStatuses {
		if status == s {
			return true
		}
	}
	return false
}

// isJobFailed checks if job failed
func (jm *JobMonitor) isJobFailed(status string) bool {
	failedStatuses := []string{
		"failed",
		"canceled",
	}

	for _, s := range failedStatuses {
		if status == s {
			return true
		}
	}
	return false
}

// Note: GetJobLogs and CancelJob are commented out for now
// as they require direct client access which breaks the interface abstraction
// Implement these methods in the Service struct if needed
