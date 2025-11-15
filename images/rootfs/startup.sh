#!/bin/bash
set -e

# FireRunner VM Startup Script
# This script runs when the VM boots

echo "==================================="
echo "  FireRunner GitLab Runner VM"
echo "==================================="

# Start Docker daemon
echo "Starting Docker daemon..."
dockerd > /var/log/docker.log 2>&1 &
DOCKER_PID=$!

# Wait for Docker to be ready
echo "Waiting for Docker to be ready..."
max_attempts=30
attempt=0
while ! docker info > /dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ $attempt -ge $max_attempts ]; then
        echo "ERROR: Docker failed to start after $max_attempts attempts"
        cat /var/log/docker.log
        exit 1
    fi
    sleep 1
done

echo "Docker is ready"

# Pre-pull common images (optional, for faster job execution)
# Uncomment if you want to cache images
# echo "Pre-pulling common Docker images..."
# docker pull alpine:latest &
# docker pull ubuntu:22.04 &
# docker pull node:20 &
# docker pull python:3.11 &
# wait

# Register and start GitLab runner
echo "Launching runner registration..."
/usr/local/bin/register-runner.sh

# Keep the container running (shouldn't reach here normally)
tail -f /dev/null
