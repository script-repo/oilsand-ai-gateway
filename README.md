# Oilsand AI Gateway Reference Architecture

This repository contains Kubernetes manifests that implement the reference architecture documented in `Complementary AI Gateway Reference Architecture  Olla + LiteLLM + Envoy AI Gateway on Nutanix NKP.md`.

The stack exposes a single OpenAI-compatible `/v1` endpoint through Envoy AI Gateway, routing local model requests to Olla and OpenRouter-prefixed requests to LiteLLM.

For step-by-step cluster rollout instructions, see [DEPLOYMENT.md](DEPLOYMENT.md).

## Architecture

```text
OpenAI-compatible clients
        |
        v
Envoy AI Gateway (envoy-ai-gateway-system)
        | model name routing
        +--> Olla (ai-local) --> Ollama GPU nodes
        +--> LiteLLM (ai-cloud) --> OpenRouter
```

## Quick start

1. Review and customize the manifests under `k8s/base/` for your NKP/AHV environment.
2. Create secrets from local values:
   ```bash
   cp k8s/base/litellm/litellm-secrets.example.env .env.litellm
   cp k8s/base/envoy-ai-gateway/litellm-internal-key.example.env .env.envoy-litellm
   # edit both files before applying
   ```
3. Render the manifests:
   ```bash
   kubectl kustomize k8s/base
   ```
4. Apply prerequisites first, then workload manifests:
   ```bash
   kubectl apply -k k8s/base
   ```
5. Run smoke tests after the gateway receives an address:
   ```bash
   scripts/smoke-test.sh http://<gateway-host-or-ip> sk-your-client-key
   ```

## Important customization points

* `k8s/base/ollama/ollama-node-*.yaml`: node selectors, GPU class labels, storage class, model PVC size.
* `k8s/base/olla/olla-configmap.yaml`: Ollama backend addresses and Olla rate limits.
* `k8s/base/litellm/litellm-configmap.yaml`: OpenRouter model allowlist and RPM/TPM budgets.
* `k8s/base/envoy-ai-gateway/ai-gateway-route.yaml`: model-name routing rules.
* `k8s/base/envoy-ai-gateway/gateway.yaml`: TLS certificate reference and listener configuration.


## One-command Ollama pool deployment

For the simpler pool workflow requested by operators, use the workflow TUI instead of hand-editing every manifest:

```bash
scripts/ai-gateway-tui.py
```

The TUI can deploy Olla first, then deploy or register Ollama workers across Kubernetes and Docker/VM-style hosts. For a greenfield Kubernetes Ollama worker, the lower-level deployer still performs the full loop for one model and one new instance:

1. Deploys or updates the `ai-local` namespace.
2. Deploys the selected Ollama instance and Service.
3. Registers that Service with Olla by updating the Olla endpoint ConfigMap and restarting Olla.
4. Pulls the chosen model into the new Ollama instance.
5. Deploys an API-key protected OpenAI-compatible web gateway in front of Olla.
6. Prints an initial API key; additional keys can be generated from the gateway web UI.


The TUI presents these workflow choices:

* Deploy an Olla gateway on Kubernetes or a Docker/VM host.
* Deploy a greenfield Ollama worker, pull the predefined model, and register it with Olla.
* Register a brownfield Ollama URL that already exists on a physical host, VM, or Kubernetes service.

Non-interactive Kubernetes worker example using the lower-level deployer:

```bash
scripts/deploy-ollama-pool.py --yes \
  --namespace ai-local \
  --model llama3.1 \
  --instance ollama-node-04 \
  --gpu-selector nvidia.com/gpu.present=true,accelerator=l40s \
  --storage-class nutanix-volume \
  --storage-size 100Gi
```

Use `--dry-run` to print the generated manifests without applying them.

## VM (AHV) deployment - Patterns A and B

For deploying directly onto a Rocky Linux VM on Nutanix AHV (instead of Kubernetes/Docker),
`scripts/nutanix_olla_vm.py` provisions the VM through the Prism Central v4 API with a
cloud-init script, then SSHes in to install software natively as systemd services.

* Pattern A: provision the VM and install Olla (the gateway/load balancer).
* Pattern B: provision the VM, install Ollama and pull a model (default `rnj-1`), then
  register the new worker with an existing Olla instance (defaults to the Pattern A VM).

Install dependencies and set Prism Central credentials:

```bash
pip install -r requirements.txt
export PRISM_CENTRAL_URL="https://10.42.156.7:9440"
export PRISM_USER="admin"
export PRISM_PASSWORD="********"
```

Pattern A (default: Rocky 9 generic cloud image, 8 vCPU / 12 GiB / 50 GiB, password `Nutanix/4u`):

```bash
scripts/nutanix_olla_vm.py pattern-a --vm-name olla-gateway-01
```

Pattern B (registers with the Pattern A Olla recorded in `~/.oilsand-ai-gateway/state.json`):

```bash
scripts/nutanix_olla_vm.py pattern-b --vm-name ollama-worker-01 --model rnj-1
# or register with a specific Olla instance:
scripts/nutanix_olla_vm.py pattern-b --model rnj-1 --olla-url http://10.42.156.50:40114
```

Defaults target the discovered environment (`canucks` cluster, `canucks.primary.vlan0`
subnet, `Rocky-9-GenericCloud-Base.latest.x86_64.qcow2` image) and can be overridden with
`--cluster-name`, `--subnet-name`, `--image-name`, `--num-sockets`, `--cores-per-socket`,
`--memory-gib`, and `--disk-gib`. Use `--dry-run` to print the VM create body and cloud-init
without creating anything. The remote installers live in `scripts/remote/`.

## Olla gateway TUI (Go / Charm)

`tui/` is the primary terminal UI, written in Go with the [Charm](https://github.com/charmbracelet)
stack — **Bubble Tea** (runtime), **Huh** (modal forms), **Bubbles** (`list`, `help`, `key`,
`progress`, `textarea`, `viewport`, `spinner`), **Lip Gloss** (styling/layout) and **Glamour**
(markdown rendering for streamed chat replies).

### Install (prebuilt binaries)

Prebuilt binaries for Windows, Linux and macOS (amd64 + arm64) are published on the
[GitHub Releases](https://github.com/script-repo/oilsand-ai-gateway/releases) page. Each
archive contains `oilsand-tui` plus the bundled `scripts/` helpers, so Nutanix deploy works
out of the box. One-line installers:

```bash
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/script-repo/oilsand-ai-gateway/main/scripts/install.ps1 | iex
```

> **Optional prerequisite:** the Nutanix deploy/delete features shell out to
> `scripts/nutanix_olla_vm.py`, which needs **Python 3** and `pip install -r requirements.txt`
> (`requests`, `paramiko`). Everything else (gateway, pool, models, chat, load) works without
> Python. Point the TUI at a custom interpreter or script path with the `OILSAND_PYTHON` and
> `OILSAND_VM_SCRIPT` environment variables.

### Build from source

```bash
# build (Go 1.23+); produces tui/oilsand-tui(.exe)
cd tui
go build -o oilsand-tui .

# run
./oilsand-tui --gateway http://10.42.156.22:40114 \
    --ssh-user rocky --ssh-password 'Nutanix/4u'
```

Tagging a release (`git tag v0.1.0 && git push origin v0.1.0`) triggers the GoReleaser
workflow (`.github/workflows/release.yml`), which cross-compiles all six OS/arch targets and
uploads the archives + checksums to GitHub Releases.

The layout is a **left sidebar (menu) + content pane** master/detail. The sidebar is the home
base; pick a section, then press `Enter` to focus its content and `Esc` to come back to the menu.

Navigation:

* `↑/↓` (or `j/k`) move through the menu; `1`–`7` jump straight to a section.
* `Enter` focus the content pane · `Esc` back to the menu.
* `c` open the **Connect** form (gateway URL + SSH creds) · `d` disconnect · `r` refresh.
* `/` filter any list · `?` toggle the full keybinding legend · `q`/`Ctrl+C` quit.

A live, context-aware **help bar** (`bubbles/help`) renders the relevant keys for wherever you
are, and forms (connect / add endpoint / pull / deploy) appear as centered **Huh** modals with
validation and `Esc`-to-cancel.

Sections:

* **Dashboard** — live metric cards (clients, req/s, throughput, latency, success, uptime) plus a
  request-rate sparkline.
* **Pool** — a filterable `bubbles/list` of Ollama endpoints. `a` opens the add-endpoint modal,
  `x` removes the selected one; both edit the gateway's `/etc/olla/olla.yaml` over SSH
  (Go `x/crypto/ssh`) and restart Olla.
* **Models** — filterable list of pool models. `p` opens the pull modal (animated `bubbles/progress`
  gradient bar), `x` deletes the selected model, and `Enter` makes it the active chat model.
  **This is how you swap the model Ollama serves** (item 4).
* **Chat** — a multi-line `bubbles/textarea` composer; prompts go to
  `/olla/openai/v1/chat/completions` and the reply **streams token-by-token live**, then renders as
  Glamour markdown when complete, reporting TTFT and tokens/sec (item 3).
* **Load** — **load-balancing visualization** (item 5): per-worker cumulative request share,
  active-connection bars, a gateway throughput sparkline, and a `◀ busiest` marker on the worker
  currently taking the most load.
* **Nutanix** — Prism Central server + managed gateway/worker VMs (filterable list). `g` deploy a
  gateway, `w` deploy a worker (auto-named, registered with the connected gateway), `n` show the
  next free name, `r` refresh, `x` delete the selected VM. Deploys open a Huh modal, then run
  `nutanix_olla_vm.py` as a subprocess and stream output into the log pane. The PC API key is read
  at runtime from `~/.cursor/mcp.json` and never stored in code.
* **Access** — create/rotate a client API token (`t`/`X`); shows the OpenAI Base URL, token, model
  and a `curl` example.

### Worker auto-increment naming (items 1 & 2)

Worker VMs are now named with an auto-incrementing index. When you deploy a worker without an
explicit name, `nutanix_olla_vm.py` scans Prism Central for existing `ollama-worker-NN` VMs and
uses the next index (zero-padded), restarting at `01` when none exist. Gateways follow the same
scheme with `olla-gateway-NN`. Query it directly:

```bash
scripts/nutanix_olla_vm.py next-name --role worker  --prism-url https://10.42.156.7:9440
scripts/nutanix_olla_vm.py next-name --role gateway --prism-url https://10.42.156.7:9440
```

## Olla gateway TUI (Python / Textual, legacy)

`scripts/olla_tui.py` is the previous terminal UI for operating an Olla gateway: connect/disconnect,
inspect the pool, manage endpoints, chat-test the pool, and watch real-time metrics.

```bash
pip install -r scripts/requirements-tui.txt
python scripts/olla_tui.py --gateway http://10.42.156.22:40114 \
    --ssh-user rocky --ssh-password 'Nutanix/4u'
```

Features:

* **Connect / Disconnect** to any Olla gateway URL (top bar, or `--gateway`).
* **Overview / Endpoints** tabs: live table of connected Ollama endpoints with status,
  model count, success rate, latency, connections and priority.
* **Models** tab: all models served through the pool (family, parameter size, quant, size).
* **Add / Remove endpoint**: edits the gateway's `/etc/olla/olla.yaml` over SSH and restarts
  Olla (needs the gateway VM SSH credentials; defaults to user `rocky`). Olla's static
  discovery has no write API, so this is done via SSH.
* **Chat** tab: send prompts through `/olla/openai/v1/chat/completions`; streams the reply and
  reports TTFT and tokens/sec for each turn.
* **Metrics** tab: real-time connected clients, server latency, requests/sec, throughput and
  success rate (polled from `/internal/status`), plus tokens/sec and TTFT. Toggle **Auto-probe**
  to continuously measure live tokens/sec and TTFT with small synthetic requests.
* **Nutanix** tab: shows the connected Prism Central server and a table of the Olla gateway /
  Ollama worker VMs (power state, IP, vCPU, memory, disk). Buttons deploy a new gateway, deploy
  a new worker (registered with the connected gateway), or delete the selected VM - each runs
  `nutanix_olla_vm.py` as a subprocess and streams progress into a log pane. Prism Central
  connection details are read at runtime from `~/.cursor/mcp.json` (the `nutanix-v4-mcp` server);
  the API key is never written into the code or passed on the command line.
* **Access** tab: create/rotate a client API token and display the OpenAI-compatible Base URL,
  token and model, with a ready-to-paste `curl` example. Note: Olla Community does not enforce
  inbound API keys (its `auth:` is backend-only), so the token is for your client configs.

Gateway URL, SSH user and password can also be supplied via the `OLLA_GATEWAY`,
`OLLA_SSH_USER` and `OLLA_SSH_PASSWORD` environment variables.

### Prism Central helper subcommands

`nutanix_olla_vm.py` also supports API-key auth (`--prism-api-key` / `PRISM_API_KEY`) in
addition to user/password, plus VM lifecycle helpers used by the TUI:

```bash
# Show info for the managed VMs (JSON)
scripts/nutanix_olla_vm.py show --name-prefix oll --prism-url https://10.42.156.7:9440

# Delete a VM by name
scripts/nutanix_olla_vm.py delete --name ollama-worker-02 --prism-url https://10.42.156.7:9440
```
