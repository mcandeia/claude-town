# Claude Town

Kubernetes operator that spawns Claude Code agents to solve GitHub issues and fix PR reviews.

## How it works

1. You mention `@your-bot-name` in a GitHub issue or PR review
2. The operator receives the webhook and creates a `ClaudeTask`
3. A Claude sandbox is allocated from a pre-warmed pool (via [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox))
4. Claude clones the repo, reads the issue context, and implements a solution
5. Claude opens a PR and comments back on the issue
6. The sandbox is released back to the pool

## Prerequisites

- Kubernetes cluster (or KIND for local development)
- [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) installed
- Helm 3
- A GitHub App
- An Anthropic API key

## Setup

### 1. Create a GitHub App

Create a new GitHub App at https://github.com/settings/apps/new with:

**Permissions:**
- Issues: Read & Write
- Pull Requests: Read & Write
- Contents: Read & Write
- Metadata: Read

**Subscribe to events:**
- Issue comment
- Pull request review
- Pull request review comment

Save the App ID, generate a private key, and note the Installation ID after installing the app on your org/repos.

### 2. Install

```bash
helm install claude-town chart/ \
  --namespace claude-town-system \
  --create-namespace \
  --set github.appId="YOUR_APP_ID" \
  --set github.installationId="YOUR_INSTALLATION_ID" \
  --set-file github.privateKey=path/to/private-key.pem \
  --set github.webhookSecret="YOUR_SECRET" \
  --set anthropic.apiKey="YOUR_ANTHROPIC_KEY" \
  --set selfDNS="your-domain.com"
```

### 3. Allow repositories

Create a `ClaudeRepository` resource for each repo you want the bot to work on:

```yaml
apiVersion: claude-town.claude-town.io/v1alpha1
kind: ClaudeRepository
metadata:
  name: my-org-my-repo
  namespace: claude-town-system
spec:
  owner: my-org
  repo: my-repo
  maxConcurrentTasks: 3
```

### 4. Use it

Comment on any issue in an allowed repository:

> @your-bot-name please fix this bug

Claude will spawn, work on the issue, and open a PR.

To fix PR review feedback:

> @your-bot-name please address this review

## Local Development

```bash
# Prerequisites: kind, helm, docker, cloudflared
export GITHUB_APP_ID=...
export GITHUB_INSTALLATION_ID=...
export GITHUB_APP_PRIVATE_KEY_FILE=./private-key.pem
export GITHUB_WEBHOOK_SECRET=...
export ANTHROPIC_API_KEY=...

# Setup KIND cluster with everything installed
make kind-setup

# In a separate terminal, start the tunnel:
cloudflared tunnel --url http://localhost:30082

# Copy the tunnel URL and update:
helm upgrade claude-town chart/ -n claude-town-system --set selfDNS=<tunnel-url>
```

## Architecture

```
GitHub Webhook -> Operator (webhook handler, port 8082)
                    |
              ClaudeTask CR created
                    |
              SandboxClaim -> agent-sandbox warm pool -> Pod ready
                    |
              PTY connection (WebSocket, port 7681)
                    |
              Claude Code CLI runs autonomously
                    |
              PR created -> GitHub comment -> Sandbox released
```

### CRDs

- **ClaudeRepository** - allowlist of repos the bot can work on
- **ClaudeTask** - represents a single task (issue solve or PR review fix)

### Key Makefile targets

```bash
make build           # Build the operator binary
make docker-build    # Build the Docker image
make helm-lint       # Lint the Helm chart
make helm-template   # Render Helm templates
make kind-setup      # Setup KIND cluster
make kind-load       # Load images into KIND
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
