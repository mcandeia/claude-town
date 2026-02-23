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
- A GitHub App **or** a Personal Access Token (PAT)
- An Anthropic API key

## Setup

There are two authentication methods: **GitHub App** (full-featured) or **PAT** (simpler setup).

### Option A: Personal Access Token (simplest)

Create a [fine-grained PAT](https://github.com/settings/tokens?type=beta) with these permissions on your target repos:

- Issues: Read & Write
- Pull Requests: Read & Write
- Contents: Read & Write
- Webhooks: Read & Write (for auto-creating repo webhooks)

```bash
helm install claude-town chart/ \
  --namespace claude-town-system \
  --create-namespace \
  --set github.pat="ghp_xxxxxxxxxxxx" \
  --set github.webhookSecret="YOUR_SECRET" \
  --set github.botName="your-bot-name" \
  --set anthropic.apiKey="YOUR_ANTHROPIC_KEY" \
  --set selfDNS="your-domain.com"
```

When you create a `ClaudeRepository`, the operator automatically creates a webhook on the repo.

### Option B: GitHub App

Create a new GitHub App at https://github.com/settings/apps/new with:

**Permissions:**
- Issues: Read & Write
- Pull Requests: Read & Write
- Contents: Read & Write
- Metadata: Read

**Subscribe to events:**
- Issues
- Issue comment
- Pull request review
- Pull request review comment

Save the App ID, generate a private key, and note the Installation ID after installing the app on your org/repos.

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

### Allow repositories

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

Webhooks are auto-created when a `ClaudeRepository` is added (using the PAT or App credentials). The webhook ID is stored in the resource status and cleaned up on deletion.

#### Per-repo PAT

You can use a different PAT for specific repos by referencing a Kubernetes Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: external-org-pat
  namespace: claude-town-system
type: Opaque
stringData:
  token: "ghp_yyyyyyyyyyyy"
---
apiVersion: claude-town.claude-town.io/v1alpha1
kind: ClaudeRepository
metadata:
  name: external-repo
  namespace: claude-town-system
spec:
  owner: external-org
  repo: their-app
  patSecretRef:
    name: external-org-pat
```

### Auto-trigger by label

You can configure labels on a `ClaudeRepository` so that issues are automatically picked up without needing an @mention:

```yaml
apiVersion: claude-town.claude-town.io/v1alpha1
kind: ClaudeRepository
metadata:
  name: my-repo
spec:
  owner: my-org
  repo: my-app
  labels: ["claude"]
```

When an issue is **created with** or **has added** a matching label (e.g. `claude`), the operator automatically creates a task. The user allowlist is still enforced — the user who created the issue or added the label must be allowed.

**Note:** If you add labels to an existing `ClaudeRepository`, you must delete and recreate it so the webhook is re-created with the `issues` event subscription.

### Use it

Comment on any issue in an allowed repository:

> @your-bot-name please fix this bug

Claude will spawn, work on the issue, and open a PR.

To fix PR review feedback:

> @your-bot-name please address this review

## Clarification

When Claude needs more information to complete a task, it will ask a clarification question directly on the GitHub issue or PR:

> **Claude needs clarification to continue:**
>
> Should I implement this as a REST endpoint or a GraphQL mutation?
>
> Please reply mentioning @your-bot-name with your answer.

Reply with your answer mentioning the bot, and Claude will resume working with the additional context. This loop can repeat up to `maxClarifications` rounds (default: 3, configurable per-task up to 10).

If no response is received within 24 hours, the task times out and is marked as failed.

## Cost Reporting

Each GitHub comment (completion, failure, and clarification) includes a cost summary:

> 📊 **Cost:** $0.1400 | **Tokens:** 15000 in / 3000 out | **Duration:** 45.2s

Costs accumulate across clarification rounds so you always see the running total. The full cost breakdown is also stored in the `ClaudeTask` status:

```yaml
status:
  costReport:
    inputTokens: 15000
    outputTokens: 3000
    cacheReadInputTokens: 8000
    cacheCreationInputTokens: 2000
    estimatedCost: "$0.1400"
    durationMs: 45200
```

## Handler Allowlist

By default, anyone who can comment on an allowed repository can trigger Claude. You can restrict this with username and/or role-based allowlists.

### Global allowlist (Helm values)

```yaml
github:
  allowedUsers: ["alice", "bob"]      # GitHub usernames
  allowedRoles: ["admin", "maintain"] # Repo permission levels
```

### Per-repo allowlist (ClaudeRepository)

```yaml
apiVersion: claude-town.claude-town.io/v1alpha1
kind: ClaudeRepository
metadata:
  name: my-repo
spec:
  owner: my-org
  repo: my-app
  allowedUsers: ["alice"]
  allowedRoles: ["admin"]
```

Global and per-repo lists are merged (union). If both lists are empty at all levels, anyone can trigger Claude (backwards compatible). Valid roles: `admin`, `maintain`, `write`.

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
              kubectl exec into sandbox pod
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
