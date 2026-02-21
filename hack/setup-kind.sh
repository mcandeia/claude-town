#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLAUDE_DIR="$(cd "${ROOT_DIR}/../../claude" 2>/dev/null || echo "${ROOT_DIR}/../claude")"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-claude-town}"

echo "=== Creating KIND cluster: ${CLUSTER_NAME} ==="
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Cluster already exists"
    kubectl config use-context "kind-${CLUSTER_NAME}"
else
    kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/kind-config.yaml"
fi

echo "=== Installing agent-sandbox CRDs ==="
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v0.1.1}"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/manifest.yaml" || echo "Warning: agent-sandbox manifest may need manual install"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/extensions.yaml" || echo "Warning: agent-sandbox extensions may need manual install"

echo "=== Building and loading Claude base image ==="
if [ -d "${CLAUDE_DIR}" ]; then
    docker build -t claude:dev "${CLAUDE_DIR}"
    kind load docker-image claude:dev --name "${CLUSTER_NAME}"
else
    echo "Warning: Claude directory not found at ${CLAUDE_DIR}, skipping image build"
fi

echo "=== Building and loading operator image ==="
docker build -t claude-town:dev "${ROOT_DIR}"
kind load docker-image claude-town:dev --name "${CLUSTER_NAME}"

echo "=== Installing claude-town chart ==="
helm upgrade --install claude-town "${ROOT_DIR}/chart" \
    --namespace claude-town-system \
    --create-namespace \
    --set image.repository=claude-town \
    --set image.tag=dev \
    --set image.pullPolicy=Never \
    --set claudeImage.repository=claude \
    --set claudeImage.tag=dev \
    --set-file github.privateKey="${GITHUB_APP_PRIVATE_KEY_FILE:-/dev/null}" \
    --set github.appId="${GITHUB_APP_ID:-}" \
    --set github.installationId="${GITHUB_INSTALLATION_ID:-}" \
    --set github.webhookSecret="${GITHUB_WEBHOOK_SECRET:-}" \
    --set anthropic.apiKey="${ANTHROPIC_API_KEY:-}" \
    --set selfDNS="${SELF_DNS:-localhost}"

echo ""
echo "=== Setup complete! ==="
echo ""
echo "To expose webhooks for local testing with cloudflared:"
echo "  cloudflared tunnel --url http://localhost:30082"
echo ""
echo "Then update the selfDNS and upgrade the chart:"
echo "  helm upgrade claude-town chart/ -n claude-town-system --set selfDNS=<tunnel-url>"
