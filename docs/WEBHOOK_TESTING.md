# Webhook Testing & Validation Guide

This guide explains how to test and validate GitLab webhooks with FireRunner.

## Quick Test

### 1. Test Health Endpoint

```bash
curl http://localhost:8080/health
# Expected: {"status":"healthy"}
```

### 2. Test Webhook Endpoint (Without Security)

```bash
curl -X POST http://localhost:8080/webhook \
  -H "X-Gitlab-Event: Job Hook" \
  -H "Content-Type: application/json" \
  -d '{
    "object_kind": "build",
    "build_id": 123,
    "build_name": "test",
    "build_status": "pending",
    "project_id": 456,
    "project_name": "test-project",
    "tags": ["firecracker-2cpu-4gb"]
  }'
```

**Expected Response:**
```json
{"status":"accepted"}
```

### 3. Test Webhook with Secret Token

```bash
# Get your webhook secret
WEBHOOK_SECRET=$(cat /etc/firerunner/webhook-secret.txt)

curl -X POST http://localhost:8080/webhook \
  -H "X-Gitlab-Event: Job Hook" \
  -H "X-Gitlab-Token: $WEBHOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "object_kind": "build",
    "build_id": 123,
    "build_name": "test",
    "build_status": "pending",
    "project_id": 456,
    "project_name": "test-project",
    "tags": ["firecracker-2cpu-4gb"]
  }'
```

---

## GitLab Webhook Setup

### Step 1: Get Webhook Information

```bash
# Find your server IP
PUBLIC_IP=$(curl -s ifconfig.me)
echo "Webhook URL: http://${PUBLIC_IP}:8080/webhook"

# Get webhook secret
WEBHOOK_SECRET=$(cat /etc/firerunner/webhook-secret.txt)
echo "Secret Token: $WEBHOOK_SECRET"
```

### Step 2: Configure in GitLab

1. **Navigate to Project Settings**
   - Go to your GitLab project
   - Settings → Webhooks

2. **Add New Webhook**
   - **URL**: `http://YOUR_SERVER_IP:8080/webhook`
   - **Secret Token**: (paste webhook secret)
   - **Trigger Events**: ☑️ Job events
   - **SSL Verification**: ☐ Enable (disable if using HTTP)
   - Click "Add webhook"

3. **Test Webhook**
   - Click "Test" → "Job events"
   - Should show: **HTTP 200** ✅

### Step 3: Verify in Logs

```bash
# Watch FireRunner logs
sudo journalctl -u firerunner -f

# You should see:
# "Received webhook event" event_type="Job Hook"
```

---

## Troubleshooting

### Issue: "Connection Refused"

```bash
# Check FireRunner is running
sudo systemctl status firerunner

# Check port is listening
sudo netstat -tlnp | grep 8080
# Should show: tcp 0.0.0.0:8080 LISTEN

# Check firewall
sudo ufw status
sudo ufw allow 8080/tcp
```

### Issue: "Invalid Signature" (401)

```bash
# Verify secret matches
cat /etc/firerunner/config.yaml | grep webhook_secret
cat /etc/firerunner/webhook-secret.txt

# Test without signature first
curl -X POST http://localhost:8080/webhook \
  -H "X-Gitlab-Event: Job Hook" \
  -d '{}'

# If works without secret, check GitLab webhook secret matches
```

### Issue: "Webhook Not Triggered"

**Check GitLab:**
1. Go to project → Settings → Webhooks
2. Click "Edit" on your webhook
3. Scroll to "Recent Deliveries"
4. Check response codes:
   - **200**: Success ✅
   - **401**: Invalid secret
   - **500**: FireRunner error
   - **Connection timeout**: Firewall blocking

**Check FireRunner:**
```bash
# Enable debug logging
sudo nano /etc/firerunner/config.yaml
# Change: level: "debug"

sudo systemctl restart firerunner
sudo journalctl -u firerunner -f
```

### Issue: "Timeout" or "No Response"

```bash
# Check network connectivity
curl -v http://localhost:8080/health

# Check from GitLab server (if self-hosted)
curl -v http://YOUR_FIRERUNNER_IP:8080/health

# Common causes:
# 1. Firewall blocking port 8080
# 2. Wrong IP address
# 3. FireRunner not running
# 4. Network routing issues
```

---

## Advanced Testing

### Test with Actual GitLab Job

**1. Create .gitlab-ci.yml:**
```yaml
test-firerunner:
  script:
    - echo "Testing FireRunner"
    - hostname
    - uptime
    - docker --version
  tags:
    - firecracker-2cpu-4gb
```

**2. Commit and Push:**
```bash
git add .gitlab-ci.yml
git commit -m "Test FireRunner"
git push
```

**3. Monitor:**
```bash
# Terminal 1: Watch FireRunner logs
sudo journalctl -u firerunner -f

# Terminal 2: Watch Flintlock logs
sudo journalctl -u flintlock -f

# Terminal 3: Watch GitLab UI
# Go to CI/CD → Pipelines
```

**Expected Flow:**
1. GitLab creates job
2. Sends webhook to FireRunner
3. FireRunner receives webhook ✅
4. Creates VM via Flintlock
5. Registers GitLab runner
6. Job runs in VM
7. VM destroyed after job

---

## Webhook Payload Examples

### Job Event (Pending)

```json
{
  "object_kind": "build",
  "ref": "main",
  "tag": false,
  "before_sha": "abc123",
  "sha": "def456",
  "build_id": 12345,
  "build_name": "test",
  "build_stage": "test",
  "build_status": "pending",
  "build_created_at": "2024-01-01T00:00:00Z",
  "pipeline_id": 67890,
  "project_id": 111,
  "project_name": "my-project",
  "user": {
    "id": 1,
    "name": "Developer",
    "username": "dev",
    "email": "dev@example.com"
  },
  "commit": {
    "id": "def456",
    "message": "Test commit",
    "timestamp": "2024-01-01T00:00:00Z",
    "author": {
      "name": "Developer",
      "email": "dev@example.com"
    }
  },
  "repository": {
    "name": "my-project",
    "url": "https://gitlab.com/user/my-project.git",
    "homepage": "https://gitlab.com/user/my-project"
  },
  "tags": ["firecracker-2cpu-4gb", "docker"]
}
```

### Pipeline Event

```json
{
  "object_kind": "pipeline",
  "object_attributes": {
    "id": 67890,
    "ref": "main",
    "status": "pending",
    "stages": ["build", "test", "deploy"]
  },
  "user": {
    "id": 1,
    "name": "Developer"
  },
  "project": {
    "id": 111,
    "name": "my-project"
  },
  "builds": [
    {
      "id": 12345,
      "stage": "test",
      "name": "test",
      "status": "pending"
    }
  ]
}
```

---

## Security Best Practices

### 1. Always Use Webhook Secret

```yaml
# config.yaml
gitlab:
  webhook_secret: "use-strong-random-secret-here"
```

### 2. Use HTTPS in Production

```yaml
# config.yaml
server:
  tls_enabled: true
  tls_cert_path: "/path/to/cert.pem"
  tls_key_path: "/path/to/key.pem"
```

### 3. Restrict IP Access (Optional)

```yaml
# Only allow GitLab IP ranges
# Edit webhook_security.go AllowedIPs
```

### 4. Enable Rate Limiting

Default: 60 requests/minute per IP

---

## Webhook Monitoring

### View Webhook Stats

```bash
# Check recent webhook requests
sudo journalctl -u firerunner --since "10 minutes ago" | grep webhook

# Count webhooks received
sudo journalctl -u firerunner --since today | grep "Received webhook" | wc -l

# Check for errors
sudo journalctl -u firerunner --since today | grep ERROR
```

### Prometheus Metrics

```bash
# Webhook requests total
curl http://localhost:9090/metrics | grep webhook_requests_total

# Webhook errors
curl http://localhost:9090/metrics | grep webhook_errors_total
```

---

## Common Webhook Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| 401 Unauthorized | Invalid secret | Verify secret matches |
| 404 Not Found | Wrong URL | Check URL is /webhook |
| 500 Internal Server Error | FireRunner crash | Check logs: `journalctl -u firerunner` |
| Connection timeout | Firewall | Open port 8080: `ufw allow 8080` |
| No response | FireRunner not running | Start: `systemctl start firerunner` |

---

## Testing Checklist

- [ ] Health endpoint responds: `curl http://localhost:8080/health`
- [ ] Webhook endpoint responds without secret
- [ ] Webhook endpoint validates secret correctly
- [ ] GitLab webhook test shows HTTP 200
- [ ] Logs show "Received webhook event"
- [ ] Real job triggers webhook
- [ ] Job appears in FireRunner logs
- [ ] Metrics are updated: `/metrics`

---

## Support

If webhook issues persist:

1. **Enable Debug Logging:**
   ```yaml
   logging:
     level: "debug"
   ```

2. **Collect Logs:**
   ```bash
   sudo journalctl -u firerunner --since "1 hour ago" > firerunner.log
   ```

3. **Report Issue:**
   - GitHub: https://github.com/ismoilovdevml/firerunner/issues
   - Include: logs, config (remove secrets), error messages

---

**Happy Hooking! 🪝**
