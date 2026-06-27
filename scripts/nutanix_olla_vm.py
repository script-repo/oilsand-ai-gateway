#!/usr/bin/env python3
"""Provision a Rocky Linux VM on Nutanix AHV and install Olla or Ollama on it.

Two workflows, mirroring the operator patterns:

  pattern-a:
    1. find a Rocky Linux image on Prism Central
    2. create a VM (default 8 vCPU / 12 GiB RAM / 50 GiB disk) with a cloud-init
       script that sets the 'rocky' password
    3. SSH in and install Olla natively as a systemd service
    4. report install success and service readiness

  pattern-b:
    1-2. same VM provisioning as pattern-a
    3. SSH in and install Ollama natively, then 'ollama pull <model>' (default rnj-1)
    4. register the new worker with an existing Olla instance (defaults to the
       Olla VM created by pattern-a) by rewriting its endpoint list and restarting

The VM lifecycle uses the Prism Central v4 REST API directly so this script is
usable on its own. Credentials are read from the environment:

  PRISM_CENTRAL_URL   e.g. https://prism-central-host:9440
  PRISM_USER          Prism Central username
  PRISM_PASSWORD      Prism Central password

The guest password is supplied per run (no default is baked in):

  OILSAND_VM_PASSWORD the cloud-init password for the 'rocky' user, or pass
                      --vm-password explicitly.

SSH into the guest uses password auth (paramiko) with the cloud-init credentials.
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import re
import sys
import time
import uuid
from pathlib import Path
from typing import Any, Iterable

try:
    import requests
    from requests.auth import HTTPBasicAuth
except ImportError:  # pragma: no cover
    sys.exit("Missing dependency 'requests'. Install with: pip install -r requirements.txt")

try:
    import paramiko
except ImportError:  # pragma: no cover
    sys.exit("Missing dependency 'paramiko'. Install with: pip install -r requirements.txt")

# --- Placement (no defaults: chosen from the live Prism Central inventory) ---
# The TUI populates these from PC; CLI callers must pass --image-name,
# --cluster-name and --subnet-name (or the env vars below) per run.
DEFAULT_IMAGE_NAME = os.environ.get("OILSAND_IMAGE_NAME", "")
DEFAULT_CLUSTER_NAME = os.environ.get("OILSAND_CLUSTER_NAME", "")
DEFAULT_SUBNET_NAME = os.environ.get("OILSAND_SUBNET_NAME", "")

DEFAULT_NUM_SOCKETS = 2
DEFAULT_CORES_PER_SOCKET = 4  # 2 x 4 = 8 vCPU
DEFAULT_MEMORY_GIB = 12
DEFAULT_DISK_GIB = 50

DEFAULT_VM_USER = "rocky"
# No password is baked in; it must be supplied per run via --vm-password or the
# OILSAND_VM_PASSWORD environment variable.
DEFAULT_VM_PASSWORD = os.environ.get("OILSAND_VM_PASSWORD", "")
DEFAULT_MODEL = "rnj-1"
OLLA_PORT = 40114
OLLAMA_PORT = 11434

GIB = 1024 ** 3
REMOTE_DIR = Path(__file__).resolve().parent / "remote"
STATE_DIR = Path.home() / ".oilsand-ai-gateway"
STATE_FILE = STATE_DIR / "state.json"


def log(msg: str) -> None:
    print(f"[nutanix-olla-vm] {msg}", flush=True)


def fatal(msg: str) -> "None":
    print(f"[nutanix-olla-vm] ERROR: {msg}", file=sys.stderr, flush=True)
    raise SystemExit(1)


# --- State ------------------------------------------------------------------
def load_state() -> dict[str, Any]:
    if STATE_FILE.exists():
        try:
            return json.loads(STATE_FILE.read_text())
        except json.JSONDecodeError:
            return {}
    return {}


def save_state(state: dict[str, Any]) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    STATE_FILE.write_text(json.dumps(state, indent=2))


# --- Prism Central v4 REST client -------------------------------------------
# Nutanix PCs serve different v4 minor versions: pc.2024.x commonly tops out at
# v4.0/v4.1 while newer builds add v4.2. Paths below are written at v4.2 and the
# client falls back to older minors on a 404, caching the working minor per
# namespace so we only probe once.
API_VERSIONS = ("v4.2", "v4.1", "v4.0")
_NS_VER_RE = re.compile(r"^/([a-z]+)/(v4\.\d+)(?=/|$)")


class PrismClient:
    def __init__(self, base_url: str, user: str | None = None, password: str | None = None,
                 verify: bool = False, api_key: str | None = None):
        self.base = base_url.rstrip("/")
        self.session = requests.Session()
        self._api_ver: dict[str, str] = {}
        self.api_key = api_key
        if api_key:
            # Prism Central v4 API-key auth (e.g. the key used by the Nutanix MCP).
            self.session.headers["X-Ntnx-Api-Key"] = api_key
        else:
            self.session.auth = HTTPBasicAuth(user or "", password or "")
        self.session.verify = verify
        if not verify:
            try:
                import urllib3

                urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)
            except Exception:
                pass

    def _send(self, method: str, path: str, headers: dict, *,
              body: dict | None = None, params: dict | None = None):
        """Send a request, substituting the v4 minor version in the path and
        falling back to older minors on a 404. The working minor is cached per
        API namespace (e.g. 'vmm', 'clustermgmt') so subsequent calls skip the
        probe and a genuine not-found isn't retried needlessly."""
        m = _NS_VER_RE.match(path)
        if not m:
            return self.session.request(method, f"{self.base}/api{path}",
                                        headers=headers, json=body, params=params, timeout=60)
        ns = m.group(1)
        candidates = [self._api_ver[ns]] if ns in self._api_ver else list(API_VERSIONS)
        resp = None
        for i, ver in enumerate(candidates):
            p = path[:m.start(2)] + ver + path[m.end(2):]
            resp = self.session.request(method, f"{self.base}/api{p}",
                                        headers=headers, json=body, params=params, timeout=60)
            if resp.status_code == 404 and ns not in self._api_ver and i < len(candidates) - 1:
                continue  # this minor isn't served; try an older one
            if resp.status_code != 404:
                self._api_ver[ns] = ver  # remember the minor this PC actually serves
            break
        return resp

    def request(self, method: str, path: str, *, body: dict | None = None, params: dict | None = None,
                extra_headers: dict | None = None) -> dict:
        headers = {
            "Accept": "application/json",
            "NTNX-Request-Id": str(uuid.uuid4()),
        }
        # Only advertise a JSON body when we actually send one; some v4 action
        # endpoints (e.g. power-on) reject a request that carries an empty body.
        if body is not None:
            headers["Content-Type"] = "application/json"
        if extra_headers:
            headers.update(extra_headers)
        resp = self._send(method, path, headers, body=body, params=params)
        if resp.status_code >= 400:
            raise RuntimeError(f"{method} {path} -> HTTP {resp.status_code}: {resp.text[:800]}")
        if not resp.content:
            return {}
        return resp.json()

    def get_with_etag(self, path: str) -> tuple[dict, str | None]:
        """GET an entity and return (data, etag-from-header) for If-Match calls."""
        headers = {"Accept": "application/json", "NTNX-Request-Id": str(uuid.uuid4())}
        resp = self._send("GET", path, headers)
        if resp.status_code >= 400:
            raise RuntimeError(f"GET {path} -> HTTP {resp.status_code}: {resp.text[:800]}")
        etag = resp.headers.get("ETag") or resp.headers.get("Etag")
        return (resp.json() if resp.content else {}), etag

    # -- lookups --
    def _list_find(self, path: str, name: str, kind: str) -> dict:
        params = {"$filter": f"name eq '{name}'", "$limit": 50}
        data = self.request("GET", path, params=params).get("data") or []
        if not data:
            # Fall back to an unfiltered scan (some fields are not filterable).
            data = self.request("GET", path, params={"$limit": 100}).get("data") or []
            data = [d for d in data if d.get("name") == name]
        if not data:
            fatal(f"no {kind} named '{name}' found")
        return data[0]

    def find_image(self, name: str) -> dict:
        return self._list_find("/vmm/v4.2/content/images", name, "image")

    def find_cluster(self, name: str) -> dict:
        return self._list_find("/clustermgmt/v4.2/config/clusters", name, "cluster")

    def find_subnet(self, name: str) -> dict:
        return self._list_find("/networking/v4.2/config/subnets", name, "subnet")

    # -- vm lifecycle --
    def create_vm(self, body: dict) -> str:
        resp = self.request("POST", "/vmm/v4.2/ahv/config/vms", body=body)
        task = (resp.get("data") or {}).get("extId")
        if not task:
            raise RuntimeError(f"createVm did not return a task extId: {json.dumps(resp)[:500]}")
        return task

    def get_task(self, task_ext_id: str) -> dict:
        return self.request("GET", f"/prism/v4.2/config/tasks/{task_ext_id}").get("data") or {}

    def wait_task(self, task_ext_id: str, timeout_s: int = 600) -> dict:
        deadline = time.time() + timeout_s
        terminal_ok = {"SUCCEEDED", "COMPLETED"}
        terminal_bad = {"FAILED", "CANCELED", "CANCELLED", "ABORTED"}
        last = ""
        while time.time() < deadline:
            task = self.get_task(task_ext_id)
            status = (task.get("status") or "").upper()
            if status and status != last:
                log(f"task {task_ext_id} status: {status}")
                last = status
            if status in terminal_ok:
                return task
            if status in terminal_bad:
                raise RuntimeError(f"task {task_ext_id} ended {status}: {json.dumps(task)[:800]}")
            time.sleep(5)
        raise RuntimeError(f"task {task_ext_id} did not complete within {timeout_s}s")

    def vm_ext_id_from_task(self, task: dict, vm_name: str) -> str:
        for ent in task.get("entitiesAffected") or []:
            rel = (ent.get("rel") or "").lower()
            if "vm" in rel and ent.get("extId"):
                return ent["extId"]
        # Fall back: resolve by name.
        data = self.request(
            "GET", "/vmm/v4.2/ahv/config/vms",
            params={"$filter": f"name eq '{vm_name}'", "$limit": 10},
        ).get("data") or []
        if data:
            return data[0]["extId"]
        raise RuntimeError("could not determine created VM extId")

    def get_vm(self, vm_ext_id: str) -> dict:
        return self.request("GET", f"/vmm/v4.2/ahv/config/vms/{vm_ext_id}").get("data") or {}

    def power_on(self, vm_ext_id: str) -> None:
        # Power actions require an If-Match header carrying the entity ETag from a prior GET,
        # and must NOT carry a request body.
        _vm, etag = self.get_with_etag(f"/vmm/v4.2/ahv/config/vms/{vm_ext_id}")
        if not etag:
            raise RuntimeError("could not read VM ETag required to power on")
        task = self.request(
            "POST", f"/vmm/v4.2/ahv/config/vms/{vm_ext_id}/$actions/power-on",
            body=None, extra_headers={"If-Match": etag},
        ).get("data") or {}
        if task.get("extId"):
            self.wait_task(task["extId"], timeout_s=180)

    def find_vm_by_name(self, name: str) -> dict | None:
        data = self.request(
            "GET", "/vmm/v4.2/ahv/config/vms",
            params={"$filter": f"name eq '{name}'", "$limit": 50},
        ).get("data") or []
        if not data:
            data = self.request("GET", "/vmm/v4.2/ahv/config/vms", params={"$limit": 100}).get("data") or []
            data = [d for d in data if d.get("name") == name]
        return data[0] if data else None

    def list_all_vm_names(self) -> list[str]:
        """Return every VM name in Prism Central (paged)."""
        names: list[str] = []
        page = 0
        while True:
            data = self.request(
                "GET", "/vmm/v4.2/ahv/config/vms",
                params={"$select": "name", "$limit": 100, "$page": page},
            ).get("data") or []
            names.extend(str(d.get("name", "")) for d in data)
            if len(data) < 100:
                break
            page += 1
        return names

    def delete_vm(self, vm_ext_id: str) -> None:
        path = f"/vmm/v4.2/ahv/config/vms/{vm_ext_id}"
        try:
            resp = self.request("DELETE", path)
        except RuntimeError as exc:
            # Some PC builds require an If-Match ETag on delete; retry with one.
            if "etag" in str(exc).lower() or "412" in str(exc):
                _vm, etag = self.get_with_etag(path)
                resp = self.request("DELETE", path, extra_headers={"If-Match": etag} if etag else None)
            else:
                raise
        task = (resp or {}).get("data") or {}
        if task.get("extId"):
            self.wait_task(task["extId"], timeout_s=180)

    def wait_for_ip(self, vm_ext_id: str, prefer_prefix: str | None = None, timeout_s: int = 600) -> str:
        deadline = time.time() + timeout_s
        while time.time() < deadline:
            vm = self.get_vm(vm_ext_id)
            ips = _collect_ips(vm)
            if ips:
                if prefer_prefix:
                    for ip in ips:
                        if ip.startswith(prefer_prefix):
                            return ip
                return ips[0]
            time.sleep(10)
        raise RuntimeError(f"VM {vm_ext_id} did not report an IP within {timeout_s}s")


def _collect_ips(obj: Any) -> list[str]:
    """Walk the VM object and collect learned/assigned IPv4 addresses."""
    found: list[str] = []

    def walk(node: Any, key_hint: str = "") -> None:
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, k)
        elif isinstance(node, list):
            for item in node:
                walk(item, key_hint)
        elif isinstance(node, str):
            if key_hint == "value" and node.count(".") == 3:
                parts = node.split(".")
                if all(p.isdigit() and 0 <= int(p) <= 255 for p in parts):
                    found.append(node)

    for nic in obj.get("nics") or []:
        info = nic.get("networkInfo") or nic.get("nicNetworkInfo") or {}
        ipv4_info = info.get("ipv4Info") or {}
        for addr in ipv4_info.get("learnedIpAddresses") or []:
            if addr.get("value"):
                found.append(addr["value"])
        ipv4_cfg = info.get("ipv4Config") or {}
        addr = ipv4_cfg.get("ipAddress") or {}
        if addr.get("value"):
            found.append(addr["value"])
    if not found:
        walk(obj.get("nics") or [])
    # De-dup, drop link-local / loopback.
    out: list[str] = []
    for ip in found:
        if ip in out or ip.startswith(("127.", "169.254.", "0.")):
            continue
        out.append(ip)
    return out


def vm_summary(vm: dict) -> dict:
    """Condense a v4 VM object into the fields the TUI/operators care about."""
    ips = _collect_ips(vm)
    disks = vm.get("disks") or []
    disk_bytes = 0
    for d in disks:
        disk_bytes = max(disk_bytes, int((d.get("backingInfo") or {}).get("diskSizeBytes", 0) or 0))
    sockets = int(vm.get("numSockets", 0) or 0)
    cores = int(vm.get("numCoresPerSocket", 0) or 0)
    return {
        "name": vm.get("name"),
        "ext_id": vm.get("extId"),
        "power_state": vm.get("powerState"),
        "ip": ips[0] if ips else None,
        "ips": ips,
        "vcpu": sockets * cores,
        "num_sockets": sockets,
        "cores_per_socket": cores,
        "memory_gib": round(int(vm.get("memorySizeBytes", 0) or 0) / GIB, 1),
        "disk_gib": round(disk_bytes / GIB, 1),
        "cluster_ext_id": (vm.get("cluster") or {}).get("extId"),
    }


# --- cloud-init + VM body ---------------------------------------------------
def render_cloud_init(hostname: str, password: str) -> str:
    tmpl = (REMOTE_DIR / "cloud-init.rocky.yaml.tmpl").read_text()
    return tmpl.replace("{{HOSTNAME}}", hostname).replace("{{PASSWORD}}", password)


def build_vm_body(
    *,
    name: str,
    cluster_ext_id: str,
    image_ext_id: str,
    subnet_ext_id: str,
    num_sockets: int,
    cores_per_socket: int,
    memory_gib: int,
    disk_gib: int,
    cloud_init_userdata: str,
    image_storage_cluster_ext_id: str | None = None,
) -> dict:
    userdata_b64 = base64.b64encode(cloud_init_userdata.encode()).decode()
    image_ref: dict[str, Any] = {
        "$objectType": "vmm.v4.ahv.config.ImageReference",
        "imageExtId": image_ext_id,
    }
    if image_storage_cluster_ext_id:
        image_ref["storageCluster"] = {
            "$objectType": "vmm.v4.ahv.config.ClusterReference",
            "extId": image_storage_cluster_ext_id,
        }
    return {
        "$objectType": "vmm.v4.ahv.config.Vm",
        "name": name,
        "description": "Provisioned by oilsand-ai-gateway nutanix_olla_vm.py",
        "numSockets": num_sockets,
        "numCoresPerSocket": cores_per_socket,
        "numThreadsPerCore": 1,
        "memorySizeBytes": memory_gib * GIB,
        "cluster": {
            "$objectType": "vmm.v4.ahv.config.ClusterReference",
            "extId": cluster_ext_id,
        },
        "disks": [
            {
                "$objectType": "vmm.v4.ahv.config.Disk",
                "diskAddress": {
                    "$objectType": "vmm.v4.ahv.config.DiskAddress",
                    "busType": "SCSI",
                    "index": 0,
                },
                "backingInfo": {
                    "$objectType": "vmm.v4.ahv.config.VmDisk",
                    "diskSizeBytes": disk_gib * GIB,
                    "dataSource": {
                        "$objectType": "vmm.v4.ahv.config.DataSource",
                        "reference": image_ref,
                    },
                },
            }
        ],
        "nics": [
            {
                "$objectType": "vmm.v4.ahv.config.Nic",
                "networkInfo": {
                    "$objectType": "vmm.v4.ahv.config.NicNetworkInfo",
                    "nicType": "NORMAL_NIC",
                    "subnet": {
                        "$objectType": "vmm.v4.ahv.config.SubnetReference",
                        "extId": subnet_ext_id,
                    },
                    "ipv4Config": {
                        "$objectType": "vmm.v4.ahv.config.Ipv4Config",
                        "shouldAssignIp": True,
                    },
                },
            }
        ],
        "guestCustomization": {
            "$objectType": "vmm.v4.ahv.config.GuestCustomizationParams",
            "config": {
                "$objectType": "vmm.v4.ahv.config.CloudInit",
                "cloudInitScript": {
                    "$objectType": "vmm.v4.ahv.config.Userdata",
                    "value": userdata_b64,
                },
            },
        },
    }


# --- SSH --------------------------------------------------------------------
class Ssh:
    def __init__(self, host: str, user: str, password: str):
        self.host = host
        self.user = user
        self.password = password
        self.client: paramiko.SSHClient | None = None

    def connect(self, timeout_s: int = 600) -> None:
        deadline = time.time() + timeout_s
        last_err: Exception | None = None
        log(f"connecting via SSH to {self.user}@{self.host} (waiting for guest to boot)")
        while time.time() < deadline:
            client = paramiko.SSHClient()
            client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
            try:
                client.connect(
                    self.host,
                    username=self.user,
                    password=self.password,
                    look_for_keys=False,
                    allow_agent=False,
                    timeout=15,
                    banner_timeout=20,
                    auth_timeout=20,
                )
                self.client = client
                log("SSH connection established")
                return
            except Exception as exc:  # noqa: BLE001
                last_err = exc
                client.close()
                time.sleep(10)
        raise RuntimeError(f"SSH to {self.host} failed within {timeout_s}s: {last_err}")

    def run(self, cmd: str, *, stream: bool = True) -> tuple[int, str, str]:
        assert self.client is not None
        stdin, stdout, stderr = self.client.exec_command(cmd, get_pty=False)
        out_chunks: list[str] = []
        err_chunks: list[str] = []
        for line in iter(stdout.readline, ""):
            out_chunks.append(line)
            if stream:
                print(f"  | {line.rstrip()}", flush=True)
        rc = stdout.channel.recv_exit_status()
        err = stderr.read().decode(errors="replace")
        if err:
            err_chunks.append(err)
            if stream:
                for line in err.splitlines():
                    print(f"  ! {line}", flush=True)
        return rc, "".join(out_chunks), "".join(err_chunks)

    def put(self, local: Path, remote: str) -> None:
        assert self.client is not None
        sftp = self.client.open_sftp()
        try:
            sftp.put(str(local), remote)
        finally:
            sftp.close()

    def put_text(self, text: str, remote: str) -> None:
        assert self.client is not None
        sftp = self.client.open_sftp()
        try:
            with sftp.open(remote, "w") as fh:
                fh.write(text)
        finally:
            sftp.close()

    def wait_cloud_init(self) -> None:
        log("waiting for cloud-init to finish")
        self.run("cloud-init status --wait || true", stream=False)

    def close(self) -> None:
        if self.client:
            self.client.close()


# --- Olla endpoint registry rendering --------------------------------------
def render_olla_config(endpoints: Iterable[dict], port: int = OLLA_PORT) -> str:
    lines = [
        "server:",
        '  host: "0.0.0.0"',
        f"  port: {port}",
        "  request_logging: true",
        "",
        "proxy:",
        '  engine: "sherpa"',
        '  load_balancer: "priority"',
        "",
        "discovery:",
        '  type: "static"',
        "  model_discovery:",
        "    enabled: true",
        "    interval: 5m",
        "  static:",
        "    endpoints:",
    ]
    endpoints = list(endpoints)
    if not endpoints:
        lines.append("      []")
    else:
        for ep in endpoints:
            lines.extend([
                f'      - url: "{ep["url"]}"',
                f'        name: "{ep["name"]}"',
                f'        type: "{ep.get("type", "ollama")}"',
                f'        priority: {ep.get("priority", 100)}',
                "        check_interval: 10s",
                "        check_timeout: 3s",
            ])
    lines.extend(["", "logging:", '  level: "info"', '  format: "json"', ""])
    return "\n".join(lines)


def upsert_endpoint(endpoints: list[dict], endpoint: dict) -> list[dict]:
    return [e for e in endpoints if e.get("name") != endpoint["name"]] + [endpoint]


WORKER_PREFIX = "ollama-worker-"
GATEWAY_PREFIX = "olla-gateway-"


def next_indexed_name(pc: PrismClient, prefix: str, width: int = 2) -> str:
    """Return the next free '<prefix>NN' name.

    Scans existing VM names for ones matching '<prefix><digits>' and returns the
    prefix with (max index + 1), zero-padded to `width`. If none exist, restarts
    at 01. Example: with workers 01 and 03 present -> 'ollama-worker-04'; with
    none present -> 'ollama-worker-01'.
    """
    pattern = re.compile(rf"^{re.escape(prefix)}(\d+)$")
    highest = 0
    try:
        names = pc.list_all_vm_names()
    except Exception as exc:  # noqa: BLE001
        log(f"warning: could not list VMs to compute next name ({exc}); defaulting to 01")
        names = []
    for name in names:
        match = pattern.match(name or "")
        if match:
            highest = max(highest, int(match.group(1)))
    return f"{prefix}{highest + 1:0{width}d}"


# --- shared provisioning ----------------------------------------------------
def provision_vm(pc: PrismClient, args: argparse.Namespace, vm_name: str) -> dict:
    image = pc.find_image(args.image_name)
    cluster = pc.find_cluster(args.cluster_name)
    subnet = pc.find_subnet(args.subnet_name)

    image_ext_id = image["extId"]
    cluster_ext_id = cluster["extId"]
    subnet_ext_id = subnet["extId"]
    image_clusters = image.get("clusterLocationExtIds") or []
    storage_cluster = cluster_ext_id if cluster_ext_id in image_clusters else (image_clusters[0] if image_clusters else None)

    log(f"image '{args.image_name}' -> {image_ext_id}")
    log(f"cluster '{args.cluster_name}' -> {cluster_ext_id}")
    log(f"subnet '{args.subnet_name}' -> {subnet_ext_id}")

    cloud_init = render_cloud_init(vm_name, args.vm_password)
    body = build_vm_body(
        name=vm_name,
        cluster_ext_id=cluster_ext_id,
        image_ext_id=image_ext_id,
        subnet_ext_id=subnet_ext_id,
        num_sockets=args.num_sockets,
        cores_per_socket=args.cores_per_socket,
        memory_gib=args.memory_gib,
        disk_gib=args.disk_gib,
        cloud_init_userdata=cloud_init,
        image_storage_cluster_ext_id=storage_cluster,
    )

    if args.dry_run:
        log("dry-run: VM create body and cloud-init below; nothing will be created")
        print(json.dumps(body, indent=2))
        print("\n--- cloud-init ---\n" + cloud_init)
        raise SystemExit(0)

    vcpus = args.num_sockets * args.cores_per_socket
    log(f"creating VM '{vm_name}' ({vcpus} vCPU / {args.memory_gib} GiB / {args.disk_gib} GiB)")
    task_ext_id = pc.create_vm(body)
    task = pc.wait_task(task_ext_id)
    vm_ext_id = pc.vm_ext_id_from_task(task, vm_name)
    log(f"VM created: {vm_ext_id}")

    pc.power_on(vm_ext_id)
    prefix = args.ip_prefix or None
    ip = pc.wait_for_ip(vm_ext_id, prefer_prefix=prefix)
    log(f"VM IP: {ip}")
    return {"vm_ext_id": vm_ext_id, "ip": ip, "name": vm_name}


def ssh_install(ip: str, args: argparse.Namespace, script_name: str, remote_args: str = "") -> Ssh:
    ssh = Ssh(ip, args.vm_user, args.vm_password)
    ssh.connect()
    ssh.wait_cloud_init()
    local = REMOTE_DIR / script_name
    remote = f"/tmp/{script_name}"
    log(f"uploading {script_name}")
    # Normalize to LF so the guest's bash doesn't choke on Windows CRLF endings.
    script_text = local.read_text(encoding="utf-8").replace("\r\n", "\n").replace("\r", "\n")
    ssh.put_text(script_text, remote)
    cmd = f"chmod +x {remote} && sudo bash {remote} {remote_args}".strip()
    log(f"running {script_name} on the guest")
    rc, _out, _err = ssh.run(cmd)
    if rc != 0:
        ssh.close()
        fatal(f"{script_name} failed on the guest (exit {rc})")
    return ssh


# --- pattern A --------------------------------------------------------------
def pattern_a(pc: PrismClient, args: argparse.Namespace) -> None:
    vm_name = args.vm_name or next_indexed_name(pc, GATEWAY_PREFIX)
    log(f"gateway name: {vm_name}")
    result = provision_vm(pc, args, vm_name)
    finish_pattern_a(result["ip"], args, vm_name, vm_ext_id=result["vm_ext_id"])


def finish_pattern_a(ip: str, args: argparse.Namespace, vm_name: str, vm_ext_id: str | None = None) -> None:
    ssh = ssh_install(ip, args, "install-olla.sh")
    # Readiness checks.
    active_rc, active_out, _ = ssh.run("systemctl is-active olla", stream=False)
    models_rc, models_out, _ = ssh.run(
        f"curl -fsS http://127.0.0.1:{OLLA_PORT}/v1/models || "
        f"curl -fsS http://127.0.0.1:{OLLA_PORT}/internal/health || "
        f"curl -fsS http://127.0.0.1:{OLLA_PORT}/",
        stream=False,
    )
    ssh.close()

    olla_url = f"http://{ip}:{OLLA_PORT}"
    state = load_state()
    state["pattern_a"] = {
        "vm_ext_id": vm_ext_id,
        "ip": ip,
        "ssh_host": ip,
        "olla_url": olla_url,
    }
    save_state(state)

    report = {
        "pattern": "A",
        "vm_name": vm_name,
        "vm_ext_id": vm_ext_id,
        "ip": ip,
        "olla_url": olla_url,
        "olla_service_active": active_out.strip() == "active",
        "olla_http_ready": models_rc == 0,
    }
    print("\n=== Pattern A report ===")
    print(json.dumps(report, indent=2))
    if not report["olla_service_active"]:
        fatal("Olla service is not active")
    log("Pattern A complete: Olla is installed and serving.")


# --- pattern B --------------------------------------------------------------
def _resolve_olla_target(args: argparse.Namespace) -> tuple[str, str]:
    state = load_state()
    olla_url = args.olla_url
    olla_ssh_host = args.olla_ssh_host
    if not olla_url:
        pa = state.get("pattern_a") or {}
        olla_url = pa.get("olla_url")
        olla_ssh_host = olla_ssh_host or pa.get("ssh_host")
    if not olla_url:
        fatal("no Olla instance to register with: run pattern-a first or pass --olla-url")
    if not olla_ssh_host:
        olla_ssh_host = olla_url.split("//", 1)[-1].split(":", 1)[0].split("/", 1)[0]
    return olla_url, olla_ssh_host


def pattern_b(pc: PrismClient, args: argparse.Namespace) -> None:
    vm_name = args.vm_name or next_indexed_name(pc, WORKER_PREFIX)
    log(f"worker name: {vm_name}")
    _resolve_olla_target(args)  # fail fast before provisioning if unresolved
    result = provision_vm(pc, args, vm_name)
    finish_pattern_b(result["ip"], args, vm_name, vm_ext_id=result["vm_ext_id"])


def finish_pattern_b(ip: str, args: argparse.Namespace, vm_name: str, vm_ext_id: str | None = None) -> None:
    olla_url, olla_ssh_host = _resolve_olla_target(args)

    ssh = ssh_install(ip, args, "install-ollama.sh", remote_args=args.model)
    # Confirm model is present.
    tags_rc, tags_out, _ = ssh.run(f"curl -fsS http://127.0.0.1:{OLLAMA_PORT}/api/tags", stream=False)
    active_rc, active_out, _ = ssh.run("systemctl is-active ollama", stream=False)
    ssh.close()

    model_present = args.model in tags_out

    # Register with the existing Olla instance.
    worker_url = f"http://{ip}:{OLLAMA_PORT}"
    endpoint = {"name": vm_name, "url": worker_url, "type": "ollama", "priority": 100}
    registered = register_with_olla(olla_ssh_host, args, endpoint, olla_url)

    report = {
        "pattern": "B",
        "vm_name": vm_name,
        "vm_ext_id": vm_ext_id,
        "ip": ip,
        "ollama_url": worker_url,
        "ollama_service_active": active_out.strip() == "active",
        "model": args.model,
        "model_pulled": model_present,
        "registered_with_olla": registered,
        "olla_url": olla_url,
    }
    print("\n=== Pattern B report ===")
    print(json.dumps(report, indent=2))
    if not report["ollama_service_active"]:
        fatal("Ollama service is not active")
    if not model_present:
        fatal(f"model '{args.model}' not present after pull")
    log("Pattern B complete: Ollama worker installed and registered with Olla.")


def register_with_olla(olla_ssh_host: str, args: argparse.Namespace, endpoint: dict, olla_url: str) -> bool:
    log(f"registering {endpoint['url']} with Olla at {olla_url} (via SSH to {olla_ssh_host})")
    state = load_state()
    registry = state.setdefault("olla_endpoints", {})
    current = registry.get(olla_url, [])
    current = upsert_endpoint(current, endpoint)
    registry[olla_url] = current
    save_state(state)

    config = render_olla_config(current)
    ssh = Ssh(olla_ssh_host, args.vm_user, args.vm_password)
    ssh.connect(timeout_s=120)
    try:
        ssh.put_text(config, "/tmp/olla.yaml")
        rc, _out, _err = ssh.run(
            "sudo install -o olla -g olla -m 0644 /tmp/olla.yaml /etc/olla/olla.yaml "
            "&& sudo systemctl restart olla",
        )
        if rc != 0:
            log("warning: failed to update/restart Olla on the gateway VM")
            return False
        # Give Olla a moment, then confirm it is healthy and the endpoint is registered.
        # Olla v0.0.x exposes /internal/health and /internal/status (no /v1/models route).
        time.sleep(5)
        check_rc, _o, _e = ssh.run(
            f"curl -fsS http://127.0.0.1:{OLLA_PORT}/internal/health "
            f"|| curl -fsS http://127.0.0.1:{OLLA_PORT}/internal/status",
            stream=False,
        )
        return check_rc == 0
    finally:
        ssh.close()


# --- CLI --------------------------------------------------------------------
def add_common_args(p: argparse.ArgumentParser) -> None:
    p.add_argument("--prism-url", default=os.environ.get("PRISM_CENTRAL_URL"),
                   help="Prism Central base URL (or env PRISM_CENTRAL_URL)")
    p.add_argument("--prism-user", default=os.environ.get("PRISM_USER"),
                   help="Prism Central username (or env PRISM_USER)")
    p.add_argument("--prism-password", default=os.environ.get("PRISM_PASSWORD"),
                   help="Prism Central password (or env PRISM_PASSWORD)")
    p.add_argument("--prism-api-key", default=os.environ.get("PRISM_API_KEY"),
                   help="Prism Central v4 API key (or env PRISM_API_KEY); alternative to user/password")
    p.add_argument("--verify-tls", action="store_true", help="Verify Prism Central TLS certificate")
    p.add_argument("--image-name", default=DEFAULT_IMAGE_NAME)
    p.add_argument("--cluster-name", default=DEFAULT_CLUSTER_NAME)
    p.add_argument("--subnet-name", default=DEFAULT_SUBNET_NAME)
    p.add_argument("--vm-name", default=None)
    p.add_argument("--num-sockets", type=int, default=DEFAULT_NUM_SOCKETS)
    p.add_argument("--cores-per-socket", type=int, default=DEFAULT_CORES_PER_SOCKET)
    p.add_argument("--memory-gib", type=int, default=DEFAULT_MEMORY_GIB)
    p.add_argument("--disk-gib", type=int, default=DEFAULT_DISK_GIB)
    p.add_argument("--vm-user", default=DEFAULT_VM_USER)
    p.add_argument("--vm-password", default=DEFAULT_VM_PASSWORD)
    p.add_argument("--ip-prefix", default=os.environ.get("OILSAND_IP_PREFIX", ""),
                   help="Prefer a learned IP with this subnet prefix (default: take the first learned IP; "
                        "or set OILSAND_IP_PREFIX)")
    p.add_argument("--dry-run", action="store_true",
                   help="Print the VM create body and cloud-init, then exit")


def main() -> int:
    # Remote scripts can emit UTF-8 (e.g. arrows); avoid crashing on Windows cp1252 consoles.
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")  # type: ignore[attr-defined]
        except Exception:
            pass

    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    pa = sub.add_parser("pattern-a", help="Provision a Rocky VM and install Olla")
    add_common_args(pa)

    pb = sub.add_parser("pattern-b", help="Provision a Rocky VM, install Ollama + model, register with Olla")
    add_common_args(pb)
    pb.add_argument("--model", default=DEFAULT_MODEL, help="Ollama model to pull (default: rnj-1)")
    pb.add_argument("--olla-url", default=None,
                    help="Existing Olla base URL to register with (default: pattern-a VM from state)")
    pb.add_argument("--olla-ssh-host", default=None,
                    help="SSH host of the Olla gateway VM (default: derived from --olla-url / state)")

    # install-only variants: skip provisioning and run the SSH install against an
    # already-running VM (e.g. one created out-of-band via the Nutanix MCP).
    ia = sub.add_parser("install-a", help="Install Olla on an existing VM (no provisioning)")
    ia.add_argument("--host", required=True, help="IP/hostname of the target VM")
    ia.add_argument("--vm-name", default="olla-gateway-01")
    ia.add_argument("--vm-user", default=DEFAULT_VM_USER)
    ia.add_argument("--vm-password", default=DEFAULT_VM_PASSWORD)

    ib = sub.add_parser("install-b", help="Install Ollama + model on an existing VM and register with Olla")
    ib.add_argument("--host", required=True, help="IP/hostname of the target VM")
    ib.add_argument("--vm-name", default="ollama-worker-01")
    ib.add_argument("--vm-user", default=DEFAULT_VM_USER)
    ib.add_argument("--vm-password", default=DEFAULT_VM_PASSWORD)
    ib.add_argument("--model", default=DEFAULT_MODEL)
    ib.add_argument("--olla-url", default=None)
    ib.add_argument("--olla-ssh-host", default=None)

    dl = sub.add_parser("delete", help="Delete a VM by name (gateway or worker) in Prism Central")
    add_common_args(dl)
    dl.add_argument("--name", required=True, help="VM name to delete")

    sh = sub.add_parser("show", help="Print VM info (JSON) for one or more VM names")
    add_common_args(sh)
    sh.add_argument("--name", action="append", default=[], help="VM name (repeatable)")
    sh.add_argument("--name-prefix", default=None, help="Match all VMs whose name starts with this")

    nn = sub.add_parser("next-name", help="Print the next available indexed VM name for a role")
    add_common_args(nn)
    nn.add_argument("--role", choices=["worker", "gateway"], default="worker")
    nn.add_argument("--prefix", default=None, help="Override the name prefix (e.g. ollama-worker-)")

    args = parser.parse_args()

    # Commands that SSH into the guest need the cloud-init password. No default
    # is baked in, so require it explicitly (flag or OILSAND_VM_PASSWORD).
    if args.command in ("pattern-a", "pattern-b", "install-a", "install-b") and not getattr(args, "vm_password", ""):
        fatal("VM password required: pass --vm-password or set OILSAND_VM_PASSWORD "
              "(no default password is shipped)")

    # Provisioning needs explicit placement; no lab defaults are baked in.
    if args.command in ("pattern-a", "pattern-b"):
        prov_missing = [name for name, val in (
            ("--image-name", getattr(args, "image_name", "")),
            ("--cluster-name", getattr(args, "cluster_name", "")),
            ("--subnet-name", getattr(args, "subnet_name", "")),
        ) if not val]
        if prov_missing:
            fatal("missing required placement option(s): " + ", ".join(prov_missing) +
                  " — pick them in the TUI, or pass them explicitly (also settable via "
                  "OILSAND_IMAGE_NAME / OILSAND_CLUSTER_NAME / OILSAND_SUBNET_NAME)")

    if args.command in ("install-a", "install-b"):
        if args.command == "install-a":
            finish_pattern_a(args.host, args, args.vm_name)
        else:
            finish_pattern_b(args.host, args, args.vm_name)
        return 0

    if not args.prism_url or not (args.prism_api_key or (args.prism_user and args.prism_password)):
        fatal("Prism Central credentials required: set PRISM_CENTRAL_URL and either PRISM_API_KEY "
              "or PRISM_USER + PRISM_PASSWORD")

    pc = PrismClient(args.prism_url, args.prism_user, args.prism_password,
                     verify=args.verify_tls, api_key=args.prism_api_key)

    if args.command == "pattern-a":
        pattern_a(pc, args)
    elif args.command == "pattern-b":
        pattern_b(pc, args)
    elif args.command == "delete":
        vm = pc.find_vm_by_name(args.name)
        if not vm:
            fatal(f"no VM named '{args.name}' found")
        summary = vm_summary(vm)
        log(f"deleting VM '{args.name}' ({summary['ext_id']}, power={summary['power_state']}, ip={summary['ip']})")
        pc.delete_vm(summary["ext_id"])
        log(f"VM '{args.name}' deleted")
        print(json.dumps({"deleted": True, **summary}, indent=2))
    elif args.command == "next-name":
        prefix = args.prefix or (WORKER_PREFIX if args.role == "worker" else GATEWAY_PREFIX)
        name = next_indexed_name(pc, prefix)
        print(json.dumps({"role": args.role, "prefix": prefix, "name": name}))
    elif args.command == "show":
        names = list(args.name)
        results = []
        if args.name_prefix:
            allvms = pc.request("GET", "/vmm/v4.2/ahv/config/vms", params={"$limit": 100}).get("data") or []
            results = [vm_summary(v) for v in allvms if str(v.get("name", "")).startswith(args.name_prefix)]
        for n in names:
            vm = pc.find_vm_by_name(n)
            results.append(vm_summary(vm) if vm else {"name": n, "found": False})
        print(json.dumps(results, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
