#!/bin/bash
set -e

# GitLab Runner Auto-Registration Script
# This script runs inside the Firecracker VM and registers the runner with GitLab

echo "FireRunner: Starting runner registration..."

# Wait for network to be available
echo "Waiting for network..."
max_attempts=30
attempt=0
while ! ping -c 1 -W 1 8.8.8.8 > /dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ $attempt -ge $max_attempts ]; then
        echo "ERROR: Network not available after $max_attempts attempts"
        exit 1
    fi
    sleep 1
done

echo "Network is available"

# Get configuration from metadata service (injected by FireRunner)
METADATA_URL="http://169.254.169.254"

# Fetch GitLab configuration
GITLAB_URL=$(curl -s -f "${METADATA_URL}/latest/meta-data/gitlab-url" || echo "")
RUNNER_TOKEN=$(curl -s -f "${METADATA_URL}/latest/meta-data/runner-token" || echo "")
PROJECT_ID=$(curl -s -f "${METADATA_URL}/latest/meta-data/project-id" || echo "")
JOB_ID=$(curl -s -f "${METADATA_URL}/latest/meta-data/job-id" || echo "")
RUNNER_NAME=$(curl -s -f "${METADATA_URL}/latest/meta-data/runner-name" || echo "firerunner-${JOB_ID}")
RUNNER_TAGS=$(curl -s -f "${METADATA_URL}/latest/meta-data/runner-tags" || echo "firecracker,microvm")
EXECUTOR=$(curl -s -f "${METADATA_URL}/latest/meta-data/executor" || echo "docker")

# Validate required parameters
if [ -z "$GITLAB_URL" ] || [ -z "$RUNNER_TOKEN" ]; then
    echo "ERROR: Missing required metadata (gitlab-url or runner-token)"
    echo "GITLAB_URL: $GITLAB_URL"
    echo "RUNNER_TOKEN: [${#RUNNER_TOKEN} chars]"
    exit 1
fi

echo "Registering runner with GitLab..."
echo "  GitLab URL: $GITLAB_URL"
echo "  Runner Name: $RUNNER_NAME"
echo "  Runner Tags: $RUNNER_TAGS"
echo "  Executor: $EXECUTOR"
echo "  Job ID: $JOB_ID"

# Prepare executor-specific config
if [ "$EXECUTOR" = "shell" ]; then
    EXECUTOR_CONFIG=""
else
    # Docker executor (default)
    EXECUTOR="docker"
    EXECUTOR_CONFIG="--docker-image alpine:latest"
fi

# Register runner with GitLab
gitlab-runner register \
  --non-interactive \
  --url "$GITLAB_URL" \
  --registration-token "$RUNNER_TOKEN" \
  --name "$RUNNER_NAME" \
  --tag-list "$RUNNER_TAGS" \
  --executor "$EXECUTOR" \
  --locked=true \
  --run-untagged=false \
  --maximum-timeout=3600 \
  $EXECUTOR_CONFIG

if [ $? -eq 0 ]; then
    echo "Runner registered successfully!"
else
    echo "ERROR: Failed to register runner"
    exit 1
fi

# Start GitLab runner
echo "Starting GitLab runner service..."
gitlab-runner run --user runner --working-directory /home/runner &

RUNNER_PID=$!
echo "Runner started with PID: $RUNNER_PID"

# Monitor runner process
wait $RUNNER_PID
EXIT_CODE=$?

echo "Runner process exited with code: $EXIT_CODE"

# Signal completion to FireRunner (via metadata service or log)
curl -X POST "${METADATA_URL}/latest/meta-data/job-complete" \
  -d "{\"job_id\":\"${JOB_ID}\",\"exit_code\":${EXIT_CODE}}" \
  2>/dev/null || true

exit $EXIT_CODE
