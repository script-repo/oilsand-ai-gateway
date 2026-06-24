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
