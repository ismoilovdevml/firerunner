package gitlab

import (
	"fmt"
	"strings"
	"time"
)

// JobEvent represents a GitLab Job Hook event
type JobEvent struct {
	ObjectKind         string     `json:"object_kind"`
	Ref                string     `json:"ref"`
	Tag                bool       `json:"tag"`
	BeforeSHA          string     `json:"before_sha"`
	SHA                string     `json:"sha"`
	BuildID            int64      `json:"build_id"`
	BuildName          string     `json:"build_name"`
	BuildStage         string     `json:"build_stage"`
	BuildStatus        string     `json:"build_status"`
	BuildCreatedAt     time.Time  `json:"build_created_at"`
	BuildStartedAt     time.Time  `json:"build_started_at"`
	BuildFinishedAt    time.Time  `json:"build_finished_at"`
	BuildDuration      float64    `json:"build_duration"`
	BuildAllowFailure  bool       `json:"build_allow_failure"`
	BuildFailureReason string     `json:"build_failure_reason"`
	PipelineID         int64      `json:"pipeline_id"`
	RunnerID           int64      `json:"runner_id"`
	RunnerDescription  string     `json:"runner_description"`
	RunnerActive       bool       `json:"runner_active"`
	RunnerIsShared     bool       `json:"runner_is_shared"`
	RunnerTags         []string   `json:"runner_tags"`
	ProjectID          int64      `json:"project_id"`
	ProjectName        string     `json:"project_name"`
	User               User       `json:"user"`
	Commit             Commit     `json:"commit"`
	Repository         Repository `json:"repository"`
	Environment        string     `json:"environment"`
	BuildTags          []string   `json:"tags"`
}

// PipelineEvent represents a GitLab Pipeline Hook event
type PipelineEvent struct {
	ObjectKind       string             `json:"object_kind"`
	ObjectAttributes PipelineAttributes `json:"object_attributes"`
	MergeRequest     MergeRequest       `json:"merge_request"`
	User             User               `json:"user"`
	Project          Project            `json:"project"`
	Commit           Commit             `json:"commit"`
	Builds           []Build            `json:"builds"`
}

// PipelineAttributes represents pipeline attributes
type PipelineAttributes struct {
	ID         int64      `json:"id"`
	Ref        string     `json:"ref"`
	Tag        bool       `json:"tag"`
	SHA        string     `json:"sha"`
	BeforeSHA  string     `json:"before_sha"`
	Source     string     `json:"source"`
	Status     string     `json:"status"`
	Stages     []string   `json:"stages"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt time.Time  `json:"finished_at"`
	Duration   int        `json:"duration"`
	Variables  []Variable `json:"variables"`
}

// User represents a GitLab user
type User struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

// Commit represents a Git commit
type Commit struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	Title     string    `json:"title"`
	Timestamp time.Time `json:"timestamp"`
	URL       string    `json:"url"`
	Author    Author    `json:"author"`
}

// Author represents a commit author
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Repository represents a Git repository
type Repository struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	Description     string `json:"description"`
	Homepage        string `json:"homepage"`
	GitHTTPURL      string `json:"git_http_url"`
	GitSSHURL       string `json:"git_ssh_url"`
	VisibilityLevel int    `json:"visibility_level"`
}

// Project represents a GitLab project
type Project struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	WebURL            string `json:"web_url"`
	AvatarURL         string `json:"avatar_url"`
	GitSSHURL         string `json:"git_ssh_url"`
	GitHTTPURL        string `json:"git_http_url"`
	Namespace         string `json:"namespace"`
	VisibilityLevel   int    `json:"visibility_level"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
}

// MergeRequest represents a GitLab merge request
type MergeRequest struct {
	ID              int64  `json:"id"`
	IID             int64  `json:"iid"`
	Title           string `json:"title"`
	SourceBranch    string `json:"source_branch"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetBranch    string `json:"target_branch"`
	TargetProjectID int64  `json:"target_project_id"`
	State           string `json:"state"`
	MergeStatus     string `json:"merge_status"`
	URL             string `json:"url"`
}

// Build represents a pipeline build/job
type Build struct {
	ID            int64         `json:"id"`
	Stage         string        `json:"stage"`
	Name          string        `json:"name"`
	Status        string        `json:"status"`
	CreatedAt     time.Time     `json:"created_at"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	When          string        `json:"when"`
	Manual        bool          `json:"manual"`
	AllowFailure  bool          `json:"allow_failure"`
	User          User          `json:"user"`
	Runner        Runner        `json:"runner"`
	ArtifactsFile ArtifactsFile `json:"artifacts_file"`
	Environment   Environment   `json:"environment"`
}

// Runner represents a GitLab runner
type Runner struct {
	ID          int64    `json:"id"`
	Description string   `json:"description"`
	Active      bool     `json:"active"`
	IsShared    bool     `json:"is_shared"`
	Tags        []string `json:"tags"`
}

// ArtifactsFile represents build artifacts
type ArtifactsFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// Environment represents a deployment environment
type Environment struct {
	Name           string `json:"name"`
	Action         string `json:"action"`
	DeploymentTier string `json:"deployment_tier"`
}

// Variable represents a CI/CD variable
type Variable struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RunnerRegistration represents a GitLab runner registration
type RunnerRegistration struct {
	ID          int64    `json:"id"`
	Token       string   `json:"token"`
	Description string   `json:"description"`
	Active      bool     `json:"active"`
	IsShared    bool     `json:"is_shared"`
	RunnerType  string   `json:"runner_type"`
	Tags        []string `json:"tag_list"`
	Locked      bool     `json:"locked"`
}

// ParseVMRequirements extracts VM requirements from job tags
func ParseVMRequirements(tags []string) (vcpu int64, memoryMB int64) {
	// Default values
	vcpu = 2
	memoryMB = 4096

	for _, tag := range tags {
		// Format: actuated-4cpu-8gb or firecracker-2cpu-4gb
		if strings.Contains(tag, "cpu") && strings.Contains(tag, "gb") {
			parts := strings.Split(tag, "-")
			for i, part := range parts {
				if strings.HasSuffix(part, "cpu") {
					if cpu, err := parseInt(strings.TrimSuffix(part, "cpu")); err == nil {
						vcpu = cpu
					}
				}
				if strings.HasSuffix(part, "gb") && i > 0 {
					if mem, err := parseInt(strings.TrimSuffix(part, "gb")); err == nil {
						memoryMB = mem * 1024
					}
				}
			}
		}
	}

	return vcpu, memoryMB
}

// Helper function to parse integer from string
func parseInt(s string) (int64, error) {
	var result int64
	_, err := fmt.Sscanf(s, "%d", &result)
	return result, err
}
