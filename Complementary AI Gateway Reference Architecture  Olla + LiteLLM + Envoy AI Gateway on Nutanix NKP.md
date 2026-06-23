# Complementary AI Gateway Reference Architecture
## Olla (On-Prem Ollama Pool) + LiteLLM (OpenRouter) + Envoy AI Gateway on Nutanix NKP/AHV

***

## Executive Summary

This document describes a three-tier, complementary AI gateway architecture deployable on Nutanix Kubernetes Platform (NKP) running on AHV. The design assigns each gateway layer a distinct, non-overlapping responsibility:

- **Olla** — lightweight Go proxy that load-balances across the on-prem Ollama GPU node pool using priority-based routing, health checks, and circuit breakers
- **LiteLLM Proxy** — Python gateway that manages OpenRouter free-tier model access with per-model RPM budgets and fallback logic
- **Envoy AI Gateway** — Kubernetes-native data plane (Envoy Proxy CRDs) that presents a single unified OpenAI-compatible `/v1` endpoint to all consuming clients, routing to either Olla or LiteLLM as upstream `AIServiceBackend` targets

Together, this stack provides local-first, on-prem inference via Ollama with seamless cloud overflow to OpenRouter free-tier models — all behind a single stable endpoint with token-rate limiting, OIDC-ready auth, and native Prometheus/OpenTelemetry observability[^1][^2][^3].

***

## 1. Reference Architecture

### 1.1 Logical Topology

```
╔══════════════════════════════════════════════════════════════════════════════╗
║                        CLIENT CONSUMER LAYER                                 ║
║  OpenAI SDK  │  LangChain  │  Open WebUI  │  VS Code Continue  │  AI Agents  ║
╚══════════════════════════════╦═════════════════════════════════════════════════╝
                               ║  POST /v1/chat/completions
                               ║  Authorization: Bearer sk-gateway-key
                               ▼
╔══════════════════════════════════════════════════════════════════════════════╗
║               TIER 1 — ENVOY AI GATEWAY (North-South Ingress)                ║
║               Namespace: envoy-ai-gateway-system                              ║
║               Service: envoy-ai-gateway  → 10.10.50.10:80/443                ║
║               CRDs: AIGatewayRoute, AIServiceBackend, BackendSecurityPolicy   ║
║               Token rate limiting │ Request routing │ Auth │ Observability    ║
╚══════════════╦═══════════════════════════════╦════════════════════════════════╝
               ║  model: local/*               ║  model: openrouter/*
               ▼                               ▼
╔══════════════════════╗          ╔═══════════════════════════╗
║  TIER 2a — OLLA      ║          ║  TIER 2b — LITELLM PROXY  ║
║  Namespace: ai-local  ║          ║  Namespace: ai-cloud      ║
║  Service: olla-svc   ║          ║  Service: litellm-svc      ║
║  10.10.50.20:40114   ║          ║  10.10.50.30:4000          ║
║  Load balancer for   ║          ║  OpenRouter provider mgmt  ║
║  Ollama node pool    ║          ║  Rate budget enforcement   ║
╚══════════════╦═══════╝          ╚══════════════╦═════════════╝
               ║                                 ║
    ┌──────────┼──────────┐                      ║
    ▼          ▼          ▼                      ▼
╔═══════╗ ╔═══════╗ ╔═══════╗         ╔══════════════════╗
║GPU-01 ║ ║GPU-02 ║ ║GPU-03 ║         ║  OpenRouter API  ║
║:11434 ║ ║:11434 ║ ║:11434 ║         ║  (cloud / free   ║
║L40S   ║ ║L40S   ║ ║A100   ║         ║   tier models)   ║
╚═══════╝ ╚═══════╝ ╚═══════╝         ╚══════════════════╝
10.10.10.11 .12    .13
```

### 1.2 IP Address Allocation

| Component | Role | IP Address | Port | Namespace |
|---|---|---|---|---|
| NKP Control Plane VIP | K8s API | 10.10.50.5 | 6443 | — |
| Envoy AI Gateway (LoadBalancer) | Client-facing ingress | 10.10.50.10 | 80 / 443 | envoy-ai-gateway-system |
| Olla Service (ClusterIP) | Internal LB for Ollama | 10.10.50.20 | 40114 | ai-local |
| LiteLLM Service (ClusterIP) | OpenRouter proxy | 10.10.50.30 | 4000 | ai-cloud |
| Redis (for LiteLLM state) | Rate limit state | 10.10.50.35 | 6379 | ai-cloud |
| Ollama GPU Node 01 | Inference (L40S) | 10.10.10.11 | 11434 | ai-local |
| Ollama GPU Node 02 | Inference (L40S) | 10.10.10.12 | 11434 | ai-local |
| Ollama GPU Node 03 | Inference (A100) | 10.10.10.13 | 11434 | ai-local |
| Prometheus | Metrics scrape target | 10.10.50.40 | 9090 | monitoring |
| Grafana | Dashboards | 10.10.50.41 | 3000 | monitoring |

> **Note:** All Service IPs above use example RFC 1918 ranges. In NKP/AHV, `LoadBalancer` IPs are assigned by MetalLB or the AHV Load Balancer integration; `ClusterIP` addresses are assigned by Kubernetes from the cluster service CIDR. Substitute your actual IP assignments.

***

## 2. Namespace and Network Segmentation

```bash
kubectl create namespace ai-local
kubectl create namespace ai-cloud
kubectl create namespace envoy-ai-gateway-system
kubectl create namespace monitoring
```

Apply a NetworkPolicy to prevent direct client access to Olla and LiteLLM (all inbound AI traffic must transit Envoy AI Gateway):

```yaml
# networkpolicy-ai-tiers.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-only-from-envoy
  namespace: ai-local
spec:
  podSelector: {}
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: envoy-ai-gateway-system
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-only-from-envoy
  namespace: ai-cloud
spec:
  podSelector: {}
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: envoy-ai-gateway-system
```

```bash
kubectl apply -f networkpolicy-ai-tiers.yaml
```

***

## 3. Tier 2a — Olla Deployment (On-Prem Ollama Load Balancer)

Olla runs as a single Go binary with ~20MB idle memory and ~14.4MB static image[^3], making it appropriate for a lightweight sidecar-style Deployment in the `ai-local` namespace.

### 3.1 Olla ConfigMap

```yaml
# olla-configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: olla-config
  namespace: ai-local
data:
  olla.yaml: |
    discovery:
      type: "static"
      static:
        endpoints:
          # GPU Node 01 — High priority (L40S, 48GB VRAM)
          - name: "ollama-gpu-01"
            url: "http://10.10.10.11:11434"
            type: "ollama"
            priority: 100
            check_interval: 10s
            check_timeout: 3s

          # GPU Node 02 — High priority (L40S, 48GB VRAM)
          - name: "ollama-gpu-02"
            url: "http://10.10.10.12:11434"
            type: "ollama"
            priority: 100
            check_interval: 10s
            check_timeout: 3s

          # GPU Node 03 — Medium priority (A100, 80GB VRAM — heavy models)
          - name: "ollama-gpu-03"
            url: "http://10.10.10.13:11434"
            type: "ollama"
            priority: 75
            check_interval: 10s
            check_timeout: 3s

    proxy:
      load_balancer: "least-connections"

    server:
      port: 40114
      rate_limits:
        per_ip_requests_per_minute: 300
        global_requests_per_minute: 5000

    logging:
      level: "info"
      format: "json"
```

### 3.2 Olla Deployment and Service

```yaml
# olla-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: olla
  namespace: ai-local
  labels:
    app: olla
spec:
  replicas: 2
  selector:
    matchLabels:
      app: olla
  template:
    metadata:
      labels:
        app: olla
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "40114"
        prometheus.io/path: "/metrics"
    spec:
      containers:
      - name: olla
        image: ghcr.io/thushan/olla:latest
        args: ["--config", "/app/config/olla.yaml"]
        ports:
        - containerPort: 40114
          name: http
        volumeMounts:
        - name: config
          mountPath: /app/config
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
          limits:
            memory: "256Mi"
            cpu: "500m"
        livenessProbe:
          httpGet:
            path: /health
            port: 40114
          initialDelaySeconds: 5
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 40114
          initialDelaySeconds: 3
          periodSeconds: 5
      volumes:
      - name: config
        configMap:
          name: olla-config
---
apiVersion: v1
kind: Service
metadata:
  name: olla-svc
  namespace: ai-local
  labels:
    app: olla
spec:
  type: ClusterIP
  selector:
    app: olla
  ports:
  - name: http
    port: 40114
    targetPort: 40114
    protocol: TCP
```

```bash
kubectl apply -f olla-configmap.yaml
kubectl apply -f olla-deployment.yaml

# Verify
kubectl get pods -n ai-local
kubectl get svc -n ai-local
```

### 3.3 Ollama Node Deployments (GPU-Scheduled on NKP)

Each Ollama instance runs as a Kubernetes Deployment with GPU node affinity and NVIDIA taint tolerations. NKP GPU node pools are tainted with `nvidia.com/gpu=present:NoSchedule` by default[^4].

```yaml
# ollama-node-01.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama-node-01
  namespace: ai-local
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ollama
      instance: "01"
  template:
    metadata:
      labels:
        app: ollama
        instance: "01"
    spec:
      tolerations:
      - key: "nvidia.com/gpu"
        operator: "Exists"
        effect: "NoSchedule"
      nodeSelector:
        nvidia.com/gpu.present: "true"
        accelerator: "l40s"          # Label applied to L40S nodes
      containers:
      - name: ollama
        image: ollama/ollama:latest
        ports:
        - containerPort: 11434
        env:
        - name: OLLAMA_HOST
          value: "0.0.0.0"
        - name: OLLAMA_NUM_PARALLEL
          value: "4"
        - name: OLLAMA_MAX_LOADED_MODELS
          value: "2"
        resources:
          limits:
            nvidia.com/gpu: "1"
          requests:
            nvidia.com/gpu: "1"
            memory: "16Gi"
            cpu: "4"
        volumeMounts:
        - name: ollama-models
          mountPath: /root/.ollama
      volumes:
      - name: ollama-models
        persistentVolumeClaim:
          claimName: ollama-models-pvc-01
---
apiVersion: v1
kind: Service
metadata:
  name: ollama-svc-01
  namespace: ai-local
spec:
  type: ClusterIP
  selector:
    app: ollama
    instance: "01"
  ports:
  - port: 11434
    targetPort: 11434
```

> Repeat the above pattern for `ollama-node-02` and `ollama-node-03`, adjusting the `instance` label and PVC name. For node-03 (A100), change `nodeSelector` to `accelerator: a100`.

**Pre-pull models onto all nodes:**

```bash
# Pull models on each Ollama pod (adjust pod names after deployment)
for pod in $(kubectl get pods -n ai-local -l app=ollama -o jsonpath='{.items[*].metadata.name}'); do
  echo "Pulling on $pod..."
  kubectl exec -n ai-local $pod -- ollama pull llama3.1
  kubectl exec -n ai-local $pod -- ollama pull nomic-embed-text
done
```

**Apply a PVC for model storage (Nutanix CSI):**

```yaml
# ollama-pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ollama-models-pvc-01
  namespace: ai-local
spec:
  accessModes:
  - ReadWriteOnce
  storageClassName: nutanix-volume    # Default NKP Nutanix CSI storage class
  resources:
    requests:
      storage: 100Gi
```

```bash
kubectl apply -f ollama-pvc.yaml
kubectl apply -f ollama-node-01.yaml
# repeat for 02, 03
```

***

## 4. Tier 2b — LiteLLM Proxy Deployment (OpenRouter Gateway)

LiteLLM manages the OpenRouter connection with model-level RPM budgets and virtual key authentication[^2][^5].

### 4.1 LiteLLM Secret (API Keys)

```bash
# Create Kubernetes secrets for API keys — never store in ConfigMap or YAML
kubectl create secret generic litellm-secrets \
  --namespace ai-cloud \
  --from-literal=OPENROUTER_API_KEY="sk-or-v1-your-openrouter-key" \
  --from-literal=LITELLM_MASTER_KEY="sk-litellm-internal-key-abc123"
```

### 4.2 LiteLLM ConfigMap

```yaml
# litellm-configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: litellm-config
  namespace: ai-cloud
data:
  config.yaml: |
    model_list:

      # ─── OPENROUTER FREE MODELS ────────────────────────────────────────
      - model_name: or-deepseek-r1-free
        litellm_params:
          model: openrouter/deepseek/deepseek-r1:free
          api_key: os.environ/OPENROUTER_API_KEY
          api_base: https://openrouter.ai/api/v1
          rpm: 20
          tpm: 200000

      - model_name: or-llama3.3-free
        litellm_params:
          model: openrouter/meta-llama/llama-3.3-70b-instruct:free
          api_key: os.environ/OPENROUTER_API_KEY
          api_base: https://openrouter.ai/api/v1
          rpm: 20

      - model_name: or-qwen3-coder-free
        litellm_params:
          model: openrouter/qwen/qwen3-coder-480b-a35b:free
          api_key: os.environ/OPENROUTER_API_KEY
          api_base: https://openrouter.ai/api/v1
          rpm: 20

      - model_name: or-deepseek-v4-flash-free
        litellm_params:
          model: openrouter/deepseek/deepseek-v4-flash:free
          api_key: os.environ/OPENROUTER_API_KEY
          api_base: https://openrouter.ai/api/v1
          rpm: 20

      - model_name: or-free-auto
        litellm_params:
          model: openrouter/openrouter/free
          api_key: os.environ/OPENROUTER_API_KEY
          api_base: https://openrouter.ai/api/v1
          rpm: 20

    router_settings:
      routing_strategy: least-busy
      num_retries: 2
      timeout: 90
      redis_host: redis-svc.ai-cloud.svc.cluster.local
      redis_port: 6379

    general_settings:
      master_key: os.environ/LITELLM_MASTER_KEY

      # Allowlist — only free models may be called through this proxy
      allowed_models:
        - or-deepseek-r1-free
        - or-llama3.3-free
        - or-qwen3-coder-free
        - or-deepseek-v4-flash-free
        - or-free-auto
```

### 4.3 LiteLLM Deployment, Service, and Redis

```yaml
# litellm-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: litellm
  namespace: ai-cloud
  labels:
    app: litellm
spec:
  replicas: 2
  selector:
    matchLabels:
      app: litellm
  template:
    metadata:
      labels:
        app: litellm
    spec:
      containers:
      - name: litellm
        image: ghcr.io/berriai/litellm:main-latest
        command: ["litellm"]
        args:
          - "--config"
          - "/app/config.yaml"
          - "--port"
          - "4000"
          - "--num_workers"
          - "4"
        ports:
        - containerPort: 4000
          name: http
        env:
        - name: OPENROUTER_API_KEY
          valueFrom:
            secretKeyRef:
              name: litellm-secrets
              key: OPENROUTER_API_KEY
        - name: LITELLM_MASTER_KEY
          valueFrom:
            secretKeyRef:
              name: litellm-secrets
              key: LITELLM_MASTER_KEY
        volumeMounts:
        - name: config
          mountPath: /app/config.yaml
          subPath: config.yaml
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "1Gi"
            cpu: "2"
        livenessProbe:
          httpGet:
            path: /health
            port: 4000
          initialDelaySeconds: 15
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 4000
          initialDelaySeconds: 10
          periodSeconds: 5
      volumes:
      - name: config
        configMap:
          name: litellm-config
---
apiVersion: v1
kind: Service
metadata:
  name: litellm-svc
  namespace: ai-cloud
  labels:
    app: litellm
spec:
  type: ClusterIP
  selector:
    app: litellm
  ports:
  - name: http
    port: 4000
    targetPort: 4000
---
# Redis for LiteLLM shared state
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  namespace: ai-cloud
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        ports:
        - containerPort: 6379
        resources:
          requests:
            memory: "128Mi"
            cpu: "100m"
          limits:
            memory: "512Mi"
            cpu: "500m"
---
apiVersion: v1
kind: Service
metadata:
  name: redis-svc
  namespace: ai-cloud
spec:
  type: ClusterIP
  selector:
    app: redis
  ports:
  - port: 6379
    targetPort: 6379
```

```bash
kubectl apply -f litellm-configmap.yaml
kubectl apply -f litellm-deployment.yaml

# Verify
kubectl get pods -n ai-cloud
kubectl get svc -n ai-cloud
```

***

## 5. Tier 1 — Envoy AI Gateway Installation and Configuration

Envoy AI Gateway requires Kubernetes 1.31+ and Envoy Gateway 1.1.0+[^6]. NKP ships with a compatible upstream Kubernetes version.

### 5.1 Install Envoy Gateway (Prerequisite)

```bash
# Install Envoy Gateway via Helm
helm install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version v1.4.0 \
  --namespace envoy-gateway-system \
  --create-namespace

kubectl wait --timeout=5m \
  -n envoy-gateway-system deployment/envoy-gateway \
  --for=condition=Available
```

### 5.2 Install Envoy AI Gateway CRDs and Controller

```bash
# Install CRDs
helm install ai-gateway-crds \
  oci://docker.io/envoyproxy/ai-gateway-crds-helm \
  --version v0.5.0 \
  --namespace envoy-ai-gateway-system \
  --create-namespace

# Install AI Gateway controller
helm install ai-gateway \
  oci://docker.io/envoyproxy/ai-gateway-helm \
  --version v0.5.0 \
  --namespace envoy-ai-gateway-system \
  --set 'endpointConfig.openai=/'

kubectl wait --timeout=5m \
  -n envoy-ai-gateway-system deployment/envoy-ai-gateway \
  --for=condition=Available
```

### 5.3 GatewayClass and Gateway

```yaml
# gateway-class.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy-ai-gateway
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
---
# gateway.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ai-gateway
  namespace: envoy-ai-gateway-system
spec:
  gatewayClassName: envoy-ai-gateway
  listeners:
  - name: http
    protocol: HTTP
    port: 80
  - name: https
    protocol: HTTPS
    port: 443
    tls:
      mode: Terminate
      certificateRefs:
      - name: ai-gateway-tls-cert    # Pre-existing TLS secret
        kind: Secret
```

```bash
kubectl apply -f gateway-class.yaml
kubectl apply -f gateway.yaml

# Get the assigned LoadBalancer IP (MetalLB / AHV LB)
kubectl get svc -n envoy-ai-gateway-system
```

### 5.4 Backend Resources — Olla and LiteLLM

Envoy Gateway `Backend` resources define the upstream connection details[^7]:

```yaml
# backends.yaml

# ── Olla Backend (on-prem Ollama pool) ───────────────────────────────────────
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: olla-backend
  namespace: envoy-ai-gateway-system
spec:
  endpoints:
  - fqdn:
      hostname: olla-svc.ai-local.svc.cluster.local
      port: 40114
---
# ── LiteLLM Backend (OpenRouter cloud proxy) ─────────────────────────────────
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: Backend
metadata:
  name: litellm-backend
  namespace: envoy-ai-gateway-system
spec:
  endpoints:
  - fqdn:
      hostname: litellm-svc.ai-cloud.svc.cluster.local
      port: 4000
```

### 5.5 BackendSecurityPolicy — LiteLLM Internal Key

```yaml
# security-policies.yaml

# API key secret for LiteLLM master key
apiVersion: v1
kind: Secret
metadata:
  name: litellm-internal-key-secret
  namespace: envoy-ai-gateway-system
type: Opaque
stringData:
  apiKey: "sk-litellm-internal-key-abc123"    # Match LITELLM_MASTER_KEY value
---
# BackendSecurityPolicy injects the key into upstream requests to LiteLLM
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: BackendSecurityPolicy
metadata:
  name: litellm-bsp
  namespace: envoy-ai-gateway-system
spec:
  targetRefs:
  - group: aigateway.envoyproxy.io
    kind: AIServiceBackend
    name: litellm-openrouter-backend
  apiKey:
    secretRef:
      name: litellm-internal-key-secret
      namespace: envoy-ai-gateway-system
```

> **Note:** Olla does not enforce auth by default in this config — inbound traffic is controlled at the NetworkPolicy level. If Olla auth is required, add a corresponding `BackendSecurityPolicy` for the Olla backend.

### 5.6 AIServiceBackend Resources

These are the bridge between the `AIGatewayRoute` and the upstream services[^7][^8]:

```yaml
# ai-service-backends.yaml

# ── AIServiceBackend: Olla (exposes an OpenAI-compatible schema) ─────────────
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: AIServiceBackend
metadata:
  name: olla-local-backend
  namespace: envoy-ai-gateway-system
spec:
  schema:
    name: OpenAI
    version: "v1"
  backendRef:
    name: olla-backend
    kind: Backend
    group: gateway.envoyproxy.io
---
# ── AIServiceBackend: LiteLLM (OpenRouter — also OpenAI-compatible) ──────────
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: AIServiceBackend
metadata:
  name: litellm-openrouter-backend
  namespace: envoy-ai-gateway-system
spec:
  schema:
    name: OpenAI
    version: "v1"
  backendRef:
    name: litellm-backend
    kind: Backend
    group: gateway.envoyproxy.io
  backendSecurityPolicyRef:
    name: litellm-bsp
    namespace: envoy-ai-gateway-system
```

### 5.7 AIGatewayRoute — Unified Routing Rules

The `AIGatewayRoute` defines the unified `/v1` API and model-name-based routing rules. Local models route to Olla; OpenRouter models route to LiteLLM[^8][^9]:

```yaml
# ai-gateway-route.yaml
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: AIGatewayRoute
metadata:
  name: unified-ai-route
  namespace: envoy-ai-gateway-system
spec:
  schema:
    name: OpenAI
  parentRefs:
  - name: ai-gateway
    namespace: envoy-ai-gateway-system
    kind: Gateway

  # Token usage tracking for cost and quota management
  llmRequestCosts:
  - metadataKey: llm-input-token-cost
    type: InputToken
    cost: 1
  - metadataKey: llm-output-token-cost
    type: OutputToken
    cost: 1

  rules:
    # ── Rule 1: OpenRouter models → LiteLLM backend ────────────────────────
    # Match any model name prefixed with "or-"
    - matches:
      - headers:
        - type: RegularExpression
          name: x-ai-eg-model
          value: "^or-.*"
      backendRefs:
      - name: litellm-openrouter-backend
        namespace: envoy-ai-gateway-system
        kind: AIServiceBackend

    # ── Rule 2: Specific local model names → Olla backend ──────────────────
    - matches:
      - headers:
        - type: RegularExpression
          name: x-ai-eg-model
          value: "^(llama3.1|llama3.2|phi4|qwen2.5|deepseek-coder|nomic-embed-text|mistral).*"
      backendRefs:
      - name: olla-local-backend
        namespace: envoy-ai-gateway-system
        kind: AIServiceBackend

    # ── Rule 3: Catch-all → Olla (local-first default) ──────────────────────
    - backendRefs:
      - name: olla-local-backend
        namespace: envoy-ai-gateway-system
        kind: AIServiceBackend
```

```bash
kubectl apply -f backends.yaml
kubectl apply -f security-policies.yaml
kubectl apply -f ai-service-backends.yaml
kubectl apply -f ai-gateway-route.yaml

# Verify the generated HTTPRoute
kubectl get httproute -n envoy-ai-gateway-system

# Get gateway external IP
export GATEWAY_URL=$(kubectl get svc -n envoy-ai-gateway-system \
  -l gateway.envoyproxy.io/owning-gateway-name=ai-gateway \
  -o jsonpath='{.items.status.loadBalancer.ingress.ip}')

echo "Gateway: http://$GATEWAY_URL"
```

***

## 6. End-to-End Validation

### 6.1 Health Checks

```bash
# Check all pod health across all tiers
kubectl get pods -n ai-local
kubectl get pods -n ai-cloud
kubectl get pods -n envoy-ai-gateway-system

# Check Olla health and backend pool status
kubectl exec -n ai-local deploy/olla -- wget -qO- http://localhost:40114/health
kubectl exec -n ai-local deploy/olla -- wget -qO- http://localhost:40114/v1/models

# Check LiteLLM health
kubectl exec -n ai-cloud deploy/litellm -- curl -s http://localhost:4000/health
kubectl exec -n ai-cloud deploy/litellm -- \
  curl -s http://localhost:4000/v1/models \
  -H "Authorization: Bearer sk-litellm-internal-key-abc123" | jq '.data[].id'
```

### 6.2 Test Local Inference (via Envoy → Olla → Ollama)

```bash
# Chat completions — local model via Envoy AI Gateway
curl -X POST http://$GATEWAY_URL/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-client-key" \
  -d '{
    "model": "llama3.1",
    "messages": [{"role": "user", "content": "What is Nutanix NKP?"}],
    "stream": false
  }'

# Embeddings — local via Envoy → Olla → Ollama
curl -X POST http://$GATEWAY_URL/v1/embeddings \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-client-key" \
  -d '{
    "model": "nomic-embed-text",
    "input": "Nutanix hybrid cloud platform"
  }'
```

### 6.3 Test Cloud Inference (via Envoy → LiteLLM → OpenRouter)

```bash
# Cloud model — name prefix "or-" routes to LiteLLM
curl -X POST http://$GATEWAY_URL/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-client-key" \
  -d '{
    "model": "or-deepseek-r1-free",
    "messages": [{"role": "user", "content": "Explain transformer attention mechanisms."}],
    "stream": false
  }'
```

### 6.4 Validate Routing Logic

```bash
# List all models visible at the gateway (merges local + cloud)
curl -s http://$GATEWAY_URL/v1/models \
  -H "Authorization: Bearer sk-your-client-key" | jq '.data[].id'

# Check Olla backend pool health directly (internal)
kubectl exec -n ai-local deploy/olla -- \
  wget -qO- http://localhost:40114/status | jq '.endpoints'

# Check LiteLLM routing info
kubectl exec -n ai-cloud deploy/litellm -- \
  curl -s http://localhost:4000/provider/list \
  -H "Authorization: Bearer sk-litellm-internal-key-abc123"
```

***

## 7. Token Rate Limiting via QuotaPolicy

Envoy AI Gateway provides token-level quota management with `QuotaPolicy`[^10]. Apply per-client token budgets on top of the gateway route:

```yaml
# quota-policy.yaml
apiVersion: aigateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: global-token-quota
  namespace: envoy-ai-gateway-system
spec:
  targetRefs:
  - group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: unified-ai-route
  rateLimit:
    type: Global
    global:
      rules:
      - clientSelectors:
        - headers:
          - name: x-user-id
            type: Distinct      # Per-user rate limit key
        limit:
          requests: 100
          unit: Minute
```

```bash
kubectl apply -f quota-policy.yaml
```

***

## 8. Observability Integration (NKP Prometheus Stack)

NKP deploys Prometheus and Grafana via the DKP Catalog[^11][^12]. Configure scrape targets for all three tiers:

```yaml
# prometheus-scrape-config.yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ai-gateway-stack
  namespace: monitoring
  labels:
    release: kube-prometheus-stack
spec:
  namespaceSelector:
    matchNames:
    - ai-local
    - ai-cloud
    - envoy-ai-gateway-system
  selector:
    matchLabels:
      app: olla
  endpoints:
  - port: http
    path: /metrics
    interval: 15s
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: litellm-monitor
  namespace: monitoring
  labels:
    release: kube-prometheus-stack
spec:
  namespaceSelector:
    matchNames:
    - ai-cloud
  selector:
    matchLabels:
      app: litellm
  endpoints:
  - port: http
    path: /metrics
    interval: 15s
```

```bash
kubectl apply -f prometheus-scrape-config.yaml
```

**Key metrics to monitor across the stack:**

| Metric Source | Key Metrics |
|---|---|
| Envoy AI Gateway | `ai_gateway_llm_input_tokens_total`, `ai_gateway_request_duration_seconds`, `ai_gateway_upstream_errors_total` |
| Olla | `olla_requests_total`, `olla_backend_health`, `olla_response_latency_p99` |
| LiteLLM | `litellm_requests_total`, `litellm_spend_per_model`, `litellm_429_errors` |
| Ollama (via DCGM) | `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_DEV_MEM_COPY_UTIL` |

***

## 9. Client SDK Configuration

Any OpenAI-compatible client SDK requires only one configuration change — point the `base_url` at the Envoy AI Gateway address:

### Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-your-client-key",           # Envoy AI Gateway client key
    base_url=f"http://{GATEWAY_URL}/v1"     # Single unified endpoint
)

# Local model via Olla → Ollama
local_response = client.chat.completions.create(
    model="llama3.1",
    messages=[{"role": "user", "content": "Hello from NKP!"}]
)

# Cloud model via LiteLLM → OpenRouter
cloud_response = client.chat.completions.create(
    model="or-deepseek-r1-free",
    messages=[{"role": "user", "content": "Explain quantum entanglement."}]
)
```

### LangChain

```python
from langchain_openai import ChatOpenAI

llm_local = ChatOpenAI(
    model="llama3.1",
    openai_api_key="sk-your-client-key",
    openai_api_base=f"http://{GATEWAY_URL}/v1"
)

llm_cloud = ChatOpenAI(
    model="or-qwen3-coder-free",
    openai_api_key="sk-your-client-key",
    openai_api_base=f"http://{GATEWAY_URL}/v1"
)
```

### Continue (VS Code / JetBrains)

```json
{
  "models": [
    {
      "title": "NKP Local (llama3.1)",
      "provider": "openai",
      "model": "llama3.1",
      "apiKey": "sk-your-client-key",
      "apiBase": "http://10.10.50.10/v1"
    },
    {
      "title": "OpenRouter DeepSeek R1 (Free)",
      "provider": "openai",
      "model": "or-deepseek-r1-free",
      "apiKey": "sk-your-client-key",
      "apiBase": "http://10.10.50.10/v1"
    }
  ]
}
```

***

## 10. Deployment Order Summary

| Step | Command / Action | Validates With |
|---|---|---|
| 1. Create namespaces | `kubectl create namespace ai-local ai-cloud monitoring` | `kubectl get ns` |
| 2. Apply NetworkPolicies | `kubectl apply -f networkpolicy-ai-tiers.yaml` | `kubectl get networkpolicy -A` |
| 3. Create Ollama PVCs | `kubectl apply -f ollama-pvc.yaml` | `kubectl get pvc -n ai-local` |
| 4. Deploy Ollama nodes | `kubectl apply -f ollama-node-0{1,2,3}.yaml` | `kubectl get pods -n ai-local` |
| 5. Pull models | `kubectl exec ... ollama pull llama3.1` | `kubectl exec ... ollama list` |
| 6. Deploy Olla | `kubectl apply -f olla-configmap.yaml -f olla-deployment.yaml` | `kubectl get pods -n ai-local` |
| 7. Create LiteLLM secrets | `kubectl create secret generic litellm-secrets ...` | `kubectl get secret -n ai-cloud` |
| 8. Deploy Redis + LiteLLM | `kubectl apply -f litellm-deployment.yaml` | `kubectl get pods -n ai-cloud` |
| 9. Install Envoy Gateway | `helm install envoy-gateway ...` | `kubectl get pods -n envoy-gateway-system` |
| 10. Install Envoy AI Gateway | `helm install ai-gateway ...` | `kubectl get pods -n envoy-ai-gateway-system` |
| 11. Apply Gateway CRDs | `kubectl apply -f gateway-class.yaml -f gateway.yaml` | `kubectl get gateway -A` |
| 12. Apply Backends + Route | `kubectl apply -f backends.yaml -f ai-service-backends.yaml -f ai-gateway-route.yaml` | `kubectl get httproute -A` |
| 13. Apply QuotaPolicy | `kubectl apply -f quota-policy.yaml` | `kubectl get backendtrafficpolicy -A` |
| 14. Apply ServiceMonitors | `kubectl apply -f prometheus-scrape-config.yaml` | Grafana → Targets |
| 15. End-to-end validation | `curl http://$GATEWAY_URL/v1/chat/completions ...` | HTTP 200 + JSON response |

***

## 11. Architecture Decision Notes

| Decision | Rationale |
|---|---|
| Olla for local pool LB instead of K8s Services | Olla provides health checks, circuit breakers, and priority-based routing across Ollama nodes that standard Kubernetes Services do not offer[^3] |
| LiteLLM for OpenRouter instead of direct Envoy backend | OpenRouter requires API key injection and per-model RPM budgets; LiteLLM's `allowed_models` and `rpm:` params enforce cost controls[^2] — Envoy AI Gateway has no native OpenRouter provider[^13] |
| Envoy AI Gateway as ingress instead of LiteLLM at ingress | Envoy operates at the network data plane; it provides token-level quotas, CRD-based GitOps config, and future MCP routing support without adding another Python process in the critical path[^9] |
| Model name prefix routing (`or-*` vs local names) | Avoids complex regex patterns; Envoy AI Gateway extracts `model` from the request body into the `x-ai-eg-model` header automatically before route matching[^14] |
| GPU nodes tainted with `nvidia.com/gpu=present:NoSchedule` | NKP/NAI standard — prevents CPU workloads from landing on GPU nodes; Ollama pods carry matching tolerations[^4] |

***

*Reference architecture designed for Nutanix NKP (Kubernetes 1.31+) on AHV. All IP addresses are illustrative — replace with actual IP assignments from your NKP cluster and MetalLB/AHV Load Balancer pool. June 2026.*

---

## References

1. [Home | Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/) - Provide a unified layer for routing and managing LLM/AI traffic. · Support automatic failover mechan...

2. [Proxy - Load Balancing - LiteLLM](https://docs.litellm.ai/docs/proxy/load_balancing) - Load balance multiple instances of the same model

3. [Olla - Open Source LLM Proxy - TensorFoundry](https://tensorfoundry.io/products/olla) - Open-source LLM proxy for small businesses and growing teams. Dual proxy engines, circuit breakers, ...

4. [NKP クラスタの GPU ノードで GPU Pod むけ ... - NTNX＞日記](https://blog.ntnx.jp/entry/2026/03/16/235035) - Nutanix Kubernetes Platform（NKP）で GPU ノード プールを作成すると、デフォルトでは GPU を必要としない Pod であっても、GPU ノードで起動されてしまいます...

5. [LiteLLM proxy routes between Ollama, vLLM, and cloud APIs ...](https://openclawsome.com/news/ai/litellm-unified-proxy-multi-model-routing) - LiteLLM is an open-source proxy that exposes a single OpenAI-compatible API endpoint and routes requ...

6. [Installation | envoyproxy/ai-gateway | DeepWiki](https://deepwiki.com/envoyproxy/ai-gateway/6.1-installation) - This page provides step-by-step instructions for deploying Envoy AI Gateway to a Kubernetes cluster....

7. [Connecting to AI Providers - Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/capabilities/llm-integrations/connect-providers/) - Envoy AI Gateway provides a unified interface for connecting to multiple AI providers through a stan...

8. [API Reference | Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/latest/api/) - aigateway.envoyproxy.io/v1alpha1

9. [Managing Enterprise AI Workloads with the Envoy AI Gateway](https://saptak.in/writing/2025/04/23/envoy-ai-gateway) - The home page of Saptak Sen, product manager, author, and software engineer at Hortonworks and Micro...

10. [AIGatewayRoute + InferencePool Guide | Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/0.3/capabilities/inference/aigatewayroute-inferencepool/) - This guide demonstrates how to use InferencePool with AIGatewayRoute for advanced AI-specific infere...

11. [Monitor Ollama Performance with Prometheus & Grafana ...](https://www.arsturn.com/blog/the-ultimate-guide-to-monitoring-ollama-with-prometheus-and-grafana) - Learn to monitor your local Ollama LLM server's performance and resource usage. This guide covers se...

12. [Observability Stack: Monitoring Your AI Gateway - Sebastian Maniak](https://maniak.io/articles/02-observability-stack-monitoring-your-ai-gateway/) - This setup will give you real-time visibility into your AI Gateway's performance, cost metrics, toke...

13. [Supported AI Providers - Envoy AI Gateway](https://aigateway.envoyproxy.io/docs/0.5/capabilities/llm-integrations/supported-providers/) - Since the Envoy AI Gateway is designed to provide a Unified API for routing and managing LLM/AI traf...

14. [OpenAI backend with Envoy AI Gateway | by using System - Medium](https://medium.com/h7w/openai-backend-with-envoy-ai-gateway-3cc4c438effb) - In my previous post — “Install an AI Gateway with Envoy Gateway AI” — we bootstrapped Envoy AI Gatew...

