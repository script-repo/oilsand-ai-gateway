# Oilsand AI Gateway

A terminal UI (TUI) for deploying and operating an OpenAI-compatible LLM pool on
Nutanix AHV: an [Olla](https://github.com/thushan/olla) gateway/load-balancer in
front of one or more [Ollama](https://ollama.com) worker VMs.

The TUI provisions the VMs through the Prism Central v4 API, installs Olla and
Ollama natively as systemd services, manages the pool and models, and gives you a
streaming chat and load-balancing view over the gateway.

```text
OpenAI-compatible clients
        |
        v
   Olla gateway VM  (load balancer, :40114)
        |  least-connections / priority routing
        +--> Ollama worker VM  (:11434)
        +--> Ollama worker VM  (:11434)
        +--> ...
```

## Install (prebuilt binaries)

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

The installers also set up everything the **interactive features** need (best-effort):

> * **OpenSSH client** (for the Console/Agents sessions): checked, and installed via the
>   platform package manager / Windows capability when missing.
> * **Python 3 + a dedicated virtualenv** at `<install-dir>/venv` with `requests` and
>   `paramiko` from `requirements.txt` (for Nutanix deploy/delete). The TUI auto-discovers
>   this venv, so no `OILSAND_PYTHON` export is needed.
>
> System-package installs run non-interactively and never block a piped install; if a
> dependency can't be installed automatically the script prints the exact command to run.
> Set `OILSAND_SKIP_DEPS=1` to skip dependency setup entirely. The core features (gateway,
> pool, models, chat, load) need none of these — just the static binary. You can still
> override the interpreter/script path with `OILSAND_PYTHON` and `OILSAND_VM_SCRIPT`.

## Build from source

```bash
# build (Go 1.25+); produces tui/oilsand-tui(.exe)
cd tui
go build -o oilsand-tui .

# run (first launch prompts for the gateway + SSH credentials and remembers them)
./oilsand-tui

# …or pre-seed them via flags / environment variables
./oilsand-tui --gateway http://gateway-host:40114 \
    --ssh-user rocky --ssh-password 'your-ssh-password'
```

Tagging a release (`git tag v0.1.0 && git push origin v0.1.0`) triggers the GoReleaser
workflow (`.github/workflows/release.yml`), which cross-compiles all six OS/arch targets and
uploads the archives + checksums to GitHub Releases.

## Using the TUI

The TUI is written in Go with the [Charm](https://github.com/charmbracelet) stack —
**Bubble Tea** (runtime), **Huh** (modal forms), **Bubbles** (`list`, `help`, `key`,
`progress`, `textarea`, `viewport`, `spinner`), **Lip Gloss** (styling/layout) and
**Glamour** (markdown rendering for streamed chat replies).

The layout is a **left sidebar (menu) + content pane** master/detail. The sidebar is the home
base; pick a section, then press `Enter` to focus its content and `Esc` to come back to the menu.

Navigation:

* `↑/↓` (or `j/k`) move through the menu; `1`–`7` jump straight to a section.
* `Enter` focus the content pane · `Esc` back to the menu.
* `c` open the **Connect** form (gateway URL + SSH creds) · `d` disconnect · `r` refresh.
* `/` filter any list · `?` toggle the full keybinding legend · `q`/`Ctrl+C` quit.

A live, context-aware **help bar** (`bubbles/help`) renders the relevant keys for wherever you
are, and forms (connect / add endpoint / pull / deploy) appear as centered **Huh** modals with
validation and `Esc`-to-cancel. Inside a form, `Tab`/`Enter` move to the next field and
`Shift+Tab` (or `Ctrl+P`, for terminals/consoles that don't send Shift+Tab) move back.

Sections:

* **Dashboard** — live metric cards (clients, req/s, throughput, latency, success, uptime) plus a
  request-rate sparkline.
* **Pool** — a filterable `bubbles/list` of Ollama endpoints. `a` opens the add-endpoint modal,
  `x` removes the selected one; both edit the gateway's `/etc/olla/olla.yaml` over SSH
  (Go `x/crypto/ssh`) and restart Olla.
* **Models** — filterable list of pool models. `p` opens the pull modal (animated `bubbles/progress`
  gradient bar), `x` deletes the selected model, and `Enter` makes it the active chat model.
  This is how you swap the model Ollama serves.
* **Chat** — a multi-line `bubbles/textarea` composer; prompts go to
  `/olla/openai/v1/chat/completions` and the reply **streams token-by-token live**, then renders as
  Glamour markdown when complete, reporting TTFT and tokens/sec.
* **Load** — load-balancing visualization: per-worker cumulative request share,
  active-connection bars, a gateway throughput sparkline, and a `◀ busiest` marker on the worker
  currently taking the most load.
* **Nutanix** — Prism Central server + managed gateway/worker VMs (filterable list). `g` deploy a
  gateway, `w` deploy a worker (auto-named, registered with the connected gateway). The worker
  deploy modal has an **Instances** field: set it above `1` to provision that many workers in
  parallel (names auto-increment, e.g. `ollama-worker-04..06`); they all register with the gateway
  in a single batched `olla.yaml` write so concurrent deploys don't race. `o` install
  Olla on **this** server (no Nutanix VM — see below), `n` show the next free name, `r` refresh,
  `x` delete the selected VM. Deploys open a Huh modal, then run `nutanix_olla_vm.py` as a
  subprocess and stream output into the log pane. The PC API key is read at runtime from
  `~/.cursor/mcp.json` and never stored in code. The client auto-negotiates the Prism Central v4
  API version (it tries `v4.2`, then `v4.1`, then `v4.0`) so it works across PC releases.
* **Access** — create/rotate a client API token (`t`/`X`); shows the OpenAI Base URL, token, model
  and a `curl` example.

### First launch

The first time you run the TUI with no saved configuration it opens the **Connect** form
automatically and asks for your Olla gateway URL and SSH credentials — nothing is hardcoded.
Those values are saved to `~/.oilsand-ai-gateway/tui.json` (mode `0600`) and reused on later
runs. Gateway URL, SSH user and password can also be supplied up front via the `OLLA_GATEWAY`,
`OLLA_SSH_USER` and `OLLA_SSH_PASSWORD` environment variables (flags/env take precedence over the
saved values). The VM password for Nutanix deploys is entered in the **Nutanix** settings form
(`e`) — deploys are blocked until it is set.

### Install Olla locally (no Nutanix VM)

If you are running the TUI on the Linux box that should host the gateway (for example over
SSH), press `o` in the **Nutanix** section to install Olla on that machine instead of
provisioning a VM. This runs `scripts/remote/install-olla.sh` (via `sudo`) to install the Olla
binary, write `/etc/olla/olla.yaml`, and start the `olla` systemd service on `:40114`; the TUI
then connects to `http://<host-primary-ip>:40114` automatically (the external IP of the host's
default-route NIC, so the gateway is reachable from other machines). Linux only, and it needs passwordless
`sudo` (or run the TUI as root) since the installer can't service an interactive password prompt.

When the gateway is this local host, agents that run on the gateway (for example Crush) are
installed and launched directly — no SSH password is required for them.

## VM (AHV) deployment — Patterns A and B

The TUI's Nutanix section drives `scripts/nutanix_olla_vm.py`, which can also be run directly.
It provisions a Rocky Linux VM through the Prism Central v4 API with a cloud-init script, then
SSHes in to install software natively as systemd services.

* Pattern A: provision the VM and install Olla (the gateway/load balancer).
* Pattern B: provision the VM, install Ollama and pull a model (default `rnj-1`), then
  register the new worker with an existing Olla instance (defaults to the Pattern A VM).

Install dependencies and set Prism Central credentials:

```bash
pip install -r requirements.txt
export PRISM_CENTRAL_URL="https://prism-central-host:9440"
export PRISM_USER="admin"
export PRISM_PASSWORD="********"
# guest (cloud-init) password for the new VM — no default is shipped
export OILSAND_VM_PASSWORD="your-vm-password"
# placement — no lab defaults are baked in (the TUI offers these as live
# dropdowns from Prism Central; the CLI needs them passed or set as env vars)
export OILSAND_IMAGE_NAME="Rocky-9-GenericCloud-Base.latest.x86_64.qcow2"
export OILSAND_CLUSTER_NAME="your-cluster"
export OILSAND_SUBNET_NAME="your-subnet"
```

Pattern A (8 vCPU / 12 GiB / 50 GiB). The guest password and placement (image / cluster /
subnet) must be supplied — via the env vars above, or explicit flags:

```bash
scripts/nutanix_olla_vm.py pattern-a --vm-name olla-gateway-01 \
    --image-name "$OILSAND_IMAGE_NAME" --cluster-name "$OILSAND_CLUSTER_NAME" \
    --subnet-name "$OILSAND_SUBNET_NAME"
```

Pattern B (registers with the Pattern A Olla recorded in `~/.oilsand-ai-gateway/state.json`):

```bash
scripts/nutanix_olla_vm.py pattern-b --vm-name ollama-worker-01 --model rnj-1
# or register with a specific Olla instance:
scripts/nutanix_olla_vm.py pattern-b --model rnj-1 --olla-url http://gateway-host:40114
```

To deploy many workers in parallel without each one racing on the gateway config, provision them
with `--no-register` (which prints `OILSAND_ENDPOINT <json>` instead of touching `olla.yaml`), then
register the whole batch in one pass — this is exactly what the TUI's **Instances** field does:

```bash
scripts/nutanix_olla_vm.py pattern-b --no-register --vm-name ollama-worker-04 --model rnj-1 --olla-url http://gateway-host:40114
scripts/nutanix_olla_vm.py pattern-b --no-register --vm-name ollama-worker-05 --model rnj-1 --olla-url http://gateway-host:40114
# ...then one batched registration (single olla.yaml write + restart):
scripts/nutanix_olla_vm.py register-endpoints --olla-url http://gateway-host:40114 \
  --endpoint name=ollama-worker-04,url=http://10.0.0.4:11434 \
  --endpoint name=ollama-worker-05,url=http://10.0.0.5:11434
```

There are **no baked-in placement defaults**. In the TUI, the Nutanix settings form populates
**Image**, **Cluster** and **Subnet** as dropdowns polled live from Prism Central (it falls back
to free-text if PC isn't reachable yet). On the CLI, pass `--image-name`, `--cluster-name` and
`--subnet-name` (or the `OILSAND_*` env vars). VM sizing can still be tuned with `--num-sockets`,
`--cores-per-socket`, `--memory-gib`, and `--disk-gib`. Use `--dry-run` to print the VM create
body and cloud-init without creating anything. The remote installers live in `scripts/remote/`.

### Worker auto-increment naming

When you deploy a worker without an explicit name, `nutanix_olla_vm.py` scans Prism Central for
existing `ollama-worker-NN` VMs and uses the next index (zero-padded), restarting at `01` when
none exist. Gateways follow the same scheme with `olla-gateway-NN`. Query it directly:

```bash
scripts/nutanix_olla_vm.py next-name --role worker  --prism-url https://prism-central-host:9440
scripts/nutanix_olla_vm.py next-name --role gateway --prism-url https://prism-central-host:9440
```

### Prism Central helper subcommands

`nutanix_olla_vm.py` also supports API-key auth (`--prism-api-key` / `PRISM_API_KEY`) in
addition to user/password, plus VM lifecycle helpers used by the TUI:

```bash
# Show info for the managed VMs (JSON)
scripts/nutanix_olla_vm.py show --name-prefix oll --prism-url https://prism-central-host:9440

# Delete a VM by name
scripts/nutanix_olla_vm.py delete --name ollama-worker-02 --prism-url https://prism-central-host:9440
```
