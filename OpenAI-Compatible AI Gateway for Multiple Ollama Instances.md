# OpenAI-Compatible AI Gateway for Multiple Ollama Instances
### A Technical Reference for Enterprise Hybrid AI Infrastructure

***

## Executive Summary

Ollama natively exposes an OpenAI-compatible REST API at `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, and `/v1/models` on port `11434`[^1]. While a single Ollama instance is simple to consume, production and enterprise environments demand load balancing, high availability, authentication, and observability across a **pool** of Ollama nodes. An AI gateway layer solves this by presenting a single, stable OpenAI-compatible endpoint that front-ends multiple Ollama backends — handling request distribution, failover, health checking, rate limiting, and security transparently to any consuming application or agent.

This report surveys the leading open-source and commercial solutions for this pattern, provides configuration examples, and outlines architectural considerations relevant to enterprise on-premises and hybrid cloud deployments, including Nutanix NKP environments.

***

## 1. Foundational Concepts

### 1.1 Ollama's Native OpenAI Compatibility

Since early 2024, Ollama has shipped with a built-in OpenAI Chat Completions compatibility layer[^2]. Any OpenAI client SDK (Python, JavaScript, etc.) can target `http://<ollama-host>:11434` with a dummy API key and receive fully OpenAI-shaped responses[^3]. The supported endpoints are:

| OpenAI Endpoint | Ollama Mapping | Notes |
|---|---|---|
| `/v1/chat/completions` | `/v1/chat/completions` | Full support incl. streaming, JSON mode, vision, tools[^4] |
| `/v1/completions` | `/v1/completions` | Full support[^5] |
| `/v1/embeddings` | `/v1/embeddings` | Full support[^5] |
| `/v1/models` | `/v1/models` | Lists locally pulled models[^1] |
| `/api/tags` (native) | N/A | Model management, pull/push not in `/v1` path[^3] |

Ollama does **not** natively provide authentication for local deployments, load balancing across instances, or multi-tenant isolation[^6]. These capabilities require a gateway layer.

### 1.2 The Gateway Pattern

An AI gateway for Ollama acts as a reverse proxy that:

1. Accepts requests over a single, unified OpenAI-compatible endpoint
2. Routes requests to one or more backend Ollama instances using configurable load balancing strategies
3. Performs health checking and removes failed backends from the routing pool
4. Optionally enforces authentication, rate limiting, and usage tracking
5. Provides observability through metrics, traces, and structured logs

This pattern is directly analogous to a standard API gateway or service mesh sidecar but is purpose-built for LLM inference traffic, which has unique characteristics: long-lived streaming connections, GPU resource saturation, and latency profiles that differ from typical web traffic[^7].

***

## 2. Solution Landscape

### 2.1 Comparison of Major Solutions

| Solution | Language | License | Ollama Support | Load Balancing Strategies | Kubernetes Native | Auth | Observability |
|---|---|---|---|---|---|---|---|
| **LiteLLM Proxy** | Python | MIT | ✅ Full | Simple-shuffle, least-busy, latency-based, cost-based, usage-based[^8] | ✅ (Helm) | API key, JWT, OAuth2 | Prometheus, OpenTelemetry[^9] |
| **Olla** | Go | Apache 2.0 | ✅ Full | Priority, round-robin, least-connections[^10] | ✅ | Rate limiting, audit logs[^11] | Structured logs, metrics[^12] |
| **llm-gateway (OpenZiti)** | Go | Apache 2.0 | ✅ Full | Weighted round-robin, health checks, failover[^13] | ✅ | Zero-trust (OpenZiti) | Background health checks[^14] |
| **ollamaMQ** | Rust | MIT | ✅ Full | Least-connections + round-robin, per-user fair-share[^15] | ❌ (standalone) | X-User-ID header[^15] | Real-time TUI dashboard[^16] |
| **Inference Gateway** | Go | Apache 2.0 | ✅ Full | Standard routing | ✅ (CRDs, Operator) | OIDC/Keycloak, JWT[^17] | OpenTelemetry[^17] |
| **Traefik AI Gateway** | Go | Enterprise/OSS | ✅ Full | Kubernetes-native HTTPRoute[^18] | ✅ (GitOps) | Enterprise IAM | Prometheus, Grafana[^18] |
| **Ollama Proxy Fortress** | Python | MIT | ✅ Full | Round-robin federation[^19] | ❌ (standalone) | Bearer token / API key[^19] | Web UI model view |
| **GPUStack** | Python/Go | Apache 2.0 | Partial (own backends) | Cluster-level scheduling[^20] | ✅ | Built-in auth | Full observability[^21] |
| **ngrok AI Gateway** | SaaS | Commercial | ✅ Via tunnel[^22] | Traffic policies | Managed | ngrok auth | ngrok analytics |

***

## 3. Deep Dive: Top Solution Profiles

### 3.1 LiteLLM Proxy — Feature-Rich Enterprise Gateway

LiteLLM is the most widely deployed open-source AI gateway, exposing a single OpenAI-compatible endpoint and routing to over 100 backends[^23]. It is the de facto standard for teams running heterogeneous model providers alongside local Ollama instances.

**Key Capabilities:**
- Five routing strategies: `simple-shuffle` (default), `least-busy`, `latency-based-routing`, `cost-based-routing`, `usage-based-routing`[^8]
- Deployment ordering with `order` parameter for priority-based primary/fallback chains[^8]
- Redis-backed shared rate limit state for multi-instance LiteLLM deployments[^8]
- Virtual API keys with per-key budget controls, team scoping, and spend tracking[^24]
- Background health checks that proactively remove unhealthy deployments before users hit errors[^25]
- Handles 1,500+ requests/second during load tests[^24]

**Configuration Example — Multiple Ollama Backends:**

```yaml
# litellm-config.yaml
model_list:
  # Node 1 — Primary GPU host
  - model_name: llama3.1
    litellm_params:
      model: ollama/llama3.1
      api_base: http://ollama-node-01:11434
      rpm: 60

  # Node 2 — Secondary GPU host (same model name enables load balancing)
  - model_name: llama3.1
    litellm_params:
      model: ollama/llama3.1
      api_base: http://ollama-node-02:11434
      rpm: 60

  # Node 3 — Tertiary, lower RPM capacity
  - model_name: llama3.1
    litellm_params:
      model: ollama/llama3.1
      api_base: http://ollama-node-03:11434
      rpm: 30

  # Separate model pool — Embedding node
  - model_name: nomic-embed-text
    litellm_params:
      model: ollama/nomic-embed-text
      api_base: http://ollama-embed-01:11434

router_settings:
  routing_strategy: least-busy      # Route to node with fewest active requests
  num_retries: 3
  timeout: 120
  redis_host: redis.internal
  redis_port: 6379

general_settings:
  master_key: sk-enterprise-key-123  # Enables API key auth
```

**Starting the proxy:**
```bash
litellm --config litellm-config.yaml --port 4000 --num_workers 8
```

**Client usage (unchanged from OpenAI SDK):**
```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-enterprise-key-123",
    base_url="http://litellm-gateway:4000"
)

response = client.chat.completions.create(
    model="llama3.1",
    messages=[{"role": "user", "content": "Hello!"}]
)
```

***

### 3.2 Olla — High-Performance Go Proxy

Olla is a purpose-built, open-source LLM proxy written in Go, optimized for Ollama and similar local inference backends[^11]. It delivers sub-millisecond proxy overhead (~0.30ms p50) and 10,200 req/s throughput ceiling[^11], making it well-suited for resource-constrained on-premises environments.

**Key Capabilities:**
- Dual proxy engines: **Sherpa** (lightweight, simplicity-focused) and **Olla** (full-featured with circuit breakers, connection pooling)[^10]
- Priority-based routing with automatic failover — zero requests dropped across 20,223-request failover test[^11]
- Unified model registry aggregates models from all backends into a single `/v1/models` response[^11]
- Native LiteLLM backend type enables hybrid local-plus-cloud routing[^26]
- ~20MB idle memory, single 14.4MB static binary[^11]
- 14.8x lighter memory footprint than LiteLLM proxy[^11]

**Configuration Example — Priority-Based Failover:**

```yaml
# olla-config.yaml
discovery:
  type: "static"
  static:
    endpoints:
      - name: "primary-gpu-server"
        url: "http://10.10.1.10:11434"
        type: "ollama"
        priority: 100        # Highest priority — use first

      - name: "secondary-gpu-server"
        url: "http://10.10.1.11:11434"
        type: "ollama"
        priority: 80         # Used when primary is unavailable

      - name: "dev-workstation"
        url: "http://10.10.1.20:11434"
        type: "ollama"
        priority: 50         # Last resort

      - name: "litellm-cloud-fallback"
        url: "http://litellm:4000"
        type: "litellm"      # Routes to cloud providers if all local nodes fail
        priority: 25

proxy:
  load_balancer: "least-connections"

server:
  port: 40114
  rate_limits:
    per_ip_requests_per_minute: 120
    global_requests_per_minute: 2000
```

***

### 3.3 llm-gateway (OpenZiti) — Zero-Trust Networking Gateway

The OpenZiti team's `llm-gateway` is an OpenAI-compatible proxy that adds zero-trust network identity to LLM routing[^14]. It supports weighted round-robin distribution and automatic failover across multiple Ollama instances, and can optionally expose instances through an encrypted overlay network — no open ports or VPN required[^27].

**Key Capabilities:**
- Weighted round-robin across Ollama endpoints — `weight: 3` means 3x the requests of default weight[^13]
- Background health checks on a configurable interval with per-endpoint timeout[^13]
- Automatic failover: failed endpoints are removed from rotation; they re-enter automatically upon recovery[^27]
- Supports mixing `base_url` (direct) and `zrok_share_token` (zero-trust tunneled) endpoints[^13]
- Multi-provider routing: can front-end Ollama, OpenAI, Anthropic, and Azure OpenAI from a single endpoint[^28]

**Configuration Example — Weighted Multi-Endpoint:**

```yaml
# gateway-config.yaml
listen: ":8080"

providers:
  local:
    endpoints:
      - name: "gpu-rack-01"
        base_url: "http://10.10.1.10:11434"
        weight: 3             # 3x traffic share (high GPU capacity)

      - name: "gpu-rack-02"
        base_url: "http://10.10.1.11:11434"
        weight: 2

      - name: "edge-node"
        base_url: "http://10.10.2.5:11434"
        weight: 1             # Lighter node, reduced traffic

    health_check:
      interval_seconds: 30
      timeout_seconds: 5
```

**Install and run:**
```bash
go install github.com/openziti/llm-gateway/cmd/llm-gateway@latest
llm-gateway run gateway-config.yaml
```

***

### 3.4 ollamaMQ — Fair-Share Queuing Proxy

ollamaMQ is a Rust-based asynchronous message queue dispatcher purpose-built for multi-user environments where fairness under concurrent load matters[^15]. It excels in shared team environments or CI/CD pipelines where multiple agents compete for the same inference backends.

**Key Capabilities:**
- Per-user FIFO queues identified by `X-User-ID` header prevent any single consumer from monopolizing all backends[^15]
- Least-connections + round-robin hybrid scheduling[^29]
- Backend health checks every 10 seconds; failed instances are skipped and marked in the dashboard[^15]
- VIP mode for absolute priority and Boost mode for increased scheduling frequency per user[^15]
- Real-time TUI dashboard showing backend health, active requests, queue depths, and throughput[^16]
- Built on `tokio` + `axum` for high concurrency; fully async[^15]
- OpenAI-compatible endpoints (`/v1/chat/completions`, etc.)[^30]

***

### 3.5 Inference Gateway — Cloud-Native Kubernetes Operator

Inference Gateway is an open-source, cloud-native proxy with a Kubernetes Operator and Custom Resource Definitions (CRDs)[^17]. It is designed for declarative, GitOps-friendly cluster management and integrates OIDC authentication.

**Key Capabilities:**
- Kubernetes Operator manages gateways, agents, MCP servers, and chat orchestrators as Custom Resources[^17]
- OIDC authentication with Keycloak and any standards-compliant identity provider; JWT validation against issuer's JWKS[^17]
- Supports Ollama, OpenAI, Anthropic, Groq, Cohere, DeepSeek, Cloudflare, Google, Mistral, and more from a single endpoint[^31]
- Switch providers with one config change; no application redeploys[^17]
- MCP (Model Context Protocol) tools added once, available for every model that supports tool calls[^17]

***

### 3.6 Traefik AI Gateway — Kubernetes-Native Enterprise Option

Traefik AI Gateway extends the popular Traefik proxy with LLM-specific routing capabilities[^18]. It is declarative, fully GitOps-compatible, and integrates NVIDIA Safety NIMs and Presidio PII filtering.

**Kubernetes AIService CRD for Ollama:**

```yaml
apiVersion: hub.traefik.io/v1alpha1
kind: AIService
metadata:
  name: ollama-node-01
  namespace: ai-inference
spec:
  ollama:
    baseUrl: "http://ollama-01.ai-inference.svc.cluster.local:11434"
    model: "llama3.1"
---
apiVersion: hub.traefik.io/v1alpha1
kind: AIService
metadata:
  name: ollama-node-02
  namespace: ai-inference
spec:
  ollama:
    baseUrl: "http://ollama-02.ai-inference.svc.cluster.local:11434"
    model: "llama3.1"
```

Traefik Hub AI Gateway supports Ollama alongside Anthropic, Azure OpenAI, Bedrock, Cohere, DeepSeek, Gemini, Mistral, OpenAI, and Qwen[^32] — making it a natural fit for hybrid model routing across local and cloud providers.

***

## 4. Architecture Patterns

### 4.1 Flat Multi-Node Pool (Horizontal Scale)

The simplest gateway topology: all Ollama instances serve the same model(s), and the gateway distributes load round-robin or by least connections.

```
                        ┌─────────────────────────────┐
  OpenAI SDK Clients    │        AI Gateway Layer       │
  LangChain / Agents    │  (LiteLLM / Olla / llm-gw)   │
  Open WebUI            │   http://gateway:4000/v1      │
  Continue / VS Code    └──────────┬───────────┬────────┘
                                   │           │
                        ┌──────────┘           └──────────┐
                        ▼                                  ▼
             ┌─────────────────┐               ┌─────────────────┐
             │  Ollama Node 01  │               │  Ollama Node 02  │
             │  GPU: L40S       │               │  GPU: L40S       │
             │  :11434          │               │  :11434          │
             └─────────────────┘               └─────────────────┘
```

### 4.2 Tiered Priority Routing (Primary + Fallback)

Route to high-GPU-capacity nodes first; fail over to lower-capacity or cloud-backed endpoints when primary nodes are saturated or offline.

```
  Clients
     │
     ▼
  Gateway (Olla / LiteLLM with order= param)
     │
     ├── Priority 100: RTX 6000 Ada server  ──► Healthy → Serve
     ├── Priority 80:  H100 workstation     ──► Fallback if P100 down
     └── Priority 25:  LiteLLM → Cloud API  ──► Last resort fallback
```

### 4.3 Model-Sharded Topology (Different Models per Node)

Route by model name so specialized nodes serve specific model families.

```
  Gateway endpoint: http://gateway:4000
      │
      ├── model=llama3.1      → ollama-node-01 (large VRAM, 70B model)
      ├── model=llama3.1      → ollama-node-02 (large VRAM, 70B model)  [load balanced]
      ├── model=phi4-mini     → ollama-node-03 (smaller GPU, fast 4B model)
      ├── model=nomic-embed-text → ollama-embed-01 (embedding-optimized node)
      └── model=deepseek-coder → ollama-code-01  (code-focused node)
```

### 4.4 Kubernetes Sidecar / Service Mesh Pattern (NKP/AHV)

On Nutanix Kubernetes Platform, Ollama instances run as Kubernetes Deployments with GPU node affinity. The gateway runs as a separate Deployment or as a sidecar and targets Ollama Services over the cluster network.

```yaml
# Example: LiteLLM targeting Ollama K8s Services on NKP
model_list:
  - model_name: llama3.1
    litellm_params:
      model: ollama/llama3.1
      api_base: http://ollama-svc-01.ai-inference.svc.cluster.local:11434

  - model_name: llama3.1
    litellm_params:
      model: ollama/llama3.1
      api_base: http://ollama-svc-02.ai-inference.svc.cluster.local:11434
```

The Kubernetes Gateway API Inference Extension (introduced in K8s 1.30+) provides a standardized CRD-driven approach for this[^7], complementing tools like Traefik Hub or Inference Gateway's operator model.

***

## 5. Security Considerations

### 5.1 Authentication

Ollama has **no built-in authentication** for locally-served APIs[^6]. The gateway layer is the enforcement point for all authentication. Common patterns include:

- **API Key (Bearer Token):** LiteLLM `master_key` or virtual keys; Ollama Proxy Fortress bearer tokens[^19]
- **JWT / OIDC:** Inference Gateway with Keycloak/OIDC; validates JWT against the issuer's JWKS endpoint[^17]
- **OAuth 2.0 via Nginx/OAuth2-Proxy:** Nginx `auth_request` directive forwards validation to an OAuth2 provider (Keycloak, GitHub, Google)[^33]
- **Zero-Trust (mTLS + Identity):** OpenZiti llm-gateway wraps backend connections in cryptographic identity; no open ports, no VPN[^14]

### 5.2 Rate Limiting and Abuse Prevention

| Mechanism | Tool | Notes |
|---|---|---|
| Per-model RPM/TPM limits | LiteLLM `rpm:` / `tpm:` params | Hard limits return HTTP 429 with `retry-after` header[^8] |
| Per-IP rate limiting | Olla server config | `per_ip_requests_per_minute` and global rate[^34] |
| Per-user fair queuing | ollamaMQ | `X-User-ID` header enforces fair scheduling[^15] |
| Circuit breakers | Olla (Olla engine) | Automatic failure isolation, exponential backoff recovery[^10] |
| Request size caps | Olla | Protects backend from oversized payloads[^12] |

### 5.3 Network Isolation

Ollama backends should **never** be directly exposed to client networks. The gateway is the only network-accessible endpoint:

```
[Client Network / K8s Ingress]
        │
        ▼
[Gateway: LiteLLM / Olla / llm-gateway]   ← Only public-facing component
        │
[Internal/Private Network Only]
        ▼
[Ollama Node Pool]                         ← No external access
```

***

## 6. Observability and Monitoring

### 6.1 Metrics Stack

A production AI gateway deployment should export metrics to a standard observability stack:

```
Gateway (LiteLLM / agentgateway)
  ── OTLP gRPC ──► Jaeger (distributed traces)
  ── OTLP HTTP ──► Prometheus (metrics)
                       └──► Grafana (dashboards)
```

Key metrics to monitor[^9][^35]:
- **Request rate and latency (p50, p95, p99)** per model and per backend node
- **Token throughput** (input/output tokens per second)
- **Error rates** (4xx/5xx, timeout rates per backend)
- **Queue depth** (critical for understanding saturation in ollamaMQ)
- **Backend health state** (healthy/degraded/failed count)
- **GPU utilization** (via Prometheus node-exporter or DCGM exporter on Ollama hosts)

### 6.2 Structured Logging

Olla produces compact, millisecond-precision structured logs covering startup, health checks, and per-request routing decisions[^11]. LiteLLM logs to standard output with configurable verbosity and integrates with OpenTelemetry for trace-level request visibility[^9].

***

## 7. Solution Selection Guide

The right gateway depends on deployment scale, operational complexity tolerance, and security requirements:

| Scenario | Recommended Solution | Rationale |
|---|---|---|
| Single-team homelabs / dev cluster | **Olla** | Single binary, low overhead, minimal ops burden |
| Multi-team enterprise on-prem (bare metal/AHV) | **LiteLLM Proxy** | Rich auth, budget controls, broad provider support, Redis HA |
| Kubernetes-native (NKP / K8s) | **LiteLLM (Helm) + Inference Gateway Operator** | CRD-driven, GitOps-friendly, OIDC integration |
| Zero-trust / air-gapped environments | **llm-gateway (OpenZiti)** | Encrypted overlay, no open ports, no VPN |
| Multi-user shared inference pool | **ollamaMQ** | Per-user fairness, queue management, real-time TUI |
| Enterprise Kubernetes + policy enforcement | **Traefik AI Gateway Hub** | GitOps, NVIDIA Safety NIMs, PII filtering, full observability |
| Hybrid cloud (local + cloud fallback) | **Olla + LiteLLM backend** | Local-first priority with cloud API as automatic fallback |

***

## 8. Relevance to Nutanix Enterprise AI (NAI)

Nutanix Enterprise AI introduced an **AI Gateway** capability as part of its broader Agentic AI platform[^36]. This gateway provides a unified, secure inference endpoint for both cloud-hosted models and private LLMs deployed on NKP, with authentication, observability, and token-based rate limiting[^37]. The pattern is architecturally equivalent to the open-source approaches described in this document, embedded within the Nutanix operational model.

For enterprise clients running Ollama workloads on Nutanix AHV or NKP (pre-NAI adoption or alongside NAI), the recommended approach is:

1. **Deploy Ollama** as GPU-pinned Kubernetes Deployments on NKP with appropriate GPU node selectors and resource limits
2. **Front-end with LiteLLM or Olla** as the gateway layer, deployed as a Kubernetes Deployment with a ClusterIP or LoadBalancer Service
3. **Integrate with NKP's observability stack** (Prometheus/Grafana, deployed by NKP) for unified metrics
4. **Align authentication** with enterprise identity (Keycloak/OIDC via Inference Gateway, or LiteLLM virtual keys linked to LDAP/AD groups)
5. **Consider NAI's AI Gateway** for customers seeking a fully integrated, supported path as part of the Nutanix platform[^36][^37]

***

## 9. Appendix: Quick Reference Configurations

### LiteLLM Docker Compose (Production HA)

```yaml
# docker-compose.yml
services:
  litellm:
    image: ghcr.io/berriai/litellm:main-latest
    deploy:
      replicas: 2
    ports:
      - "4000-4001:4000"
    volumes:
      - ./litellm-config.yaml:/app/config.yaml
    command: ["--config", "/app/config.yaml", "--port", "4000", "--num_workers", "8"]
    environment:
      - LITELLM_MASTER_KEY=sk-prod-key

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
```

### Olla Docker Run

```bash
docker run -t --name olla \
  -p 40114:40114 \
  -v ./olla-config.yaml:/app/config.yaml \
  ghcr.io/thushan/olla:latest
```

### llm-gateway Minimal Config

```yaml
listen: ":8080"
providers:
  local:
    endpoints:
      - name: "node-01"
        base_url: "http://10.10.1.10:11434"
        weight: 2
      - name: "node-02"
        base_url: "http://10.10.1.11:11434"
        weight: 1
    health_check:
      interval_seconds: 30
      timeout_seconds: 5
```

### Verifying the Gateway (Any Solution)

```bash
# Health check
curl http://gateway:4000/health

# List federated models
curl http://gateway:4000/v1/models

# Test chat completion
curl http://gateway:4000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-prod-key" \
  -d '{
    "model": "llama3.1",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

***

*Report compiled June 2026. All solutions referenced are open-source unless otherwise noted. Version numbers and features evolve rapidly; consult official documentation for current release capabilities.*

---

## References

1. [What is Ollama? Local LLM Runtime 2026 - Future AGI](https://futureagi.com/blog/what-is-ollama-2026/) - By exposing a /v1/chat/completions endpoint on localhost:11434 , Ollama lets any tool that already s...

2. [OpenAI compatibility · Ollama Blog](https://ollama.com/blog/openai-compatibility) - Ollama now has built-in compatibility with the OpenAI Chat Completions API, making it possible to us...

3. [Ollama REST API: Integration into Application 2026](https://webscraft.org/blog/ollama-rest-api-integratsiya-u-sviy-zastosunok-java-python-javascript?lang=en) - Full guide to Ollama API: /api/chat, streaming, embeddings, tool calling. Examples in Java (WebClien...

4. [OpenAI Compatibility - Ollama English Documentation](https://ollama.readthedocs.io/en/openai/) - Ollama provides experimental compatibility with parts of the OpenAI API to help connect existing app...

5. [How to Use Ollama API](https://oneuptime.com/blog/post/2026-02-02-ollama-api/view) - A comprehensive guide to the Ollama API for building applications with local large language models. ...

6. [Support for API_KEY based authentication · Issue #8536](https://github.com/ollama/ollama/issues/8536) - Would be great if Ollama server would support some basic level API_KEY-based authentication. Use cas...

7. [Introducing Gateway API Inference Extension - Kubernetes](https://kubernetes.io/blog/2025/06/05/introducing-gateway-api-inference-extension/) - Modern generative AI and large language model (LLM) services create unique traffic-routing challenge...

8. [Proxy - Load Balancing - LiteLLM](https://docs.litellm.ai/docs/proxy/load_balancing) - Load balance multiple instances of the same model

9. [Observability | AISIX AI Gateway Docs](https://docs.api7.ai/aisix/observability) - Monitor LLM traffic with OpenTelemetry distributed tracing and Prometheus metrics. Gain full visibil...

10. [README ¶](https://pkg.go.dev/github.com/thushan/olla@v0.0.11)

11. [Olla - Open Source LLM Proxy - TensorFoundry](https://tensorfoundry.io/products/olla) - Open-source LLM proxy for small businesses and growing teams. Dual proxy engines, circuit breakers, ...

12. [Olla v0.0.16 - Lightweight LLM Proxy for Homelab & OnPrem AI ...](https://www.reddit.com/r/ollama/comments/1mtjbfj/olla_v0016_lightweight_llm_proxy_for_homelab/) - It's a lightweight Go proxy that sits in front of Ollama, LM Studio, vLLM or OpenAI-compatible backe...

13. [llm-gateway/docs/multi-endpoint.md at main - GitHub](https://github.com/openziti/llm-gateway/blob/main/docs/multi-endpoint.md) - the gateway can distribute requests across them with weighted round-robin load balancing, health che...

14. [openziti.ai - Identity-First Connectivity™ for AI](https://openziti.ai) - Two open source gateways built on OpenZiti. Route AI clients to tools and models through an encrypte...

15. [ollamaMQ](https://lib.rs/crates/ollamamq) - High-performance Ollama proxy with per-user fair-share queuing, round-robin scheduling, and a real-t...

16. [ollamaMQ - simple proxy with fair-share queuing + nice TUI](https://www.reddit.com/r/ollama/comments/1rgbvdi/ollamamq_simple_proxy_with_fairshare_queuing_nice/) - ollamaMQ - simple proxy with fair-share queuing + nice TUI

17. [Inference Gateway Documentation](https://docs.inference-gateway.com) - Documentation for Inference Gateway, an open-source, cloud-native gateway unifying multiple LLM prov...

18. [Traefik AI Gateway: Turn any AI endpoint into a managed API](https://traefik.io/solutions/ai-gateway) - Traefik AI Gateway accelerates AI adoption by turning any AI endpoint into an API that can be manage...

19. [A proxy server for multiple ollama instances with Key security](https://github.com/ParisNeo/ollama_proxy_server) - A proxy server for multiple ollama instances with Key security - ParisNeo/ollama_proxy_server

20. [Overview - GPUStack](https://docs.gpustack.ai/0.4/overview/)

21. [OpenAI Compatible APIs - GPUStack](https://docs.gpustack.ai/0.7/user-guide/openai-compatible-apis/)

22. [Ollama - ngrok documentation](https://ngrok.com/docs/ai-gateway/custom-providers/ollama)

23. [LiteLLM proxy routes between Ollama, vLLM, and cloud APIs ...](https://openclawsome.com/news/ai/litellm-unified-proxy-multi-model-routing) - LiteLLM is an open-source proxy that exposes a single OpenAI-compatible API endpoint and routes requ...

24. [Quick Start - LiteLLM Proxy CLI](https://docs.litellm.ai/docs/proxy/quick_start) - Setup LiteLLM Proxy quickly via CLI.

25. [Routing & Load Balancing](https://docs.litellm.ai/docs/routing-load-balancing) - Learn how to load balance, route, and set fallbacks for your LLM requests

26. [Olla vs Alternatives - LLM Proxy Comparison Guide](https://thushan.github.io/olla/compare/overview/) - Compare Olla with LiteLLM, GPUStack, LocalAI, and other LLM infrastructure tools. Find the right too...

27. [Open source load balancer for Ollama instances](https://www.reddit.com/r/LocalLLaMA/comments/1s3ctq3/open_source_load_balancer_for_ollama_instances/) - Open source load balancer for Ollama instances

28. [llm-gateway module - github.com/openziti/llm-gateway - Go Packages](https://pkg.go.dev/github.com/openziti/llm-gateway@v0.1.3)

29. [ollamaMQ 0.2.6 on Cargo - Libraries.io - security & maintenance ...](https://libraries.io/cargo/ollamaMQ) - High-performance Ollama proxy with per-user fair-share queuing, round-robin scheduling, and a real-t...

30. [New TUI dropped for managing LLM traffic and GPU resources](https://www.linkedin.com/posts/orhunp_new-tui-dropped-for-managing-llm-traffic-activity-7433945049995968512-ZKEf) - New TUI dropped for managing LLM traffic and GPU resources 🔥

31. [Inference Gateway](https://github.com/inference-gateway) - An open-source, cloud-native, high-performance gateway unifying multiple LLM providers, from local s...

32. [AI Gateway | Traefik Hub Documentation](https://doc.traefik.io/traefik-hub/api-gateway/reference/routing/http/load-balancing/ref-ai-service) - AI Gateway in Traefik Hub offers effortless integration with various popular LLMs, eliminating the n...

33. [Securing Ollama API: Authentication and Rate Limiting - Dasroot!](https://dasroot.net/posts/2025/12/securing-ollama-api-authentication-rate-limiting/) - Learn how to secure the Ollama API using authentication methods like OAuth 2.0, API keys, and JWT, c...

34. [Usage - Using Olla - Olla](https://thushan.github.io/olla/usage/) - Usecases and usage scenarios with Olla

35. [Observability Stack: Monitoring Your AI Gateway - Sebastian Maniak](https://maniak.io/articles/02-observability-stack-monitoring-your-ai-gateway/) - This setup will give you real-time visibility into your AI Gateway's performance, cost metrics, toke...

36. [Building the Enterprise AI Stack with Nutanix Agentic AI](https://www.nutanix.com/blog/building-enterprise-ai-stack-with-nutanix-agentic-ai) - In the race to deploy GenAI, the "last mile" is often the hardest. Data scientists and developers do...

37. [Nutanix Enterprise AI Datasheet](https://www.nutanix.com/library/datasheets/nutanix-enterprise-ai) - The Nutanix Enterprise AI (NAI) solution offers endpoint APIs for the leading LLM providers, includi...

