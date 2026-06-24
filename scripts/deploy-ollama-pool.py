#!/usr/bin/env python3
"""Interactive Ollama pool deployer for an Olla-fronted OpenAI-compatible endpoint.

This script intentionally keeps the workflow in one place:
  1. deploy/upgrade Olla,
  2. deploy one Ollama instance for a selected model,
  3. register that instance in Olla's load-balancing pool,
  4. deploy a small API-key web gateway in front of Olla.

Registration is implemented with Kubernetes primitives: the script creates a stable
ClusterIP Service for each Ollama instance and rewrites the Olla ConfigMap endpoint
list, then restarts Olla. This gives operators an idempotent "announce/register"
flow even though Olla itself does not expose a native registration API.
"""
from __future__ import annotations

import argparse
import base64
import os
import secrets
import shutil
import subprocess
import sys
import textwrap
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

DEFAULT_NAMESPACE = "ai-local"
DEFAULT_MODEL = "llama3.1"
DEFAULT_INSTANCE = "ollama-node-01"
DEFAULT_STORAGE_CLASS = "nutanix-volume"
DEFAULT_STORAGE_SIZE = "100Gi"
DEFAULT_GPU_SELECTOR = "nvidia.com/gpu.present=true,accelerator=l40s"
DEFAULT_OLLA_IMAGE = "ghcr.io/thushan/olla:latest"
DEFAULT_OLLAMA_IMAGE = "ollama/ollama:latest"
DEFAULT_PORTAL_IMAGE = "python:3.12-alpine"
DEFAULT_GATEWAY_SERVICE_TYPE = "LoadBalancer"
OLLA_CONFIG_NAME = "olla-config"
OLLA_DEPLOYMENT_NAME = "olla"
OLLA_SERVICE_NAME = "olla-svc"
PORTAL_CONFIG_NAME = "olla-api-key-gateway-app"
PORTAL_KEYS_SECRET = "olla-api-keys"
PORTAL_DEPLOYMENT_NAME = "olla-api-key-gateway"
PORTAL_SERVICE_NAME = "olla-openai-gateway"


@dataclass
class Settings:
    namespace: str
    model: str
    instance: str
    storage_class: str
    storage_size: str
    gpu_selector: str
    olla_image: str
    ollama_image: str
    portal_image: str
    gateway_service_type: str
    apply: bool


def run(cmd: list[str], *, input_text: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    print("$ " + " ".join(cmd), file=sys.stderr)
    return subprocess.run(cmd, input=input_text, text=True, check=check, capture_output=True)


def kubectl(args: list[str], *, input_text: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run(["kubectl", *args], input_text=input_text, check=check)


def require_tool(name: str) -> None:
    if shutil.which(name) is None:
        raise SystemExit(f"Required tool '{name}' was not found in PATH")


def prompt(label: str, default: str) -> str:
    value = input(f"{label} [{default}]: ").strip()
    return value or default


def parse_selector(selector: str) -> dict[str, str]:
    result: dict[str, str] = {}
    for item in selector.split(","):
        item = item.strip()
        if not item:
            continue
        if "=" not in item:
            raise SystemExit(f"Invalid selector '{item}'. Expected key=value.")
        key, value = item.split("=", 1)
        result[key.strip()] = value.strip()
    return result


def yaml_map(data: dict[str, str], indent: int) -> str:
    pad = " " * indent
    return "\n".join(f'{pad}{key}: "{value}"' for key, value in data.items())


def apply_yaml(manifest: str, apply: bool) -> None:
    if apply:
        kubectl(["apply", "-f", "-"], input_text=manifest)
    else:
        print("---")
        print(manifest.rstrip())


def get_existing_endpoints(namespace: str) -> list[tuple[str, str, int]]:
    proc = kubectl(["get", "configmap", OLLA_CONFIG_NAME, "-n", namespace, "-o", "jsonpath={.binaryData.endpoints}"], check=False)
    if proc.returncode != 0 or not proc.stdout.strip():
        return []
    try:
        registry = base64.b64decode(proc.stdout.strip()).decode()
    except Exception:
        registry = proc.stdout
    endpoints: list[tuple[str, str, int]] = []
    for raw in registry.splitlines():
        parts = raw.split("|")
        if len(parts) != 3:
            continue
        name, url, priority = parts
        try:
            endpoints.append((name, url, int(priority)))
        except ValueError:
            continue
    return endpoints


def render_olla_config(endpoints: Iterable[tuple[str, str, int]]) -> str:
    endpoint_lines = []
    for name, url, priority in endpoints:
        endpoint_lines.extend([
            f"          - name: {name}",
            f"            url: {url}",
            "            type: ollama",
            f"            priority: {priority}",
            "            check_interval: 10s",
            "            check_timeout: 3s",
        ])
    if not endpoint_lines:
        endpoint_lines.extend([
            "          # No registered Ollama endpoints yet.",
            "          # Re-run this script and deploy an Ollama instance to populate the pool.",
        ])
    endpoints_text = "\n".join(endpoint_lines)
    return textwrap.dedent(f"""
    discovery:
      type: static
      static:
        endpoints:
    {endpoints_text}
    proxy:
      load_balancer: least-connections
    server:
      port: 40114
      rate_limits:
        per_ip_requests_per_minute: 300
        global_requests_per_minute: 5000
    logging:
      level: info
      format: json
    """).strip() + "\n"


def endpoint_registry_text(endpoints: Iterable[tuple[str, str, int]]) -> str:
    return "\n".join(f"{name}|{url}|{priority}" for name, url, priority in endpoints) + "\n"


def deploy_namespace(namespace: str, apply: bool) -> None:
    apply_yaml(f"""
apiVersion: v1
kind: Namespace
metadata:
  name: {namespace}
""", apply)


def deploy_olla(namespace: str, image: str, endpoints: list[tuple[str, str, int]], apply: bool) -> None:
    config_b64 = base64.b64encode(render_olla_config(endpoints).encode()).decode()
    registry_b64 = base64.b64encode(endpoint_registry_text(endpoints).encode()).decode()
    manifest = f"""
apiVersion: v1
kind: ConfigMap
metadata:
  name: {OLLA_CONFIG_NAME}
  namespace: {namespace}
binaryData:
  olla.yaml: {config_b64}
  endpoints: {registry_b64}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {OLLA_DEPLOYMENT_NAME}
  namespace: {namespace}
  labels:
    app: olla
spec:
  replicas: 1
  selector:
    matchLabels:
      app: olla
  template:
    metadata:
      labels:
        app: olla
    spec:
      containers:
        - name: olla
          image: {image}
          imagePullPolicy: IfNotPresent
          args: ["--config", "/app/config/olla.yaml"]
          ports:
            - name: http
              containerPort: 40114
          volumeMounts:
            - name: config
              mountPath: /app/config
          readinessProbe:
            httpGet:
              path: /health
              port: 40114
            initialDelaySeconds: 3
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /health
              port: 40114
            initialDelaySeconds: 10
            periodSeconds: 10
      volumes:
        - name: config
          configMap:
            name: {OLLA_CONFIG_NAME}
---
apiVersion: v1
kind: Service
metadata:
  name: {OLLA_SERVICE_NAME}
  namespace: {namespace}
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
"""
    apply_yaml(manifest, apply)
    if apply:
        kubectl(["rollout", "restart", f"deployment/{OLLA_DEPLOYMENT_NAME}", "-n", namespace], check=False)


def deploy_ollama_instance(settings: Settings) -> tuple[str, str, int]:
    service_name = f"{settings.instance}-svc"
    pvc_name = f"{settings.instance}-models"
    selector = parse_selector(settings.gpu_selector)
    selector_yaml = yaml_map(selector, 8)
    manifest = f"""
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {pvc_name}
  namespace: {settings.namespace}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: {settings.storage_class}
  resources:
    requests:
      storage: {settings.storage_size}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {settings.instance}
  namespace: {settings.namespace}
  labels:
    app: ollama
    ollama-pool-instance: {settings.instance}
    ollama.ai/model: {settings.model}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ollama
      ollama-pool-instance: {settings.instance}
  template:
    metadata:
      labels:
        app: ollama
        ollama-pool-instance: {settings.instance}
        ollama.ai/model: {settings.model}
    spec:
      tolerations:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      nodeSelector:
{selector_yaml}
      containers:
        - name: ollama
          image: {settings.ollama_image}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 11434
          env:
            - name: OLLAMA_HOST
              value: 0.0.0.0
            - name: OLLAMA_NUM_PARALLEL
              value: "4"
            - name: OLLAMA_MAX_LOADED_MODELS
              value: "1"
          resources:
            limits:
              nvidia.com/gpu: "1"
            requests:
              nvidia.com/gpu: "1"
              memory: 16Gi
              cpu: "4"
          volumeMounts:
            - name: models
              mountPath: /root/.ollama
          readinessProbe:
            httpGet:
              path: /api/tags
              port: 11434
            initialDelaySeconds: 10
            periodSeconds: 10
          livenessProbe:
            httpGet:
              path: /api/tags
              port: 11434
            initialDelaySeconds: 30
            periodSeconds: 20
      volumes:
        - name: models
          persistentVolumeClaim:
            claimName: {pvc_name}
---
apiVersion: v1
kind: Service
metadata:
  name: {service_name}
  namespace: {settings.namespace}
  labels:
    app: ollama
    ollama-pool-instance: {settings.instance}
    ollama.ai/model: {settings.model}
spec:
  type: ClusterIP
  selector:
    app: ollama
    ollama-pool-instance: {settings.instance}
  ports:
    - name: http
      port: 11434
      targetPort: 11434
"""
    apply_yaml(manifest, settings.apply)
    url = f"http://{service_name}.{settings.namespace}.svc.cluster.local:11434"
    return settings.instance, url, 100


def deploy_model_pull_job(settings: Settings) -> None:
    job_name = f"{settings.instance}-pull-{settings.model.replace('.', '-').replace(':', '-')}"
    manifest = f"""
apiVersion: batch/v1
kind: Job
metadata:
  name: {job_name}
  namespace: {settings.namespace}
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: pull-model
          image: curlimages/curl:8.8.0
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -eu
              until curl -fsS http://{settings.instance}-svc:11434/api/tags >/dev/null; do
                echo "waiting for Ollama service"
                sleep 5
              done
              curl -fsS http://{settings.instance}-svc:11434/api/pull \
                -H 'Content-Type: application/json' \
                -d '{{"name":"{settings.model}","stream":false}}'
"""
    apply_yaml(manifest, settings.apply)


PORTAL_APP = r'''
#!/usr/bin/env python3
import http.server
import json
import os
import secrets
import urllib.error
import urllib.request
from pathlib import Path

UPSTREAM = os.environ.get("OLLA_UPSTREAM", "http://olla-svc:40114")
MODEL_NAME = os.environ.get("MODEL_NAME", "llama3.1")
KEY_FILE = Path(os.environ.get("KEY_FILE", "/data/keys.txt"))


def load_keys():
    if not KEY_FILE.exists():
        return set()
    return {line.strip() for line in KEY_FILE.read_text().splitlines() if line.strip()}


def save_key(key):
    KEY_FILE.parent.mkdir(parents=True, exist_ok=True)
    with KEY_FILE.open("a") as handle:
        handle.write(key + "\n")


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "OllaApiKeyGateway/1.0"

    def _send(self, status, body, content_type="text/html"):
        if isinstance(body, str):
            body = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ("/", "/index.html"):
            self._send(200, f"""
<html><head><title>Olla API Key Gateway</title></head>
<body>
<h1>Olla OpenAI-Compatible Gateway</h1>
<p>Published model: <code>{MODEL_NAME}</code></p>
<form method="POST" action="/generate-api-key">
  <button type="submit">Generate API key</button>
</form>
</body></html>
""")
            return
        if self.path.startswith("/v1/"):
            self._proxy()
            return
        self._send(404, "not found", "text/plain")

    def do_POST(self):
        if self.path == "/generate-api-key":
            key = "sk-olla-" + secrets.token_urlsafe(32)
            save_key(key)
            self._send(200, f"""
<html><body>
<h1>API key generated</h1>
<p>Copy this key now. It will not be displayed again.</p>
<p><code>{key}</code></p>
<p>Use it as: <code>Authorization: Bearer {key}</code></p>
<a download="olla-api-key.txt" href="data:text/plain,{key}">Download key</a>
</body></html>
""")
            return
        if self.path.startswith("/v1/"):
            self._proxy()
            return
        self._send(404, "not found", "text/plain")

    def _authorized(self):
        header = self.headers.get("Authorization", "")
        if not header.startswith("Bearer "):
            return False
        return header.removeprefix("Bearer ").strip() in load_keys()

    def _proxy(self):
        if not self._authorized():
            self._send(401, json.dumps({"error": "missing or invalid bearer token"}), "application/json")
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else None
        target = UPSTREAM + self.path
        headers = {key: value for key, value in self.headers.items() if key.lower() not in {"host", "authorization", "content-length"}}
        request = urllib.request.Request(target, data=body, method=self.command, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=600) as response:
                data = response.read()
                content_type = response.headers.get("Content-Type", "application/json")
                self._send(response.status, data, content_type)
        except urllib.error.HTTPError as exc:
            self._send(exc.code, exc.read(), exc.headers.get("Content-Type", "application/json"))
        except Exception as exc:
            self._send(502, json.dumps({"error": str(exc)}), "application/json")


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'''


def deploy_api_key_gateway(settings: Settings) -> None:
    app_b64 = base64.b64encode(PORTAL_APP.encode()).decode()
    initial_key = "sk-olla-" + secrets.token_urlsafe(32)
    keys_b64 = base64.b64encode((initial_key + "\n").encode()).decode()
    manifest = f"""
apiVersion: v1
kind: ConfigMap
metadata:
  name: {PORTAL_CONFIG_NAME}
  namespace: {settings.namespace}
binaryData:
  gateway.py: {app_b64}
---
apiVersion: v1
kind: Secret
metadata:
  name: {PORTAL_KEYS_SECRET}
  namespace: {settings.namespace}
type: Opaque
binaryData:
  keys.txt: {keys_b64}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {PORTAL_DEPLOYMENT_NAME}
  namespace: {settings.namespace}
  labels:
    app: olla-api-key-gateway
spec:
  replicas: 1
  selector:
    matchLabels:
      app: olla-api-key-gateway
  template:
    metadata:
      labels:
        app: olla-api-key-gateway
    spec:
      initContainers:
        - name: seed-api-keys
          image: {settings.portal_image}
          command: ["/bin/sh", "-c"]
          args: ["cp /seed/keys.txt /data/keys.txt && chmod 0600 /data/keys.txt"]
          volumeMounts:
            - name: key-seed
              mountPath: /seed
            - name: keys
              mountPath: /data
      containers:
        - name: gateway
          image: {settings.portal_image}
          command: ["python", "/app/gateway.py"]
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: OLLA_UPSTREAM
              value: http://{OLLA_SERVICE_NAME}.{settings.namespace}.svc.cluster.local:40114
            - name: MODEL_NAME
              value: {settings.model}
            - name: KEY_FILE
              value: /data/keys.txt
          volumeMounts:
            - name: app
              mountPath: /app
            - name: keys
              mountPath: /data
      volumes:
        - name: app
          configMap:
            name: {PORTAL_CONFIG_NAME}
            defaultMode: 0555
        - name: key-seed
          secret:
            secretName: {PORTAL_KEYS_SECRET}
        - name: keys
          emptyDir: {{}}
---
apiVersion: v1
kind: Service
metadata:
  name: {PORTAL_SERVICE_NAME}
  namespace: {settings.namespace}
  labels:
    app: olla-api-key-gateway
spec:
  type: {settings.gateway_service_type}
  selector:
    app: olla-api-key-gateway
  ports:
    - name: http
      port: 80
      targetPort: 8080
"""
    apply_yaml(manifest, settings.apply)
    print(f"\nInitial API key: {initial_key}", file=sys.stderr)
    print(f"Generate more keys from the {PORTAL_SERVICE_NAME} web UI.", file=sys.stderr)


def build_settings(args: argparse.Namespace) -> Settings:
    if args.yes:
        return Settings(
            namespace=args.namespace,
            model=args.model,
            instance=args.instance,
            storage_class=args.storage_class,
            storage_size=args.storage_size,
            gpu_selector=args.gpu_selector,
            olla_image=args.olla_image,
            ollama_image=args.ollama_image,
            portal_image=args.portal_image,
            gateway_service_type=args.gateway_service_type,
            apply=not args.dry_run,
        )
    print("Ollama pool deployment wizard\n")
    return Settings(
        namespace=prompt("Kubernetes namespace", args.namespace),
        model=prompt("Single published Ollama model", args.model),
        instance=prompt("Ollama instance name", args.instance),
        storage_class=prompt("Model PVC StorageClass", args.storage_class),
        storage_size=prompt("Model PVC size", args.storage_size),
        gpu_selector=prompt("GPU node selector key=value pairs", args.gpu_selector),
        olla_image=prompt("Olla image", args.olla_image),
        ollama_image=prompt("Ollama image", args.ollama_image),
        portal_image=prompt("API-key web gateway image", args.portal_image),
        gateway_service_type=prompt("API-key gateway Service type", args.gateway_service_type),
        apply=not args.dry_run,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="Deploy an Ollama instance, register it with Olla, and expose an API-key protected OpenAI endpoint.")
    parser.add_argument("--yes", action="store_true", help="Run non-interactively with defaults/flags.")
    parser.add_argument("--dry-run", action="store_true", help="Print manifests instead of applying them.")
    parser.add_argument("--namespace", default=DEFAULT_NAMESPACE)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--instance", default=DEFAULT_INSTANCE)
    parser.add_argument("--storage-class", default=DEFAULT_STORAGE_CLASS)
    parser.add_argument("--storage-size", default=DEFAULT_STORAGE_SIZE)
    parser.add_argument("--gpu-selector", default=DEFAULT_GPU_SELECTOR)
    parser.add_argument("--olla-image", default=DEFAULT_OLLA_IMAGE)
    parser.add_argument("--ollama-image", default=DEFAULT_OLLAMA_IMAGE)
    parser.add_argument("--portal-image", default=DEFAULT_PORTAL_IMAGE)
    parser.add_argument("--gateway-service-type", default=DEFAULT_GATEWAY_SERVICE_TYPE, choices=["ClusterIP", "NodePort", "LoadBalancer"])
    args = parser.parse_args()

    settings = build_settings(args)
    if settings.apply:
        require_tool("kubectl")

    deploy_namespace(settings.namespace, settings.apply)
    existing = get_existing_endpoints(settings.namespace) if settings.apply else []
    endpoint = deploy_ollama_instance(settings)
    endpoints = [item for item in existing if item[0] != endpoint[0]] + [endpoint]
    deploy_olla(settings.namespace, settings.olla_image, endpoints, settings.apply)
    deploy_model_pull_job(settings)
    deploy_api_key_gateway(settings)

    if settings.apply:
        print("\nDeployment submitted. Useful follow-up commands:")
        print(f"kubectl get pods,svc -n {settings.namespace}")
        print(f"kubectl logs -n {settings.namespace} deploy/{OLLA_DEPLOYMENT_NAME}")
        print(f"kubectl get svc -n {settings.namespace} {PORTAL_SERVICE_NAME}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
