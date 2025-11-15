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

// SecurityConfig holds webhook security configuration
type SecurityConfig struct {
	Secret                    string
	RequireSecret             bool
	MaxBodySize               int64
	RateLimitPerMinute        int
	AllowedIPs                []string
	RequireSSL                bool
	TimestampToleranceMinutes int
}

// DefaultSecurityConfig returns recommended production security settings
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

// SecureWebhookHandler wraps webhook handler with security features
type SecureWebhookHandler struct {
	handler       *WebhookHandler
	security      *SecurityConfig
	logger        *logrus.Logger
	requestCounts map[string]int // IP -> count (simple rate limiting)
	lastReset     time.Time
}

// NewSecureWebhookHandler creates a secure webhook handler
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

// ServeHTTP handles incoming webhook requests with security checks
func (h *SecureWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Check SSL if required
	if h.security.RequireSSL && r.TLS == nil {
		h.logger.Warn("Rejected non-HTTPS request")
		http.Error(w, "HTTPS required", http.StatusForbidden)
		return
	}

	// 2. Check IP whitelist
	if !h.isIPAllowed(r.RemoteAddr) {
		h.logger.WithField("ip", r.RemoteAddr).Warn("Rejected request from non-whitelisted IP")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// 3. Rate limiting
	if !h.checkRateLimit(r.RemoteAddr) {
		h.logger.WithField("ip", r.RemoteAddr).Warn("Rate limit exceeded")
		http.Error(w, "Too many requests", http.StatusTooManyRequests)
		return
	}

	// 4. Check body size
	if r.ContentLength > h.security.MaxBodySize {
		h.logger.WithField("size", r.ContentLength).Warn("Request body too large")
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// 5. Verify HMAC signature (GitLab token or signature)
	if h.security.RequireSecret && h.security.Secret != "" {
		if !h.verifySignature(r) {
			h.logger.Warn("Invalid webhook signature")
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// All security checks passed - delegate to actual handler
	h.handler.ServeHTTP(w, r)
}

// verifySignature verifies webhook signature
func (h *SecureWebhookHandler) verifySignature(r *http.Request) bool {
	// GitLab sends X-Gitlab-Token header
	token := r.Header.Get(HeaderGitLabToken)
	if token != "" {
		// Simple token comparison with constant-time comparison
		return subtle.ConstantTimeCompare([]byte(token), []byte(h.security.Secret)) == 1
	}

	// Also check for X-Hub-Signature-256 (GitHub style)
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature != "" {
		return h.verifyHMACSHA256(r, signature)
	}

	// No valid signature found
	return false
}

// verifyHMACSHA256 verifies HMAC-SHA256 signature
func (h *SecureWebhookHandler) verifyHMACSHA256(r *http.Request, signature string) bool {
	// Remove "sha256=" prefix if present
	if len(signature) > 7 && signature[:7] == "sha256=" {
		signature = signature[7:]
	}

	// Read body (we'll need to reset it)
	body := make([]byte, r.ContentLength)
	if _, err := r.Body.Read(body); err != nil {
		return false
	}

	// Compute HMAC
	mac := hmac.New(sha256.New, []byte(h.security.Secret))
	mac.Write(body)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))

	// Constant-time comparison
	return subtle.ConstantTimeCompare([]byte(signature), []byte(expectedMAC)) == 1
}

// isIPAllowed checks if IP is whitelisted
func (h *SecureWebhookHandler) isIPAllowed(remoteAddr string) bool {
	// If no whitelist configured, allow all
	if len(h.security.AllowedIPs) == 0 {
		return true
	}

	// Extract IP from "IP:port"
	ip := remoteAddr
	if colon := len(remoteAddr) - 1; colon >= 0 {
		for i := len(remoteAddr) - 1; i >= 0; i-- {
			if remoteAddr[i] == ':' {
				ip = remoteAddr[:i]
				break
			}
		}
	}

	// Check whitelist
	for _, allowedIP := range h.security.AllowedIPs {
		if ip == allowedIP {
			return true
		}
	}

	return false
}

// checkRateLimit implements simple rate limiting
func (h *SecureWebhookHandler) checkRateLimit(remoteAddr string) bool {
	// Reset counts every minute
	if time.Since(h.lastReset) > time.Minute {
		h.requestCounts = make(map[string]int)
		h.lastReset = time.Now()
	}

	// Increment count
	h.requestCounts[remoteAddr]++

	// Check limit
	return h.requestCounts[remoteAddr] <= h.security.RateLimitPerMinute
}

// GetSecurityConfig returns current security configuration
func (h *SecureWebhookHandler) GetSecurityConfig() *SecurityConfig {
	return h.security
}

// UpdateSecurityConfig updates security configuration
func (h *SecureWebhookHandler) UpdateSecurityConfig(config *SecurityConfig) {
	h.security = config
}

// ValidateWebhookPayload validates webhook payload structure
func ValidateWebhookPayload(eventType string, payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}

	if eventType == "" {
		return fmt.Errorf("missing event type")
	}

	// Add more validation based on event type
	switch eventType {
	case "Job Hook":
		// Validate job event structure
		if len(payload) < 50 {
			return fmt.Errorf("payload too small for job event")
		}
	case "Pipeline Hook":
		// Validate pipeline event structure
		if len(payload) < 50 {
			return fmt.Errorf("payload too small for pipeline event")
		}
	}

	return nil
}
