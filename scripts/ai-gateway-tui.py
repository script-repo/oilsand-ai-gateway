#!/usr/bin/env python3
"""Text UI launcher for Olla/Ollama pool workflows.

The launcher is intentionally workflow-oriented:
- deploy Olla first as either Kubernetes resources or Docker containers on a VM/host;
- deploy Ollama workers later, greenfield or brownfield;
- register every worker with the selected Olla gateway;
- keep one predefined model exposed through the pool.

For Kubernetes, worker registration updates Olla's ConfigMap-backed endpoint registry.
For Docker/VM style deployments, worker registration updates a local Olla config file
and restarts the Olla container.
"""
from __future__ import annotations

import base64
import os
import secrets
import shutil
import subprocess
import sys
import textwrap
from pathlib import Path

DEFAULT_MODEL = "llama3.1"
DEFAULT_NAMESPACE = "ai-local"
DEFAULT_STATE_DIR = Path.home() / ".oilsand-ai-gateway"
DEFAULT_OLLA_IMAGE = "ghcr.io/thushan/olla:latest"
DEFAULT_OLLAMA_IMAGE = "ollama/ollama:latest"


def run(cmd: list[str], *, input_text: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    print("$ " + " ".join(cmd))
    return subprocess.run(cmd, input=input_text, text=True, check=check, capture_output=True)


def require(tool: str) -> None:
    if shutil.which(tool) is None:
        raise SystemExit(f"Missing required tool: {tool}")


def ask(label: str, default: str = "") -> str:
    suffix = f" [{default}]" if default else ""
    value = input(f"{label}{suffix}: ").strip()
    return value or default


def choose(label: str, options: list[str]) -> str:
    print(f"\n{label}")
    for idx, option in enumerate(options, 1):
        print(f"  {idx}. {option}")
    while True:
        raw = ask("Select", "1")
        if raw.isdigit() and 1 <= int(raw) <= len(options):
            return options[int(raw) - 1]
        print("Invalid selection")


def endpoint_registry_path(state_dir: Path) -> Path:
    return state_dir / "endpoints.txt"


def olla_config_path(state_dir: Path) -> Path:
    return state_dir / "olla.yaml"


def read_registry(state_dir: Path) -> list[tuple[str, str, int]]:
    path = endpoint_registry_path(state_dir)
    if not path.exists():
        return []
    endpoints: list[tuple[str, str, int]] = []
    for line in path.read_text().splitlines():
        parts = line.split("|")
        if len(parts) != 3:
            continue
        name, url, priority = parts
        endpoints.append((name, url, int(priority)))
    return endpoints


def write_registry(state_dir: Path, endpoints: list[tuple[str, str, int]]) -> None:
    state_dir.mkdir(parents=True, exist_ok=True)
    endpoint_registry_path(state_dir).write_text("\n".join(f"{n}|{u}|{p}" for n, u, p in endpoints) + "\n")


def render_olla_config(endpoints: list[tuple[str, str, int]]) -> str:
    rows: list[str] = []
    for name, url, priority in endpoints:
        rows.extend([
            f"          - name: {name}",
            f"            url: {url}",
            "            type: ollama",
            f"            priority: {priority}",
            "            check_interval: 10s",
            "            check_timeout: 3s",
        ])
    if not rows:
        rows = ["          # Add Ollama endpoints by deploying or registering workers."]
    return textwrap.dedent("""
    discovery:
      type: static
      static:
        endpoints:
    {endpoints}
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
    """).format(endpoints="\n".join(rows)).strip() + "\n"


def upsert_endpoint(endpoints: list[tuple[str, str, int]], endpoint: tuple[str, str, int]) -> list[tuple[str, str, int]]:
    return [item for item in endpoints if item[0] != endpoint[0]] + [endpoint]


def kubectl_apply(manifest: str) -> None:
    require("kubectl")
    run(["kubectl", "apply", "-f", "-"], input_text=manifest)


def k8s_get_endpoints(namespace: str) -> list[tuple[str, str, int]]:
    require("kubectl")
    proc = run(["kubectl", "get", "configmap", "olla-config", "-n", namespace, "-o", "jsonpath={.binaryData.endpoints}"], check=False)
    if proc.returncode != 0 or not proc.stdout.strip():
        return []
    try:
        decoded = base64.b64decode(proc.stdout.strip()).decode()
    except Exception:
        decoded = proc.stdout
    endpoints: list[tuple[str, str, int]] = []
    for line in decoded.splitlines():
        parts = line.split("|")
        if len(parts) == 3:
            endpoints.append((parts[0], parts[1], int(parts[2])))
    return endpoints


def k8s_apply_olla(namespace: str, endpoints: list[tuple[str, str, int]], image: str = DEFAULT_OLLA_IMAGE) -> None:
    config = base64.b64encode(render_olla_config(endpoints).encode()).decode()
    registry = base64.b64encode(("\n".join(f"{n}|{u}|{p}" for n, u, p in endpoints) + "\n").encode()).decode()
    manifest = f"""
apiVersion: v1
kind: Namespace
metadata:
  name: {namespace}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: olla-config
  namespace: {namespace}
binaryData:
  olla.yaml: {config}
  endpoints: {registry}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: olla
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
          args: ["--config", "/app/config/olla.yaml"]
          ports:
            - name: http
              containerPort: 40114
          volumeMounts:
            - name: config
              mountPath: /app/config
      volumes:
        - name: config
          configMap:
            name: olla-config
---
apiVersion: v1
kind: Service
metadata:
  name: olla-svc
  namespace: {namespace}
spec:
  selector:
    app: olla
  ports:
    - name: http
      port: 40114
      targetPort: 40114
"""
    kubectl_apply(manifest)
    run(["kubectl", "rollout", "restart", "deployment/olla", "-n", namespace], check=False)


def k8s_deploy_ollama(namespace: str, name: str, model: str, selector: str, storage_class: str, image: str = DEFAULT_OLLAMA_IMAGE) -> tuple[str, str, int]:
    selector_lines = "\n".join(f'        {k.strip()}: "{v.strip()}"' for k, v in (item.split("=", 1) for item in selector.split(",") if item.strip()))
    svc = f"{name}-svc"
    manifest = f"""
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {name}-models
  namespace: {namespace}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: {storage_class}
  resources:
    requests:
      storage: 100Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {name}
  namespace: {namespace}
  labels:
    app: ollama
    ollama.ai/model: {model}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ollama
      instance: {name}
  template:
    metadata:
      labels:
        app: ollama
        instance: {name}
        ollama.ai/model: {model}
    spec:
      tolerations:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      nodeSelector:
{selector_lines}
      containers:
        - name: ollama
          image: {image}
          ports:
            - name: http
              containerPort: 11434
          env:
            - name: OLLAMA_HOST
              value: 0.0.0.0
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
      volumes:
        - name: models
          persistentVolumeClaim:
            claimName: {name}-models
---
apiVersion: v1
kind: Service
metadata:
  name: {svc}
  namespace: {namespace}
spec:
  selector:
    app: ollama
    instance: {name}
  ports:
    - name: http
      port: 11434
      targetPort: 11434
"""
    kubectl_apply(manifest)
    pull = f"{name}-pull-{model.replace('.', '-').replace(':', '-')}"
    job = f"""
apiVersion: batch/v1
kind: Job
metadata:
  name: {pull}
  namespace: {namespace}
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: pull
          image: curlimages/curl:8.8.0
          command: ["/bin/sh", "-c"]
          args: ["until curl -fsS http://{svc}:11434/api/tags; do sleep 5; done; curl -fsS http://{svc}:11434/api/pull -H 'Content-Type: application/json' -d '{{\"name\":\"{model}\",\"stream\":false}}'"]
"""
    kubectl_apply(job)
    return name, f"http://{svc}.{namespace}.svc.cluster.local:11434", 100


def docker_apply_olla(state_dir: Path, endpoints: list[tuple[str, str, int]], image: str = DEFAULT_OLLA_IMAGE) -> None:
    require("docker")
    state_dir.mkdir(parents=True, exist_ok=True)
    write_registry(state_dir, endpoints)
    olla_config_path(state_dir).write_text(render_olla_config(endpoints))
    run(["docker", "network", "create", "oilsand-ai"], check=False)
    run(["docker", "rm", "-f", "olla-gateway"], check=False)
    run([
        "docker", "run", "-d", "--name", "olla-gateway", "--network", "oilsand-ai",
        "-p", "40114:40114", "-v", f"{olla_config_path(state_dir)}:/app/config/olla.yaml:ro",
        image, "--config", "/app/config/olla.yaml",
    ])


def docker_deploy_ollama(state_dir: Path, name: str, model: str, image: str = DEFAULT_OLLAMA_IMAGE) -> tuple[str, str, int]:
    require("docker")
    run(["docker", "network", "create", "oilsand-ai"], check=False)
    run(["docker", "volume", "create", f"{name}-models"])
    run(["docker", "rm", "-f", name], check=False)
    run(["docker", "run", "-d", "--name", name, "--network", "oilsand-ai", "-v", f"{name}-models:/root/.ollama", "-e", "OLLAMA_HOST=0.0.0.0", image])
    run(["docker", "exec", name, "ollama", "pull", model])
    return name, f"http://{name}:11434", 100


def workflow_kubernetes() -> None:
    action = choose("Kubernetes workflow", ["Deploy Olla gateway", "Deploy greenfield Ollama and register", "Register brownfield Ollama URL"])
    namespace = ask("Namespace", DEFAULT_NAMESPACE)
    if action == "Deploy Olla gateway":
        endpoints = k8s_get_endpoints(namespace)
        k8s_apply_olla(namespace, endpoints)
        return
    if action == "Deploy greenfield Ollama and register":
        model = ask("Predefined model", DEFAULT_MODEL)
        name = ask("Ollama instance name", "ollama-node-01")
        selector = ask("GPU node selector", "nvidia.com/gpu.present=true,accelerator=l40s")
        storage = ask("StorageClass", "nutanix-volume")
        endpoint = k8s_deploy_ollama(namespace, name, model, selector, storage)
    else:
        name = ask("Endpoint registration name", "brownfield-ollama-01")
        url = ask("Existing Ollama base URL", "http://10.10.10.11:11434")
        endpoint = (name, url, 100)
    endpoints = upsert_endpoint(k8s_get_endpoints(namespace), endpoint)
    k8s_apply_olla(namespace, endpoints)
    print(f"Registered {endpoint[0]} -> {endpoint[1]} with Olla in namespace {namespace}")


def workflow_docker() -> None:
    action = choose("Docker/VM workflow", ["Deploy Olla gateway", "Deploy greenfield Ollama and register", "Register brownfield Ollama URL"])
    state_dir = Path(ask("State directory", str(DEFAULT_STATE_DIR))).expanduser()
    if action == "Deploy Olla gateway":
        docker_apply_olla(state_dir, read_registry(state_dir))
        return
    if action == "Deploy greenfield Ollama and register":
        model = ask("Predefined model", DEFAULT_MODEL)
        name = ask("Ollama container name", "ollama-node-01")
        endpoint = docker_deploy_ollama(state_dir, name, model)
    else:
        name = ask("Endpoint registration name", "brownfield-ollama-01")
        url = ask("Existing Ollama base URL", "http://host.docker.internal:11434")
        endpoint = (name, url, 100)
    endpoints = upsert_endpoint(read_registry(state_dir), endpoint)
    docker_apply_olla(state_dir, endpoints)
    print(f"Registered {endpoint[0]} -> {endpoint[1]} with Docker Olla gateway")


def main() -> int:
    print("Oilsand AI Gateway TUI")
    platform = choose("Where are you deploying?", ["Kubernetes", "Docker/VM host"])
    if platform == "Kubernetes":
        workflow_kubernetes()
    else:
        workflow_docker()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
