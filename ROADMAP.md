# FireRunner Production Roadmap

## Current Status: **PROTOTYPE/MVP** (v0.1.0)

**Completion: 60%**

### ✅ What Works
- Project structure and architecture
- Configuration management
- HTTP server and webhook handling
- Job scheduling and queue management
- VM lifecycle logic (mock)
- Build system and dependencies

### ❌ What's Missing
- Real Flintlock integration (using mocks)
- GitLab runner registration (placeholder)
- VM images (not built)
- Tests (0% coverage)
- Production deployment examples
- Observability implementation

---

## Phase 1: CORE FUNCTIONALITY (v0.2.0) - **2-3 weeks**

**Goal: Make it actually work end-to-end**

### Week 1: Real Integrations

- [ ] **Flintlock gRPC Implementation**
  - Add `github.com/liquidmetal-dev/flintlock/api` dependency
  - Replace mock `CreateMicroVM` with real gRPC calls
  - Implement `DeleteMicroVM`, `GetMicroVM`, `ListMicroVMs`
  - Add proper error handling and retries
  - Test with actual Flintlock server

- [ ] **VM Images Build**
  - Build kernel image (or use Flintlock's pre-built)
  - Build and test rootfs image
  - Push images to container registry
  - Update config with real image URLs
  - Document image build process

- [ ] **GitLab Runner Registration**
  - Implement real GitLab API calls
  - SSH into VM to start runner
  - Or use cloud-init/metadata service
  - Monitor runner heartbeat
  - Implement runner cleanup

### Week 2: Testing & Validation

- [ ] **Unit Tests**
  - Test configuration loading
  - Test webhook parsing
  - Test scheduler logic
  - Test VM lifecycle management
  - Target: 70%+ coverage

- [ ] **Integration Tests**
  - Test Flintlock communication
  - Test GitLab API calls
  - Test end-to-end job flow
  - Mock external dependencies

- [ ] **E2E Tests**
  - Setup test GitLab project
  - Run real CI/CD job
  - Verify VM creation and cleanup
  - Measure performance metrics

### Week 3: Polish & Documentation

- [ ] **Error Handling**
  - Better error messages
  - Retry logic for transient failures
  - Circuit breaker for Flintlock
  - Graceful degradation

- [ ] **Documentation**
  - Complete API documentation
  - Troubleshooting guide
  - Architecture deep-dive
  - FAQ section

**Deliverable**: Fully working prototype that can run real GitLab jobs

---

## Phase 2: PRODUCTION READY (v0.3.0) - **3-4 weeks**

**Goal: Enterprise-grade reliability and observability**

### Observability

- [ ] **Metrics Implementation**
  - Prometheus metrics:
    - `firerunner_jobs_total`
    - `firerunner_vms_active`
    - `firerunner_vm_creation_duration_seconds`
    - `firerunner_job_duration_seconds`
    - `firerunner_errors_total`
  - Custom dashboards for Grafana
  - Alert rules

- [ ] **Distributed Tracing**
  - OpenTelemetry integration
  - Trace job lifecycle
  - Trace VM creation
  - Jaeger/Tempo export

- [ ] **Structured Logging**
  - JSON logs with correlation IDs
  - Log sampling for high-volume events
  - Integration with Loki/ELK

### Reliability

- [ ] **High Availability**
  - Leader election (for multiple FireRunner instances)
  - Shared state (Redis/etcd)
  - Job queue persistence
  - Graceful failover

- [ ] **Resource Management**
  - CPU/memory limits per job
  - Max concurrent VMs per host
  - Queue priority levels
  - Fair scheduling

- [ ] **Resilience**
  - Circuit breaker for Flintlock
  - Exponential backoff for retries
  - Dead letter queue for failed jobs
  - VM cleanup on crashes

### Security

- [ ] **TLS/mTLS**
  - HTTPS for webhook endpoint
  - mTLS for Flintlock connection
  - Certificate rotation

- [ ] **Authentication & Authorization**
  - Webhook signature verification
  - API token validation
  - RBAC for multi-tenant

- [ ] **Security Hardening**
  - VM isolation verification
  - Secrets management (Vault integration)
  - Image scanning (Trivy)
  - Audit logging

**Deliverable**: Production-ready system with full observability

---

## Phase 3: ADVANCED FEATURES (v0.4.0) - **4-6 weeks**

### Performance Optimization

- [ ] **VM Pre-warming**
  - Maintain pool of ready VMs
  - Instant job execution
  - Configurable pool size
  - Smart pool management

- [ ] **Image Caching**
  - Cache VM images on hosts
  - Faster VM creation
  - Bandwidth savings

- [ ] **Concurrent Operations**
  - Parallel VM creation
  - Batch job scheduling
  - Optimize worker pools

### Advanced Scheduling

- [ ] **Resource-based Scheduling**
  - Schedule based on available resources
  - GPU support
  - NUMA awareness
  - Node affinity

- [ ] **Job Priority**
  - Priority queues
  - SLA-based scheduling
  - Preemption support

- [ ] **Auto-scaling**
  - Dynamic worker count
  - Scale based on queue depth
  - Integration with cluster autoscaler

### Multi-tenancy

- [ ] **Quotas & Limits**
  - Per-project quotas
  - Rate limiting
  - Resource allocation

- [ ] **Billing & Metering**
  - Track resource usage
  - Cost attribution
  - Usage reports

**Deliverable**: Advanced features for large-scale deployments

---

## Phase 4: ECOSYSTEM (v1.0.0) - **Ongoing**

### Integrations

- [ ] **CI/CD Platforms**
  - GitHub Actions support
  - Jenkins integration
  - CircleCI support

- [ ] **Cloud Providers**
  - AWS integration
  - GCP support
  - Azure support

- [ ] **Kubernetes**
  - Operator for K8s deployment
  - CRDs for VM management
  - Pod-like API

### Tooling

- [ ] **CLI Tool**
  - `firerunner-cli` for management
  - Job inspection
  - VM debugging

- [ ] **Web UI**
  - Dashboard for monitoring
  - Job management
  - Configuration UI

- [ ] **Terraform Provider**
  - Infrastructure as Code
  - GitOps integration

### Community

- [ ] **Documentation Site**
  - Comprehensive docs
  - Tutorials and guides
  - API reference

- [ ] **Examples & Templates**
  - Common use cases
  - Best practices
  - Reference architectures

- [ ] **Community Support**
  - Discord/Slack channel
  - Regular releases
  - Community contributions

**Deliverable**: Mature, production-proven platform

---

## Timeline Summary

| Phase | Version | Duration | Status |
|-------|---------|----------|--------|
| Prototype | v0.1.0 | Done | ✅ Complete |
| Core Functionality | v0.2.0 | 2-3 weeks | 🚧 Next |
| Production Ready | v0.3.0 | 3-4 weeks | 📋 Planned |
| Advanced Features | v0.4.0 | 4-6 weeks | 📋 Planned |
| Ecosystem | v1.0.0 | Ongoing | 📋 Future |

**Total to v1.0**: ~3-4 months

---

## How to Contribute

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

**Priority Tasks** (help needed):
1. Implement real Flintlock integration
2. Build and test VM images
3. Write integration tests
4. Create Grafana dashboards
5. Documentation improvements

---

## Version Compatibility

| FireRunner | Flintlock | Firecracker | GitLab |
|------------|-----------|-------------|--------|
| v0.1.0 | v0.6.0+ | v1.4.0+ | 15.0+ |
| v0.2.0 | v0.6.0+ | v1.7.0+ | 15.0+ |
| v0.3.0 | v0.7.0+ | v1.7.0+ | 16.0+ |

---

## Breaking Changes Policy

- **Major versions** (1.0, 2.0): Breaking API changes allowed
- **Minor versions** (0.x, 1.x): New features, backwards compatible
- **Patch versions** (x.x.1): Bug fixes only

---

Last updated: November 2024
