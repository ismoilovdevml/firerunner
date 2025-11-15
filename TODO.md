# FireRunner - TODO List

## CRITICAL (Must have for v0.2.0)

### 1. Flintlock Integration ⚠️ **BLOCKING**
- [ ] Add Flintlock protobuf dependencies
  ```bash
  go get github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1
  ```
- [ ] Replace mock `CreateMicroVM` in `pkg/firecracker/client.go`
- [ ] Implement `DeleteMicroVM` with real gRPC
- [ ] Implement `GetMicroVM` for status polling
- [ ] Implement `ListMicroVMs` for cleanup
- [ ] Add connection retry logic
- [ ] Handle Flintlock errors properly

**Files to modify:**
- `pkg/firecracker/client.go` (lines 75-110)
- `pkg/firecracker/manager.go` (uses client)

### 2. GitLab Runner Registration ⚠️ **BLOCKING**
- [ ] Implement real runner registration in `pkg/gitlab/service.go`
- [ ] SSH into VM or use cloud-init to start runner
- [ ] Monitor runner heartbeat
- [ ] Implement runner cleanup/unregister
- [ ] Handle registration errors

**Files to modify:**
- `pkg/gitlab/service.go` (line 35-65)
- `pkg/scheduler/scheduler.go` (worker.registerRunner)

### 3. VM Images ⚠️ **BLOCKING**
- [ ] Build kernel image (or use Flintlock's)
- [ ] Build rootfs image from Dockerfile
- [ ] Test images with actual Firecracker
- [ ] Push to container registry
- [ ] Update config.yaml with real URLs
- [ ] Document build process

**Files:**
- `images/rootfs/Dockerfile`
- `images/BUILD.md` (already created ✅)

### 4. Basic Tests
- [ ] Unit tests for config package
- [ ] Unit tests for webhook handler
- [ ] Unit tests for scheduler
- [ ] Mock Flintlock for testing
- [ ] Integration test with real Flintlock
- [ ] E2E test with GitLab

**Target: 50%+ coverage**

---

## HIGH PRIORITY (Should have for v0.2.0)

### 5. Error Handling
- [ ] Better error messages throughout
- [ ] Wrap errors with context
- [ ] Add error codes/types
- [ ] Implement circuit breaker for Flintlock
- [ ] Add timeout handling for long operations

### 6. Observability
- [ ] Implement Prometheus metrics
  - `firerunner_jobs_total{status="success|failed"}`
  - `firerunner_vms_active`
  - `firerunner_vm_creation_duration_seconds`
  - `firerunner_job_duration_seconds`
- [ ] Add trace IDs to logs
- [ ] Create Grafana dashboard

### 7. Documentation
- [ ] Complete API documentation
- [ ] Add troubleshooting guide
- [ ] Write architecture document
- [ ] Add FAQ section
- [ ] Video tutorial (optional)

---

## MEDIUM PRIORITY (Nice to have for v0.3.0)

### 8. Performance
- [ ] VM pre-warming pool
- [ ] Image caching
- [ ] Concurrent VM creation
- [ ] Optimize scheduler

### 9. Reliability
- [ ] Leader election for HA
- [ ] Job queue persistence (Redis)
- [ ] Graceful failover
- [ ] Dead letter queue

### 10. Security
- [ ] TLS for webhook endpoint
- [ ] mTLS for Flintlock
- [ ] Secrets management (Vault)
- [ ] Image scanning (Trivy)
- [ ] Audit logging

---

## LOW PRIORITY (Future)

### 11. Advanced Features
- [ ] GPU support
- [ ] Multiple executors (shell, docker, kubernetes)
- [ ] Custom VM sizes
- [ ] Spot instances support

### 12. Multi-platform
- [ ] GitHub Actions support
- [ ] Jenkins integration
- [ ] Kubernetes operator

### 13. UI/CLI
- [ ] Web dashboard
- [ ] CLI tool for management
- [ ] Terraform provider

---

## Known Issues

1. **Mock Implementations**
   - Flintlock calls return fake data
   - Runner registration is placeholder
   - VM IP is hardcoded

2. **No Cleanup**
   - Stale VMs might not be cleaned up properly
   - Need to implement better lifecycle tracking

3. **No Validation**
   - Job requirements not validated
   - Resource limits not enforced

---

## Quick Wins (Easy improvements)

- [ ] Add `--version` flag to binary
- [ ] Add health check endpoint `/health`
- [ ] Add readiness check `/ready`
- [ ] Improve log messages
- [ ] Add debug mode (`LOG_LEVEL=debug`)
- [ ] Color output for CLI
- [ ] Add examples directory with sample jobs

---

## Community Tasks (Help Wanted)

- [ ] Logo design
- [ ] Website/landing page
- [ ] Blog post / announcement
- [ ] Conference talk
- [ ] YouTube tutorial
- [ ] Translations (docs)

---

Last updated: November 2024
