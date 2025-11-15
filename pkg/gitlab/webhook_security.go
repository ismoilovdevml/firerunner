package gitlab

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

type SecurityConfig struct {
	Secret                    string
	RequireSecret             bool
	MaxBodySize               int64
	RateLimitPerMinute        int
	AllowedIPs                []string
	RequireSSL                bool
	TimestampToleranceMinutes int
}

func DefaultSecurityConfig(secret string) *SecurityConfig {
	return &SecurityConfig{
		Secret:                    secret,
		RequireSecret:             true,
		MaxBodySize:               10 * 1024 * 1024, // 10MB
		RateLimitPerMinute:        60,
		AllowedIPs:                []string{}, // Empty = allow all
		RequireSSL:                false,      // Set to true in production with HTTPS
		TimestampToleranceMinutes: 5,
	}
}

type SecureWebhookHandler struct {
	handler       *WebhookHandler
	security      *SecurityConfig
	logger        *logrus.Logger
	requestCounts map[string]int // IP -> count (simple rate limiting)
	lastReset     time.Time
}

func NewSecureWebhookHandler(
	secret string,
	logger *logrus.Logger,
	processor EventProcessor,
) *SecureWebhookHandler {
	return &SecureWebhookHandler{
		handler:       NewWebhookHandler(secret, logger, processor),
		security:      DefaultSecurityConfig(secret),
		logger:        logger,
		requestCounts: make(map[string]int),
		lastReset:     time.Now(),
	}
}

func (h *SecureWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.security.RequireSSL && r.TLS == nil {
		h.logger.Warn("Rejected non-HTTPS request")
		http.Error(w, "HTTPS required", http.StatusForbidden)
		return
	}

	if !h.isIPAllowed(r.RemoteAddr) {
		h.logger.WithField("ip", r.RemoteAddr).Warn("Rejected request from non-whitelisted IP")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if !h.checkRateLimit(r.RemoteAddr) {
		h.logger.WithField("ip", r.RemoteAddr).Warn("Rate limit exceeded")
		http.Error(w, "Too many requests", http.StatusTooManyRequests)
		return
	}

	if r.ContentLength > h.security.MaxBodySize {
		h.logger.WithField("size", r.ContentLength).Warn("Request body too large")
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if h.security.RequireSecret && h.security.Secret != "" {
		if !h.verifySignature(r) {
			h.logger.Warn("Invalid webhook signature")
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	h.handler.ServeHTTP(w, r)
}

func (h *SecureWebhookHandler) verifySignature(r *http.Request) bool {
	token := r.Header.Get(HeaderGitLabToken)
	if token != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(h.security.Secret)) == 1
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if signature != "" {
		return h.verifyHMACSHA256(r, signature)
	}

	return false
}

func (h *SecureWebhookHandler) verifyHMACSHA256(r *http.Request, signature string) bool {
	if len(signature) > 7 && signature[:7] == "sha256=" {
		signature = signature[7:]
	}

	body := make([]byte, r.ContentLength)
	if _, err := r.Body.Read(body); err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.security.Secret))
	mac.Write(body)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))

	return subtle.ConstantTimeCompare([]byte(signature), []byte(expectedMAC)) == 1
}

func (h *SecureWebhookHandler) isIPAllowed(remoteAddr string) bool {
	if len(h.security.AllowedIPs) == 0 {
		return true
	}

	ip := remoteAddr
	if colon := len(remoteAddr) - 1; colon >= 0 {
		for i := len(remoteAddr) - 1; i >= 0; i-- {
			if remoteAddr[i] == ':' {
				ip = remoteAddr[:i]
				break
			}
		}
	}

	for _, allowedIP := range h.security.AllowedIPs {
		if ip == allowedIP {
			return true
		}
	}

	return false
}

func (h *SecureWebhookHandler) checkRateLimit(remoteAddr string) bool {
	if time.Since(h.lastReset) > time.Minute {
		h.requestCounts = make(map[string]int)
		h.lastReset = time.Now()
	}

	h.requestCounts[remoteAddr]++

	return h.requestCounts[remoteAddr] <= h.security.RateLimitPerMinute
}

func (h *SecureWebhookHandler) GetSecurityConfig() *SecurityConfig {
	return h.security
}

func (h *SecureWebhookHandler) UpdateSecurityConfig(config *SecurityConfig) {
	h.security = config
}

func ValidateWebhookPayload(eventType string, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}

	if eventType == "" {
		return fmt.Errorf("missing event type")
	}

	switch eventType {
	case "Job Hook":
		if len(payload) < 50 {
			return fmt.Errorf("payload too small for job event")
		}
	case "Pipeline Hook":
		if len(payload) < 50 {
			return fmt.Errorf("payload too small for pipeline event")
		}
	}

	return nil
}
