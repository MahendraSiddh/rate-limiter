#!/usr/bin/env bash
# setup.sh — One-shot environment setup for development
# Installs system-level dependencies on Ubuntu/Debian

set -euo pipefail

echo "=== Adaptive Rate Limiter — Environment Setup ==="

# System packages
sudo apt-get update && sudo apt-get install -y \
    docker.io docker-compose-plugin \
    golang-go \
    python3.11 python3.11-venv python3-pip \
    clang llvm libbpf-dev linux-headers-$(uname -r) \
    redis-tools \
    kafkacat \
    postgresql-client \
    curl jq

# Enable Docker
sudo systemctl enable docker
sudo systemctl start docker

# Install k6 for load testing
curl -fsSL https://dl.k6.io/key.gpg | sudo gpg --dearmor -o /usr/share/keyrings/k6-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/k6-archive-keyring.gpg] https://dl.k6.io/deb stable main" | \
    sudo tee /etc/apt/sources.list.d/k6.list
sudo apt-get update && sudo apt-get install -y k6

echo "=== Setup complete ==="
