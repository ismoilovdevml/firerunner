package gitlab

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	HeaderGitLabEvent = "X-Gitlab-Event"
	HeaderGitLabToken = "X-Gitlab-Token"
)

type WebhookHandler struct {
	secret    string
	logger    *logrus.Logger
	processor EventProcessor
}

type EventProcessor interface {
	ProcessJobEvent(event *JobEvent) error
	ProcessPipelineEvent(event *PipelineEvent) error
}

func NewWebhookHandler(secret string, logger *logrus.Logger, processor EventProcessor) *WebhookHandler {
	return &WebhookHandler{
		secret:    secret,
		logger:    logger,
		processor: processor,
	}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.WithError(err).Error("Failed to read webhook body")
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if !h.verifySignature(r, body) {
		h.logger.Warn("Invalid webhook signature")
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get(HeaderGitLabEvent)
	if eventType == "" {
		h.logger.Warn("Missing X-Gitlab-Event header")
		http.Error(w, "Missing event type", http.StatusBadRequest)
		return
	}

	h.logger.WithField("event_type", eventType).Debug("Received webhook event")

	if err := h.processEvent(eventType, body); err != nil {
		h.logger.WithError(err).Error("Failed to process webhook event")
		http.Error(w, "Failed to process event", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"accepted"}`))
}

func (h *WebhookHandler) verifySignature(r *http.Request, body []byte) bool {
	if h.secret == "" {
		return true
	}

	receivedToken := r.Header.Get(HeaderGitLabToken)
	if receivedToken == "" {
		h.logger.Warn("Missing X-Gitlab-Token header")
		return false
	}

	if receivedToken == h.secret {
		return true
	}

	signature := r.Header.Get("X-Gitlab-Signature")
	if signature != "" {
		return h.verifyHMAC(body, signature)
	}

	return false
}

func (h *WebhookHandler) verifyHMAC(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(signature), []byte(expectedMAC))
}

func (h *WebhookHandler) processEvent(eventType string, body []byte) error {
	switch eventType {
	case "Job Hook":
		return h.processJobEvent(body)
	case "Pipeline Hook":
		return h.processPipelineEvent(body)
	default:
		h.logger.WithField("event_type", eventType).Debug("Ignoring unsupported event type")
		return nil
	}
}

func (h *WebhookHandler) processJobEvent(body []byte) error {
	var event JobEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse job event: %w", err)
	}

	h.logger.WithFields(logrus.Fields{
		"build_id":     event.BuildID,
		"build_name":   event.BuildName,
		"build_status": event.BuildStatus,
		"project_id":   event.ProjectID,
		"project_name": event.ProjectName,
	}).Info("Processing job event")

	if event.BuildStatus != "pending" && event.BuildStatus != "created" {
		h.logger.WithField("status", event.BuildStatus).Debug("Ignoring non-pending job")
		return nil
	}

	if !h.hasFireRunnerTag(event.BuildTags) {
		h.logger.Debug("Job does not have firerunner tags, skipping")
		return nil
	}

	if h.processor != nil {
		return h.processor.ProcessJobEvent(&event)
	}

	return nil
}

func (h *WebhookHandler) processPipelineEvent(body []byte) error {
	var event PipelineEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse pipeline event: %w", err)
	}

	h.logger.WithFields(logrus.Fields{
		"pipeline_id": event.ObjectAttributes.ID,
		"status":      event.ObjectAttributes.Status,
		"project_id":  event.Project.ID,
	}).Debug("Processing pipeline event")

	if h.processor != nil {
		return h.processor.ProcessPipelineEvent(&event)
	}

	return nil
}

func (h *WebhookHandler) hasFireRunnerTag(tags []string) bool {
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if strings.HasPrefix(tag, "firecracker") ||
			strings.HasPrefix(tag, "microvm") ||
			strings.HasPrefix(tag, "firerunner") ||
			strings.HasPrefix(tag, "actuated") {
			return true
		}
	}
	return false
}

func (h *WebhookHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"healthy"}`))
}
