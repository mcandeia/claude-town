#!/usr/bin/env bash
# Quick rebuild and redeploy to KIND cluster.
# Builds both images, loads them, and restarts the deployment.
#
# Usage:
#   ./hack/deploy.sh            # rebuild both images
#   ./hack/deploy.sh operator   # rebuild operator only
#   ./hack/deploy.sh claude     # rebuild claude image only
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
CLAUDE_DIR="$(cd "${ROOT_DIR}/../claude" 2>/dev/null && pwd || echo "")"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-claude-town}"
NAMESPACE="claude-town-system"
TARGET="${1:-all}"

build_operator() {
    echo "=== Building operator image ==="
    docker build -t claude-town:dev "${ROOT_DIR}"
    echo "=== Loading operator image into KIND ==="
    kind load docker-image claude-town:dev --name "${CLUSTER_NAME}"
}

build_claude() {
    if [ ! -d "${CLAUDE_DIR}" ]; then
        echo "Warning: Claude directory not found at ${CLAUDE_DIR}, skipping"
        return
    fi
    echo "=== Building claude sandbox image ==="
    docker build -t claude:dev "${CLAUDE_DIR}"
    echo "=== Loading claude image into KIND ==="
    kind load docker-image claude:dev --name "${CLUSTER_NAME}"
}

case "${TARGET}" in
    operator)
        build_operator
        ;;
    claude)
        build_claude
        ;;
    all|"")
        build_operator
        build_claude
        ;;
    *)
        echo "Usage: $0 [operator|claude|all]"
        exit 1
        ;;
esac

echo "=== Upgrading Helm release ==="
helm upgrade claude-town "${ROOT_DIR}/chart" \
    --namespace "${NAMESPACE}" \
    --reuse-values

echo "=== Restarting operator deployment ==="
kubectl rollout restart deployment -n "${NAMESPACE}" -l app.kubernetes.io/name=claude-town
kubectl rollout status deployment -n "${NAMESPACE}" -l app.kubernetes.io/name=claude-town --timeout=60s

echo ""
echo "=== Deploy complete ==="
kubectl get pods -n "${NAMESPACE}"
