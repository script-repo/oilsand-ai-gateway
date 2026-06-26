# Deployment Guide: Envoy AI Gateway + Olla + LiteLLM on Nutanix NKP/AHV

This guide turns the reference architecture into an actionable rollout plan for an NKP/AHV Kubernetes cluster. It assumes you are deploying the manifests under `k8s/base/` and customizing them for your cluster before applying.

## 1. Architecture implemented by this repo

The manifests deploy a local-first, OpenAI-compatible gateway stack:

1. **Envoy AI Gateway** in `envoy-ai-gateway-system` exposes the north-south `/v1` endpoint.
2. **Olla** in `ai-local` load-balances local Ollama GPU nodes.
3. **Ollama** pods in `ai-local` run on GPU nodes and serve local models on port `11434`.
4. **LiteLLM + Redis** in `ai-cloud` broker OpenRouter access and enforce per-model budgets.
5. **Prometheus ServiceMonitor** resources in `monitoring` expose gateway-tier metrics.

Model-name routing is configured so `or-*` model IDs route to LiteLLM/OpenRouter, specific local model IDs route to Olla/Ollama, and all other model IDs default to Olla for local-first behavior.

## 2. Prerequisites

Install or verify the following before applying the base manifests:

- A Nutanix NKP/AHV Kubernetes cluster with GPU worker nodes.
- `kubectl` configured for the target cluster.
- Helm 3 for installing Envoy Gateway and Envoy AI Gateway controllers.
- NVIDIA device plugin or GPU Operator installed and advertising `nvidia.com/gpu` resources.
- GPU node labels that match the Ollama manifests:
  - `nvidia.com/gpu.present=true`
  - `accelerator=l40s` for nodes 01 and 02
  - `accelerator=a100` for node 03
- A StorageClass named `nutanix-volume`, or an updated PVC manifest using your cluster's StorageClass.
- A load balancer integration such as MetalLB or the AHV Load Balancer integration.
- Optional but recommended: Prometheus Operator CRDs for `ServiceMonitor` resources.

## 3. Install Envoy controller prerequisites

The base manifests define Envoy Gateway and Envoy AI Gateway custom resources, but they do not install the controllers or CRDs. Install those first.

```bash
helm install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version v1.4.0 \
  --namespace envoy-gateway-system \
  --create-namespace

kubectl wait --timeout=5m \
  -n envoy-gateway-system deployment/envoy-gateway \
  --for=condition=Available

helm install ai-gateway-crds \
  oci://docker.io/envoyproxy/ai-gateway-crds-helm \
  --version v0.5.0 \
  --namespace envoy-ai-gateway-system \
  --create-namespace

helm install ai-gateway \
  oci://docker.io/envoyproxy/ai-gateway-helm \
  --version v0.5.0 \
  --namespace envoy-ai-gateway-system \
  --set 'endpointConfig.openai=/'

kubectl wait --timeout=5m \
  -n envoy-ai-gateway-system deployment/envoy-ai-gateway \
  --for=condition=Available
```

## 4. Customize manifests before deployment

Review and edit the following files for your environment.

### 4.1 GPU scheduling and storage

Update the Ollama node manifests if your GPU labels, taints, StorageClass, or GPU layout differ:

- `k8s/base/ollama/ollama-node-01.yaml`
- `k8s/base/ollama/ollama-node-02.yaml`
- `k8s/base/ollama/ollama-node-03.yaml`
- `k8s/base/ollama/ollama-pvcs.yaml`

Common changes:

```yaml
nodeSelector:
  nvidia.com/gpu.present: "true"
  accelerator: l40s
```

```yaml
storageClassName: nutanix-volume
resources:
  requests:
    storage: 100Gi
```

### 4.2 Olla backend pool

Update `k8s/base/olla/olla-configmap.yaml` if you change Ollama service names, ports, priorities, or rate limits.

The default backend pool is:

- `ollama-svc-01.ai-local.svc.cluster.local:11434`, priority `100`
- `ollama-svc-02.ai-local.svc.cluster.local:11434`, priority `100`
- `ollama-svc-03.ai-local.svc.cluster.local:11434`, priority `75`

### 4.3 LiteLLM/OpenRouter models and budgets

Update `k8s/base/litellm/litellm-configmap.yaml` to change:

- OpenRouter model IDs.
- Free-tier allowlist entries.
- `rpm` and `tpm` limits.
- LiteLLM router retries and timeout.

Keep the `or-` prefix convention unless you also update Envoy routing rules.

### 4.4 Envoy routing

Update `k8s/base/envoy-ai-gateway/ai-gateway-route.yaml` to change model-name routing.

Default behavior:

- `^or-.*` routes to `litellm-openrouter-backend`.
- Local model prefixes route to `olla-local-backend`.
- Catch-all routes to `olla-local-backend`.

### 4.5 TLS

The Gateway manifest expects a TLS Secret named `ai-gateway-tls-cert` in `envoy-ai-gateway-system`.

Create it before enabling HTTPS traffic:

```bash
kubectl create secret tls ai-gateway-tls-cert \
  --namespace envoy-ai-gateway-system \
  --cert=/path/to/tls.crt \
  --key=/path/to/tls.key
```

If you are starting with HTTP only, remove or comment the HTTPS listener in `k8s/base/envoy-ai-gateway/gateway.yaml`.

## 5. Create runtime secrets

The repo includes example secrets with placeholder values for local development. Replace them before production use.

### 5.1 LiteLLM runtime secret

```bash
kubectl create namespace ai-cloud --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic litellm-secrets \
  --namespace ai-cloud \
  --from-literal=OPENROUTER_API_KEY="${OPENROUTER_API_KEY}" \
  --from-literal=LITELLM_MASTER_KEY="${LITELLM_MASTER_KEY}" \
  --dry-run=client -o yaml > /tmp/litellm-secrets.yaml
```

Review `/tmp/litellm-secrets.yaml`, then apply it:

```bash
kubectl apply -f /tmp/litellm-secrets.yaml
```

### 5.2 Envoy-to-LiteLLM internal key

The Envoy `BackendSecurityPolicy` injects the LiteLLM master key for upstream calls. It must match `LITELLM_MASTER_KEY`.

```bash
kubectl create namespace envoy-ai-gateway-system --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic litellm-internal-key-secret \
  --namespace envoy-ai-gateway-system \
  --from-literal=apiKey="${LITELLM_MASTER_KEY}" \
  --dry-run=client -o yaml > /tmp/litellm-internal-key.yaml

kubectl apply -f /tmp/litellm-internal-key.yaml
```

> Do not commit real secret values. The checked-in `*.example.yaml` files are placeholders only.

## 6. Deploy the stack

### 6.1 Render first

```bash
kubectl kustomize k8s/base > /tmp/oilsand-ai-gateway.yaml
```

Inspect the rendered output before applying:

```bash
less /tmp/oilsand-ai-gateway.yaml
```

### 6.2 Apply base manifests

```bash
kubectl apply -k k8s/base
```

### 6.3 Wait for workloads

```bash
kubectl wait --timeout=10m -n ai-local deployment/ollama-node-01 --for=condition=Available
kubectl wait --timeout=10m -n ai-local deployment/ollama-node-02 --for=condition=Available
kubectl wait --timeout=10m -n ai-local deployment/ollama-node-03 --for=condition=Available
kubectl wait --timeout=5m -n ai-local deployment/olla --for=condition=Available
kubectl wait --timeout=5m -n ai-cloud deployment/redis --for=condition=Available
kubectl wait --timeout=5m -n ai-cloud deployment/litellm --for=condition=Available
```

## 7. Pull models onto Ollama nodes

Pull required local models after Ollama pods are running.

```bash
for pod in $(kubectl get pods -n ai-local -l app=ollama -o jsonpath='{.items[*].metadata.name}'); do
  echo "Pulling models on ${pod}"
  kubectl exec -n ai-local "${pod}" -- ollama pull llama3.1
  kubectl exec -n ai-local "${pod}" -- ollama pull nomic-embed-text
done
```

An optional helper job is provided at `k8s/base/validation/model-prepull-job.yaml`, but it is not part of the base kustomization because clusters vary in RBAC permissions for pod listing and `exec`.

## 8. Get the gateway URL

```bash
kubectl get svc -n envoy-ai-gateway-system
```

If your Envoy service is labeled with the owning gateway name, you can capture the external IP like this:

```bash
export GATEWAY_HOST=$(kubectl get svc -n envoy-ai-gateway-system \
  -l gateway.envoyproxy.io/owning-gateway-name=ai-gateway \
  -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}')

export GATEWAY_URL="http://${GATEWAY_HOST}"
echo "${GATEWAY_URL}"
```

Use `https://` instead if you configured TLS and DNS.

## 9. Validate the deployment

### 9.1 Kubernetes health

```bash
kubectl get pods -n ai-local
kubectl get pods -n ai-cloud
kubectl get pods -n envoy-ai-gateway-system
kubectl get httproute -n envoy-ai-gateway-system
```

### 9.2 Olla and local model visibility

```bash
kubectl exec -n ai-local deploy/olla -- wget -qO- http://localhost:40114/health
kubectl exec -n ai-local deploy/olla -- wget -qO- http://localhost:40114/v1/models
```

### 9.3 LiteLLM health

```bash
kubectl exec -n ai-cloud deploy/litellm -- curl -fsS http://localhost:4000/health
kubectl exec -n ai-cloud deploy/litellm -- \
  curl -fsS http://localhost:4000/v1/models \
  -H "Authorization: Bearer ${LITELLM_MASTER_KEY}"
```

### 9.4 End-to-end smoke test

Use the included smoke-test helper after the gateway has an external address.

```bash
scripts/smoke-test.sh "${GATEWAY_URL}" "${CLIENT_API_KEY}"
```

Or test manually.

Local inference path:

```bash
curl -fsS -X POST "${GATEWAY_URL}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${CLIENT_API_KEY}" \
  -d '{
    "model": "llama3.1",
    "messages": [{"role": "user", "content": "What is Nutanix NKP?"}],
    "stream": false
  }'
```

Cloud/OpenRouter path:

```bash
curl -fsS -X POST "${GATEWAY_URL}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${CLIENT_API_KEY}" \
  -d '{
    "model": "or-deepseek-r1-free",
    "messages": [{"role": "user", "content": "Explain transformer attention."}],
    "stream": false
  }'
```

## 10. Operations

### 10.1 Scaling

Scale Olla and LiteLLM independently:

```bash
kubectl scale deployment/olla -n ai-local --replicas=3
kubectl scale deployment/litellm -n ai-cloud --replicas=3
```

Scale Ollama by adding new node manifests and then adding the new service endpoint to `k8s/base/olla/olla-configmap.yaml`.

### 10.2 Observability

If Prometheus Operator is installed, the `ServiceMonitor` resources in `k8s/base/observability/service-monitors.yaml` scrape:

- Olla `/metrics` on port `40114`.
- LiteLLM `/metrics` on port `4000`.

Recommended dashboard signals:

- Request rate and latency by model.
- Backend health and failover events.
- LiteLLM `429` errors and per-model usage.
- GPU utilization and memory pressure through DCGM exporter.

### 10.3 Updating model routes

When adding a new local model:

1. Pull it onto every intended Ollama pod.
2. Add or update the local regex in `k8s/base/envoy-ai-gateway/ai-gateway-route.yaml`.
3. Apply the route update:

```bash
kubectl apply -f k8s/base/envoy-ai-gateway/ai-gateway-route.yaml
```

When adding a new OpenRouter model:

1. Add it to `model_list` and `allowed_models` in `k8s/base/litellm/litellm-configmap.yaml`.
2. Keep the `or-` model prefix.
3. Restart LiteLLM pods after applying the ConfigMap:

```bash
kubectl apply -f k8s/base/litellm/litellm-configmap.yaml
kubectl rollout restart deployment/litellm -n ai-cloud
```

## 11. Troubleshooting

| Symptom | Checks | Likely fix |
|---|---|---|
| Ollama pods are pending | `kubectl describe pod -n ai-local -l app=ollama` | Confirm GPU labels, taints, and `nvidia.com/gpu` availability. |
| PVCs are pending | `kubectl get pvc -n ai-local` | Update `storageClassName` in `ollama-pvcs.yaml`. |
| Olla has no models | `kubectl exec -n ai-local deploy/olla -- wget -qO- http://localhost:40114/v1/models` | Pull models onto the Ollama pods and confirm Olla endpoints. |
| `or-*` models fail | `kubectl logs -n ai-cloud deploy/litellm` | Confirm OpenRouter key, LiteLLM master key, allowed models, and RPM limits. |
| Gateway returns no route | `kubectl get httproute -n envoy-ai-gateway-system -o yaml` | Confirm Envoy AI Gateway CRDs/controllers are installed and `AIGatewayRoute` generated an `HTTPRoute`. |
| Prometheus targets missing | `kubectl get servicemonitor -n monitoring` | Confirm Prometheus Operator CRDs and matching `release` label. |


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

## 11a. VM (AHV) deployment - Patterns A and B

For a non-Kubernetes path that installs Olla/Ollama natively on a Rocky Linux VM,
`scripts/nutanix_olla_vm.py` drives the full flow against Prism Central:

1. Finds the Rocky Linux image, target cluster, and subnet on Prism Central (v4 API).
2. Creates a VM (default 8 vCPU / 12 GiB RAM / 50 GiB disk) with a cloud-init script that
   sets the `rocky` user password to `Nutanix/4u` and grows the root filesystem.
3. SSHes into the guest (password auth) and runs a native installer as a systemd service:
   - Pattern A: `scripts/remote/install-olla.sh` (Olla on `:40114`).
   - Pattern B: `scripts/remote/install-ollama.sh` (Ollama on `:11434`) + `ollama pull <model>`,
     then registers the worker with an existing Olla by rewriting its endpoint list and
     restarting the service.
4. Prints a JSON report covering service readiness (and, for Pattern B, model presence and
   Olla registration).

Prerequisites:

- `pip install -r requirements.txt` (`requests`, `paramiko`).
- `PRISM_CENTRAL_URL`, `PRISM_USER`, `PRISM_PASSWORD` exported in the environment.
- Direct L3 reachability from where you run the script to the VM's subnet (for SSH).

```bash
# Pattern A
scripts/nutanix_olla_vm.py pattern-a --vm-name olla-gateway-01

# Pattern B (defaults to registering with the Pattern A Olla)
scripts/nutanix_olla_vm.py pattern-b --vm-name ollama-worker-01 --model rnj-1
```

Use `--dry-run` to inspect the generated VM create body and cloud-init before provisioning.

## 12. Production hardening checklist

- Replace all example Secret manifests with externally managed secrets.
- Enable HTTPS and provide a valid `ai-gateway-tls-cert` Secret.
- Integrate client authentication with your enterprise identity provider or gateway auth layer.
- Review NetworkPolicies for DNS, metrics, and any required egress paths.
- Pin container image tags instead of using `latest` or floating tags.
- Add PodDisruptionBudgets for Olla and LiteLLM.
- Add resource limits based on measured model size and concurrency.
- Confirm OpenRouter free-tier model names and rate limits before production use.
- Back up persistent model volumes or document a model rehydration process.
