package gitlab

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

type mockEventProcessor struct {
	jobCalled      bool
	pipelineCalled bool
}

func (m *mockEventProcessor) ProcessJobEvent(event *JobEvent) error {
	m.jobCalled = true
	return nil
}

func (m *mockEventProcessor) ProcessPipelineEvent(event *PipelineEvent) error {
	m.pipelineCalled = true
	return nil
}

func TestWebhookHandler_ServeHTTP(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)

	tests := []struct {
		name           string
		method         string
		secret         string
		headerToken    string
		eventType      string
		body           string
		expectedStatus int
	}{
		{
			name:           "valid job event",
			method:         http.MethodPost,
			secret:         "test-secret",
			headerToken:    "test-secret",
			eventType:      "Job Hook",
			body:           `{"build_id":123,"tags":["firecracker-2cpu-4gb"],"build_status":"pending"}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid method",
			method:         http.MethodGet,
			secret:         "test-secret",
			headerToken:    "test-secret",
			eventType:      "Job Hook",
			body:           `{}`,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid secret",
			method:         http.MethodPost,
			secret:         "test-secret",
			headerToken:    "wrong-secret",
			eventType:      "Job Hook",
			body:           `{}`,
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "missing event type",
			method:         http.MethodPost,
			secret:         "test-secret",
			headerToken:    "test-secret",
			eventType:      "",
			body:           `{}`,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &mockEventProcessor{}
			handler := NewWebhookHandler(tt.secret, logger, processor)

			req := httptest.NewRequest(tt.method, "/webhook", bytes.NewBufferString(tt.body))
			if tt.eventType != "" {
				req.Header.Set(HeaderGitLabEvent, tt.eventType)
			}
			if tt.headerToken != "" {
				req.Header.Set(HeaderGitLabToken, tt.headerToken)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}
		})
	}
}

func TestWebhookHandler_HealthCheck(t *testing.T) {
	logger := logrus.New()
	processor := &mockEventProcessor{}
	handler := NewWebhookHandler("secret", logger, processor)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()

	handler.HealthCheck(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}

	expected := `{"status":"healthy"}`
	if rr.Body.String() != expected {
		t.Errorf("Expected body %s, got %s", expected, rr.Body.String())
	}
}

func TestHasFireRunnerTag(t *testing.T) {
	handler := &WebhookHandler{}

	tests := []struct {
		name     string
		tags     []string
		expected bool
	}{
		{
			name:     "has firecracker tag",
			tags:     []string{"firecracker-2cpu-4gb"},
			expected: true,
		},
		{
			name:     "has microvm tag",
			tags:     []string{"docker", "microvm"},
			expected: true,
		},
		{
			name:     "has firerunner tag",
			tags:     []string{"firerunner-4cpu-8gb"},
			expected: true,
		},
		{
			name:     "no relevant tags",
			tags:     []string{"docker", "kubernetes"},
			expected: false,
		},
		{
			name:     "empty tags",
			tags:     []string{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := handler.hasFireRunnerTag(tt.tags)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestParseVMRequirements(t *testing.T) {
	tests := []struct {
		name           string
		tags           []string
		expectedVCPU   int64
		expectedMemory int64
	}{
		{
			name:           "standard 2cpu 4gb",
			tags:           []string{"firecracker-2cpu-4gb"},
			expectedVCPU:   2,
			expectedMemory: 4096,
		},
		{
			name:           "large 8cpu 16gb",
			tags:           []string{"actuated-8cpu-16gb"},
			expectedVCPU:   8,
			expectedMemory: 16384,
		},
		{
			name:           "no vm tags - defaults",
			tags:           []string{"docker"},
			expectedVCPU:   2,
			expectedMemory: 4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vcpu, mem := ParseVMRequirements(tt.tags)
			if vcpu != tt.expectedVCPU {
				t.Errorf("Expected VCPU %d, got %d", tt.expectedVCPU, vcpu)
			}
			if mem != tt.expectedMemory {
				t.Errorf("Expected memory %d, got %d", tt.expectedMemory, mem)
			}
		})
	}
}
