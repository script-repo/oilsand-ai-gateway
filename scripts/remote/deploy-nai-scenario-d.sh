#!/usr/bin/env bash
# --- CRLF self-heal: if this file was saved/transferred with Windows (CRLF) line -----
# --- endings, transparently re-exec a cleaned copy. You should never need dos2unix. --
[ -z "${NAI_DECRLF:-}" ] && grep -q $'\r' "$0" 2>/dev/null && NAI_DECRLF=1 exec bash <(tr -d '\r' < "$0") "$@"
#
# deploy-nai-scenario-d.sh
# =========================
# End-to-end, prompt-driven deployment of Nutanix Enterprise AI (NAI) 2.7 using
# "Scenario D" (single Ubuntu VM on AHV + MicroK8s, no NKP / no NUS, control-plane only).
#
# The script runs from ANY bash host (Linux / macOS / WSL / Git-Bash) that can reach:
#   1. Prism Central over HTTPS (to create + power on the VM via the v4 REST API), and
#   2. the resulting VM over SSH (to install MicroK8s + NAI in-guest).
#
# It is generic: nothing about the target AHV environment is hard-coded. Every required
# value is prompted up front (with sensible defaults where possible), echoed back for
# confirmation, and only then executed.
#
# Required local tools: bash, curl, jq, ssh, scp, sshpass, base64, awk, sed.
#
# NOTE on line endings: no dos2unix required - the guard above self-heals CRLF. If the
# #! line itself ever gets CRLF-corrupted (e.g. by a Windows editor's "Save As"), the OS
# can fail to even locate bash before that guard runs; if `./deploy-nai-scenario-d.sh`
# errors immediately with something like "bad interpreter", run `bash deploy-nai-scenario-d.sh`
# instead - that bypasses the OS's #! lookup entirely and the guard still cleans it up.
#
# Lessons baked in (see the companion NAI-2.7-ScenarioD-Deployment.md for the "why"):
#   * VM is created at its final size WITH memory overcommit up front (no resize/power cycle).
#   * Docker Hub registry auth is written into containerd BEFORE any NAI image is pulled,
#     so kubelet never falls back to an anonymous (rate-limited) pull of private images.
#   * ClickHouse CPU request is right-sized through Helm values (not a post-hoc CR patch),
#     so it survives future `helm upgrade`s.
#   * Helm installs use generous --timeout values and run idempotently.
#   * The requested MetalLB range is probed (ping + TCP) on the guest before use; MetalLB
#     itself does not check for collisions and will happily ARP-announce an IP someone
#     else already owns.
#
# SAFETY: this script CREATES and POWERS ON a VM and installs software on it. Review the
# confirmation summary before typing "yes".
#
set -euo pipefail

DEPLOY_START_EPOCH=$(date +%s)
DEPLOY_START_HUMAN=$(date)

nai_report_elapsed(){
  local rc=$? end secs mins remsecs
  end=$(date +%s)
  secs=$(( end - DEPLOY_START_EPOCH ))
  mins=$(( secs / 60 )); remsecs=$(( secs % 60 ))
  echo
  if [ "$rc" -eq 0 ]; then
    printf '\033[1;32m[+]\033[0m Finished: %s  (total elapsed: %dm %ds)\n' "$(date)" "$mins" "$remsecs"
  else
    printf '\033[1;31m[x]\033[0m Exited with code %d: %s  (total elapsed: %dm %ds)\n' "$rc" "$(date)" "$mins" "$remsecs" >&2
  fi
}
trap nai_report_elapsed EXIT

# --------------------------------------------------------------------------------------
# 0. helpers
# --------------------------------------------------------------------------------------
c_info(){ printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
c_ok(){   printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
c_warn(){ printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
c_err(){  printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; }
die(){ c_err "$*"; exit 1; }

need(){ command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }
for t in curl jq ssh scp sshpass base64 awk sed; do need "$t"; done
c_info "Started: ${DEPLOY_START_HUMAN}"

# ask VAR "Prompt" "default"
ask(){ local __v="$1" __p="$2" __d="${3:-}" __in
  if [ -n "$__d" ]; then read -r -p "$__p [$__d]: " __in || true; __in="${__in:-$__d}";
  else read -r -p "$__p: " __in || true; fi
  printf -v "$__v" '%s' "$__in"; }
# ask_secret VAR "Prompt"
ask_secret(){ local __v="$1" __p="$2" __in; read -r -s -p "$__p: " __in || true; echo; printf -v "$__v" '%s' "$__in"; }
# ask_yn VAR "Prompt" "default(y/n)"
ask_yn(){ local __v="$1" __p="$2" __d="${3:-y}" __in
  read -r -p "$__p (y/n) [$__d]: " __in || true; __in="${__in:-$__d}"
  case "$__in" in [Yy]*) printf -v "$__v" 'yes';; *) printf -v "$__v" 'no';; esac; }

# --------------------------------------------------------------------------------------
# 1. gather variables
# --------------------------------------------------------------------------------------
echo
c_info "NAI 2.7 - Scenario D deployer. Answer the prompts below (Enter accepts the default)."
echo

# --- Prism Central connection ---
ask PC_HOST      "Prism Central host/IP"
ask PC_PORT      "Prism Central port" "9440"
ask AUTH_MODE    "PC auth mode: 'apikey' or 'basic'" "apikey"
if [ "$AUTH_MODE" = "apikey" ]; then
  ask_secret PC_API_KEY "PC API key (X-Ntnx-Api-Key)"
else
  ask PC_USER "PC username"
  ask_secret PC_PASS "PC password"
fi
ask_yn PC_INSECURE "Skip TLS verification to PC?" "y"

# --- Target placement (resolved from names to UUIDs) ---
ask CLUSTER_NAME "AHV cluster name (as shown in PC)"
ask SUBNET_NAME  "Subnet name for the VM NIC (IPAM/DHCP recommended)"
ask IMAGE_NAME   "Ubuntu 24.04 image name (substring match, case-insensitive)"

# --- VM sizing ---
ask VM_NAME       "VM name" "DB-MK8S-NAI"
ask VCPU          "vCPUs (sockets)" "12"
ask RAM_GB        "RAM in GiB" "48"
ask DISK_GB       "Root disk in GiB" "100"
ask_yn MEM_OVERCOMMIT "Enable memory overcommit? (recommended if host RAM is tight)" "y"

# --- Guest OS / cloud-init ---
ask GUEST_HOSTNAME "Guest hostname" "$VM_NAME"
ask GUEST_DOMAIN   "Guest DNS domain (for FQDN)" "ntnxlab.local"
ask GUEST_USER     "Guest login user to create" "nutanix"
ask_secret GUEST_PASSWORD "Guest login password for $GUEST_USER"

# --- Container registry (Docker Hub creds with access to nutanix/nai-* images) ---
ask DOCKER_USERNAME "Docker Hub username (NAI entitlement)"
ask_secret DOCKER_PAT "Docker Hub password / PAT"
ask DOCKER_EMAIL    "Docker Hub email"
ask REGISTRY_SERVER "Registry server" "https://index.docker.io/v1/"

# --- NAI + MicroK8s knobs ---
ask NAI_VERSION      "NAI chart version" "2.7.0"
ask MICROK8S_CHANNEL "MicroK8s snap channel" "1.32/stable"
ask METALLB_RANGE    "MetalLB address range on the VM subnet (outside the DHCP pool)"
ask STORAGE_CLASS    "Kubernetes StorageClass" "microk8s-hostpath"
ask CH_CPU_REQUEST   "ClickHouse CPU request (cores) - keep small on tight nodes" "1"
ask OBS_NAMESPACE    "Observability namespace (Prometheus stack)" "observability"

# --- versions of AI-gateway prerequisites (rarely changed) ---
ENVOY_GW_VERSION="${ENVOY_GW_VERSION:-v1.7.0}"
KSERVE_VERSION="${KSERVE_VERSION:-v0.15.0}"
OTEL_OP_VERSION="${OTEL_OP_VERSION:-0.102.0}"
# Set METALLB_FORCE=yes in the environment beforehand to skip/override the MetalLB
# range collision check (the guest installer probes the range with ping+TCP first).
METALLB_FORCE="${METALLB_FORCE:-no}"

# convert sizes
MEM_BYTES=$(( RAM_GB  * 1073741824 ))
DISK_BYTES=$(( DISK_GB * 1073741824 ))
[ "$MEM_OVERCOMMIT" = "yes" ] && OVERCOMMIT_JSON=true || OVERCOMMIT_JSON=false
FQDN="${GUEST_HOSTNAME}.${GUEST_DOMAIN}"

echo
c_info "About to deploy with:"
cat <<SUMMARY
  Prism Central : ${PC_HOST}:${PC_PORT}  (auth=${AUTH_MODE}, insecureTLS=${PC_INSECURE})
  Cluster       : ${CLUSTER_NAME}
  Subnet        : ${SUBNET_NAME}
  Image         : ${IMAGE_NAME}
  VM            : ${VM_NAME}  ${VCPU} vCPU / ${RAM_GB} GiB (overcommit=${MEM_OVERCOMMIT}) / ${DISK_GB} GiB
  Guest         : ${GUEST_USER}@${FQDN}
  Registry      : ${DOCKER_USERNAME} @ ${REGISTRY_SERVER}
  NAI version   : ${NAI_VERSION}   MicroK8s: ${MICROK8S_CHANNEL}
  MetalLB range : ${METALLB_RANGE}
  StorageClass  : ${STORAGE_CLASS}   ClickHouse CPU req: ${CH_CPU_REQUEST}
SUMMARY
ask_yn CONFIRM "Proceed?" "n"
[ "$CONFIRM" = "yes" ] || die "Aborted by user."

# --------------------------------------------------------------------------------------
# 2. Prism Central v4 API plumbing
# --------------------------------------------------------------------------------------
PC_BASE="https://${PC_HOST}:${PC_PORT}"
CURL_TLS=(); [ "$PC_INSECURE" = "yes" ] && CURL_TLS=(-k)
auth_args(){ if [ "$AUTH_MODE" = "apikey" ]; then printf '%s\n' "-H" "X-Ntnx-Api-Key: ${PC_API_KEY}";
             else printf '%s\n' "-u" "${PC_USER}:${PC_PASS}"; fi; }
uuidgen_(){ if command -v uuidgen >/dev/null 2>&1; then uuidgen; else cat /proc/sys/kernel/random/uuid; fi; }

# pc_get PATH -> body on stdout
pc_get(){ local path="$1"; mapfile -t A < <(auth_args)
  curl -sf "${CURL_TLS[@]}" "${A[@]}" -H 'Accept: application/json' "${PC_BASE}${path}"; }
# pc_get_etag VMID -> ETag header value
pc_get_etag(){ local id="$1"; mapfile -t A < <(auth_args)
  curl -s "${CURL_TLS[@]}" "${A[@]}" -D - -o /dev/null \
    "${PC_BASE}/api/vmm/v4.0/ahv/config/vms/${id}" | awk 'tolower($1)=="etag:"{print $2}' | tr -d '\r'; }
# pc_post PATH JSON [ETAG] -> body
pc_post(){ local path="$1" body="$2" etag="${3:-}"; mapfile -t A < <(auth_args)
  local hdr=(-H "Content-Type: application/json" -H "NTNX-Request-Id: $(uuidgen_)")
  [ -n "$etag" ] && hdr+=(-H "If-Match: ${etag}")
  curl -sf "${CURL_TLS[@]}" "${A[@]}" "${hdr[@]}" -X POST --data "$body" "${PC_BASE}${path}"; }

# wait_task TASK_EXTID
wait_task(){ local raw="$1" enc; enc=$(jq -rn --arg x "$raw" '$x|@uri'); local st det
  while :; do
    local t; t=$(pc_get "/api/prism/v4.0/config/tasks/${enc}")
    st=$(jq -r '.data.status // "UNKNOWN"' <<<"$t")
    case "$st" in
      SUCCEEDED) return 0;;
      FAILED|CANCELED) det=$(jq -r '.data.errorMessages[]?.message' <<<"$t"); die "Task ${st}: ${det}";;
      *) sleep 4;;
    esac
  done; }

c_info "Resolving cluster / subnet / image names to UUIDs ..."
CLUSTER_ID=$(pc_get "/api/clustermgmt/v4.0/config/clusters" \
  | jq -r --arg n "$CLUSTER_NAME" '.data[]|select(.name==$n)|.extId' | head -1)
[ -n "$CLUSTER_ID" ] || die "Cluster '$CLUSTER_NAME' not found."
SUBNET_ID=$(pc_get "/api/networking/v4.0/config/subnets" \
  | jq -r --arg n "$SUBNET_NAME" '.data[]|select(.name==$n)|.extId' | head -1)
[ -n "$SUBNET_ID" ] || die "Subnet '$SUBNET_NAME' not found."
IMAGE_ID=$(pc_get "/api/vmm/v4.0/content/images" \
  | jq -r --arg n "$IMAGE_NAME" '.data[]|select(.name|ascii_downcase|contains($n|ascii_downcase))|.extId' | head -1)
[ -n "$IMAGE_ID" ] || die "Image matching '$IMAGE_NAME' not found."
c_ok "cluster=$CLUSTER_ID  subnet=$SUBNET_ID  image=$IMAGE_ID"

# --------------------------------------------------------------------------------------
# 3. cloud-init + VM create
# --------------------------------------------------------------------------------------
CI_B64=$(cat <<EOF | base64 -w0
#cloud-config
hostname: ${GUEST_HOSTNAME}
fqdn: ${FQDN}
preserve_hostname: false
ssh_pwauth: true
users:
  - name: ${GUEST_USER}
    groups: [sudo]
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    shell: /bin/bash
    lock_passwd: false
chpasswd:
  expire: false
  users:
    - name: ${GUEST_USER}
      password: '${GUEST_PASSWORD}'
      type: text
EOF
)

VM_BODY=$(jq -n \
  --arg name "$VM_NAME" --arg desc "NAI ${NAI_VERSION} (Scenario D - MicroK8s)" \
  --argjson sockets "$VCPU" --argjson mem "$MEM_BYTES" --argjson disk "$DISK_BYTES" \
  --argjson oc "$OVERCOMMIT_JSON" \
  --arg cluster "$CLUSTER_ID" --arg subnet "$SUBNET_ID" --arg image "$IMAGE_ID" --arg ci "$CI_B64" '
{
  name:$name, description:$desc,
  numSockets:$sockets, numCoresPerSocket:1, numThreadsPerCore:1,
  memorySizeBytes:$mem, isMemoryOvercommitEnabled:$oc, machineType:"PC",
  cluster:{extId:$cluster},
  bootConfig:{"$objectType":"vmm.v4.ahv.config.LegacyBoot", bootOrder:["CDROM","DISK","NETWORK"]},
  disks:[{"$objectType":"vmm.v4.ahv.config.Disk",
    diskAddress:{"$objectType":"vmm.v4.ahv.config.DiskAddress", busType:"SCSI", index:0},
    backingInfo:{"$objectType":"vmm.v4.ahv.config.VmDisk", diskSizeBytes:$disk,
      dataSource:{"$objectType":"vmm.v4.ahv.config.DataSource",
        reference:{"$objectType":"vmm.v4.ahv.config.ImageReference", imageExtId:$image}}}}],
  nics:[{"$objectType":"vmm.v4.ahv.config.Nic",
    nicBackingInfo:{"$objectType":"vmm.v4.ahv.config.VirtualEthernetNic", model:"VIRTIO", isConnected:true},
    nicNetworkInfo:{"$objectType":"vmm.v4.ahv.config.VirtualEthernetNicNetworkInfo", nicType:"NORMAL_NIC",
      subnet:{"$objectType":"vmm.v4.ahv.config.SubnetReference", extId:$subnet},
      vlanMode:"ACCESS",
      ipv4Config:{"$objectType":"vmm.v4.ahv.config.Ipv4Config", shouldAssignIp:true}}}],
  guestCustomization:{"$objectType":"vmm.v4.ahv.config.GuestCustomizationParams",
    config:{"$objectType":"vmm.v4.ahv.config.CloudInit",
      cloudInitScript:{"$objectType":"vmm.v4.ahv.config.Userdata", value:$ci}}}
}')

c_info "Creating VM ${VM_NAME} (${VCPU} vCPU / ${RAM_GB} GiB / ${DISK_GB} GiB, overcommit=${MEM_OVERCOMMIT}) ..."
CREATE_TASK=$(pc_post "/api/vmm/v4.0/ahv/config/vms" "$VM_BODY" | jq -r '.data.extId')
[ -n "$CREATE_TASK" ] && [ "$CREATE_TASK" != "null" ] || die "createVm did not return a task."
wait_task "$CREATE_TASK"

# find the created VM's UUID from the task
TASK_ENC=$(jq -rn --arg x "$CREATE_TASK" '$x|@uri')
VM_ID=$(pc_get "/api/prism/v4.0/config/tasks/${TASK_ENC}" \
  | jq -r '(.data.completionDetails[]?|select(.name=="VM external ID")|.value)
           // (.data.entitiesAffected[]?|select(.rel=="vmm:ahv:config:vm")|.extId)' | head -1)
[ -n "$VM_ID" ] || die "Could not determine created VM UUID."
c_ok "VM created: ${VM_ID}"

c_info "Powering on ..."
ETAG=$(pc_get_etag "$VM_ID"); [ -n "$ETAG" ] || die "Could not read VM ETag."
PWR_TASK=$(pc_post "/api/vmm/v4.0/ahv/config/vms/${VM_ID}/\$actions/power-on" "{}" "$ETAG" | jq -r '.data.extId')
wait_task "$PWR_TASK"
c_ok "Powered on."

c_info "Waiting for an IP address from IPAM/DHCP ..."
VM_IP=""
for i in $(seq 1 60); do
  VM_IP=$(pc_get "/api/vmm/v4.0/ahv/config/vms/${VM_ID}" \
    | jq -r '.data.nics[0].nicNetworkInfo.ipv4Config.ipAddress.value // empty' 2>/dev/null || true)
  [ -n "$VM_IP" ] && break; sleep 5
done
[ -n "$VM_IP" ] || die "VM did not report an IP address."
c_ok "VM IP: ${VM_IP}"

# --------------------------------------------------------------------------------------
# 4. wait for SSH
# --------------------------------------------------------------------------------------
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10)
SSHPASS_RUN(){ sshpass -p "$GUEST_PASSWORD" ssh "${SSH_OPTS[@]}" "${GUEST_USER}@${VM_IP}" "$@"; }
c_info "Waiting for SSH + cloud-init to finish (can take a few minutes) ..."
for i in $(seq 1 60); do
  if SSHPASS_RUN "cloud-init status 2>/dev/null | grep -q done" 2>/dev/null; then break; fi
  sleep 10
done
SSHPASS_RUN "true" || die "SSH to ${GUEST_USER}@${VM_IP} failed."
c_ok "Guest reachable."

# --------------------------------------------------------------------------------------
# 5. in-guest install (MicroK8s + prerequisites + NAI 2.7)
# --------------------------------------------------------------------------------------
GUEST_SCRIPT=$(mktemp)
cat > "$GUEST_SCRIPT" <<'GUESTEOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$(uname -s)" = "Linux" ] || { echo "ERROR: guest installer is Linux-only" >&2; exit 1; }
: "${DOCKER_USERNAME:?}"; : "${DOCKER_PAT:?}"; : "${DOCKER_EMAIL:?}"; : "${METALLB_RANGE:?}"
NAI_VERSION="${NAI_VERSION:-2.7.0}"; MICROK8S_CHANNEL="${MICROK8S_CHANNEL:-1.32/stable}"
STORAGE_CLASS="${STORAGE_CLASS:-microk8s-hostpath}"; CH_CPU_REQUEST="${CH_CPU_REQUEST:-1}"
ENVOY_GW_VERSION="${ENVOY_GW_VERSION:-v1.7.0}"; KSERVE_VERSION="${KSERVE_VERSION:-v0.15.0}"
OTEL_OP_VERSION="${OTEL_OP_VERSION:-0.102.0}"; OBS_NAMESPACE="${OBS_NAMESPACE:-observability}"
REGISTRY_SERVER="${REGISTRY_SERVER:-https://index.docker.io/v1/}"; REGCRED_NAME="${REGCRED_NAME:-nai-regcred}"
NAI_NAMESPACE="${NAI_NAMESPACE:-nai-system}"; METALLB_FORCE="${METALLB_FORCE:-no}"
K="sudo microk8s kubectl"; H="sudo microk8s helm3"
log(){ echo; echo "=========== $* ==========="; }
c_warn(){ printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
c_ok(){   printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
c_info(){ printf '\033[1;34m[*]\033[0m %s\n' "$*"; }

# --- MetalLB range collision check (ping + TCP; MetalLB does not check for collisions) ---
ip_to_int(){ local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(( (a<<24) + (b<<16) + (c<<8) + d )); }
int_to_ip(){ local i="$1"; echo "$(( (i>>24)&255 )).$(( (i>>16)&255 )).$(( (i>>8)&255 )).$(( i&255 ))"; }
host_responds(){
  local ip="$1" port
  ping -c1 -W1 "$ip" >/dev/null 2>&1 && return 0
  for port in 22 80 443 445 3389 9440 8443; do
    timeout 1 bash -c "echo >/dev/tcp/${ip}/${port}" >/dev/null 2>&1 && return 0
  done
  return 1
}
check_metallb_range(){
  local range="$1" sip eip si ei count ip a tmpdir
  local -a conflicts=()
  local ipre='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
  case "$range" in
    *-*) sip="${range%-*}"; eip="${range#*-}" ;;
    *)   c_warn "MetalLB range '$range' is not a start-end range; skipping collision check."; return 0 ;;
  esac
  [[ "$sip" =~ $ipre ]] && [[ "$eip" =~ $ipre ]] || { c_warn "Could not parse '$range' as start-end IPs; skipping collision check."; return 0; }
  si=$(ip_to_int "$sip"); ei=$(ip_to_int "$eip")
  [ "$si" -le "$ei" ] || { c_warn "MetalLB range start is after end; skipping collision check."; return 0; }
  count=$(( ei - si + 1 ))
  if [ "$count" -gt 64 ]; then
    c_warn "MetalLB range has ${count} addresses; only scanning the first 64 for collisions."
    ei=$(( si + 63 ))
  fi
  c_info "Checking ${sip}-$(int_to_ip "$ei") for hosts already using these addresses (ping + TCP probe) ..."
  tmpdir=$(mktemp -d)
  for (( ip=si; ip<=ei; ip++ )); do
    a=$(int_to_ip "$ip")
    ( host_responds "$a" && touch "$tmpdir/$a" ) &
  done
  wait
  for a in "$tmpdir"/*; do [ -e "$a" ] && conflicts+=("$(basename "$a")"); done
  rm -rf "$tmpdir"
  if [ "${#conflicts[@]}" -gt 0 ]; then
    c_warn "The following address(es) in the MetalLB range already respond (ping or TCP) and may be IN USE:"
    printf '        %s\n' "${conflicts[@]}" >&2
    c_warn "MetalLB does NOT check for collisions - assigning an address someone else owns causes"
    c_warn "ARP conflicts / intermittent connectivity once the gateway starts advertising it."
    if [ "$METALLB_FORCE" = "yes" ]; then
      c_warn "METALLB_FORCE=yes set - proceeding despite the warning above."
    else
      echo "ERROR: potential MetalLB IP collision. Re-run with METALLB_FORCE=yes to override, or choose a different METALLB_RANGE." >&2
      exit 1
    fi
  else
    c_ok "No responses from any address in ${sip}-$(int_to_ip "$ei") - range looks free."
  fi
}
check_metallb_range "${METALLB_RANGE}"

log "D.2 Install MicroK8s ${MICROK8S_CHANNEL}"
snap list microk8s >/dev/null 2>&1 || sudo snap install microk8s --classic --channel="${MICROK8S_CHANNEL}"
sudo usermod -a -G microk8s "$(whoami)" || true
sudo microk8s status --wait-ready >/dev/null

log "Configure containerd Docker Hub auth (BEFORE any NAI pull)"
TEMPLATE=/var/snap/microk8s/current/args/containerd-template.toml
if ! sudo grep -q 'registry-1.docker.io".auth' "$TEMPLATE"; then
  sudo cp "$TEMPLATE" "${TEMPLATE}.bak.$(date +%s)"
  sudo tee -a "$TEMPLATE" >/dev/null <<EOF

[plugins."io.containerd.grpc.v1.cri".registry.configs."registry-1.docker.io".auth]
  username = "${DOCKER_USERNAME}"
  password = "${DOCKER_PAT}"
EOF
  sudo microk8s stop; sudo microk8s start; sudo microk8s status --wait-ready >/dev/null
fi

log "D.3 Enable add-ons (no GPU)"
sudo microk8s enable hostpath-storage
sudo microk8s enable cert-manager
sudo microk8s enable "metallb:${METALLB_RANGE}"
sudo microk8s enable ingress
sudo microk8s enable metrics-server
sudo microk8s enable observability
$K apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: selfsigned-issuer }
spec: { selfSigned: {} }
EOF

log "D.5 Envoy Gateway CRDs ${ENVOY_GW_VERSION}"
$H template eg oci://docker.io/envoyproxy/gateway-crds-helm --version "${ENVOY_GW_VERSION}" \
  --set crds.gatewayAPI.enabled=true --set crds.envoyGateway.enabled=true | $K apply --server-side --force-conflicts -f -
cat > /tmp/eg-config.yaml <<'EOF'
config:
  envoyGateway:
    gateway: { controllerName: "gateway.envoyproxy.io/gatewayclass-controller" }
    logging: { level: { default: "info" } }
    provider:
      kubernetes:
        rateLimitDeployment:
          container: { image: "docker.io/envoyproxy/ratelimit:99d85510" }
          patch:
            type: "StrategicMerge"
            value:
              spec:
                template:
                  spec:
                    containers:
                      - imagePullPolicy: "IfNotPresent"
                        name: "envoy-ratelimit"
                        image: "docker.io/envoyproxy/ratelimit:99d85510"
      type: "Kubernetes"
    extensionApis: { enableEnvoyPatchPolicy: true, enableBackend: true }
    extensionManager:
      maxMessageSize: 11Mi
      backendResources:
        - { group: inference.networking.k8s.io, kind: InferencePool, version: v1 }
      hooks:
        xdsTranslator:
          translation:
            listener: { includeAll: true }
            route: { includeAll: true }
            cluster: { includeAll: true }
            secret: { includeAll: true }
          post: ["Translation", "Cluster", "Route"]
      service:
        fqdn: { hostname: "ai-gateway-controller.nai-system.svc.cluster.local", port: 1063 }
    rateLimit:
      backend:
        type: "Redis"
        redis: { url: "redis-sentinel.nai-system.svc.cluster.local:6379" }
EOF
log "D.5 Envoy Gateway ${ENVOY_GW_VERSION}"
$H upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version "${ENVOY_GW_VERSION}" \
  -n envoy-gateway-system --create-namespace --skip-crds -f /tmp/eg-config.yaml
$K wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

log "D.5 KServe ${KSERVE_VERSION}"
$H upgrade --install kserve-crd oci://ghcr.io/kserve/charts/kserve-crd --version "${KSERVE_VERSION}" -n kserve --create-namespace --wait
$H upgrade --install kserve oci://ghcr.io/kserve/charts/kserve --version "${KSERVE_VERSION}" -n kserve --create-namespace --wait \
  --set kserve.controller.deploymentMode=RawDeployment --set kserve.controller.gateway.disableIngressCreation=true

log "D.5 OpenTelemetry Operator ${OTEL_OP_VERSION}"
$H upgrade --install opentelemetry-operator opentelemetry-operator \
  --repo https://open-telemetry.github.io/opentelemetry-helm-charts --version "${OTEL_OP_VERSION}" -n opentelemetry --create-namespace --wait

log "D.6 Pull secret ${REGCRED_NAME}"
$K create namespace "${NAI_NAMESPACE}" --dry-run=client -o yaml | $K apply -f -
for NS in "${NAI_NAMESPACE}" envoy-gateway-system; do
  $K -n "$NS" create secret docker-registry "${REGCRED_NAME}" \
    --docker-server="${REGISTRY_SERVER}" --docker-username="${DOCKER_USERNAME}" \
    --docker-password="${DOCKER_PAT}" --docker-email="${DOCKER_EMAIL}" \
    --dry-run=client -o yaml | $K apply -f -
done

log "D.6 Helm repo + nai-operators ${NAI_VERSION}"
$H repo add ntnx-charts https://nutanix.github.io/helm-releases >/dev/null 2>&1 || true
$H repo update ntnx-charts
$H upgrade --install nai-operators ntnx-charts/nai-operators --version "${NAI_VERSION}" \
  -n "${NAI_NAMESPACE}" --create-namespace --wait --timeout 15m \
  --set global.imagePullSecrets[0].name="${REGCRED_NAME}"

log "D.6 nai-core ${NAI_VERSION} (ClickHouse CPU request=${CH_CPU_REQUEST})"
$H upgrade --install nai-core ntnx-charts/nai-core --version "${NAI_VERSION}" \
  -n "${NAI_NAMESPACE}" --create-namespace --wait --timeout 20m --insecure-skip-tls-verify \
  --set global.imagePullSecrets[0].name="${REGCRED_NAME}" \
  --set global.storage.storageClassNameRWX="${STORAGE_CLASS}" \
  --set global.storage.storageClassName="${STORAGE_CLASS}" \
  --set naiMonitoring.nodeExporter.serviceMonitor.namespaceSelector.matchNames[0]="${OBS_NAMESPACE}" \
  --set naiMonitoring.dcgmExporter.serviceMonitor.namespaceSelector.matchNames[0]="${OBS_NAMESPACE}" \
  --set nai-clickhouse-server.clickhouse.resources.requests.cpu="${CH_CPU_REQUEST}"

log "D.7 Discover gateway LoadBalancer IP + create ingress certificate"
LB_IP=""
for i in $(seq 1 30); do
  LB_IP=$($K -n envoy-gateway-system get svc \
    -l "gateway.envoyproxy.io/owning-gateway-name=nai-ingress-gateway,gateway.envoyproxy.io/owning-gateway-namespace=${NAI_NAMESPACE}" \
    -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
  [ -n "$LB_IP" ] && break; sleep 5
done
[ -n "$LB_IP" ] || { echo "ERROR: no gateway LB IP"; exit 1; }
NAI_HOST="nai.${LB_IP}.nip.io"
$K apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: nai-ingress-cert, namespace: ${NAI_NAMESPACE} }
spec:
  issuerRef: { name: selfsigned-issuer, kind: ClusterIssuer }
  secretName: ingress-certificate
  commonName: ${NAI_HOST}
  dnsNames: [ ${NAI_HOST} ]
  ipAddresses: [ ${LB_IP} ]
EOF
$K -n "${NAI_NAMESPACE}" wait --for=condition=Ready certificate/nai-ingress-cert --timeout=3m

log "Summary"
AU=$($K -n "${NAI_NAMESPACE}" get secret nai-default-user -o jsonpath='{.data.NAI_SUPERADMIN_USERNAME}' | base64 -d)
AP=$($K -n "${NAI_NAMESPACE}" get secret nai-default-user -o jsonpath='{.data.NAI_SUPERADMIN_PASSWORD}' | base64 -d)
CODE=$(curl -sk -o /dev/null -w '%{http_code}' "https://${NAI_HOST}/nai" || true)
echo "NAI_UI=https://${NAI_HOST}/nai"
echo "NAI_ADMIN_USER=${AU}"
echo "NAI_ADMIN_PASS=${AP}"
echo "NAI_UI_HTTP_PROBE=${CODE}"
GUESTEOF

c_info "Uploading and running the in-guest installer (this is the long part) ..."
sshpass -p "$GUEST_PASSWORD" scp -O "${SSH_OPTS[@]}" "$GUEST_SCRIPT" "${GUEST_USER}@${VM_IP}:/tmp/nai-guest.sh"
rm -f "$GUEST_SCRIPT"

sshpass -p "$GUEST_PASSWORD" ssh "${SSH_OPTS[@]}" "${GUEST_USER}@${VM_IP}" \
  "sed -i 's/\r$//' /tmp/nai-guest.sh; \
   DOCKER_USERNAME='${DOCKER_USERNAME}' DOCKER_PAT='${DOCKER_PAT}' DOCKER_EMAIL='${DOCKER_EMAIL}' \
   METALLB_RANGE='${METALLB_RANGE}' NAI_VERSION='${NAI_VERSION}' MICROK8S_CHANNEL='${MICROK8S_CHANNEL}' \
   STORAGE_CLASS='${STORAGE_CLASS}' CH_CPU_REQUEST='${CH_CPU_REQUEST}' OBS_NAMESPACE='${OBS_NAMESPACE}' \
   ENVOY_GW_VERSION='${ENVOY_GW_VERSION}' KSERVE_VERSION='${KSERVE_VERSION}' OTEL_OP_VERSION='${OTEL_OP_VERSION}' \
   REGISTRY_SERVER='${REGISTRY_SERVER}' METALLB_FORCE='${METALLB_FORCE}' bash /tmp/nai-guest.sh" | tee /tmp/nai-deploy-guest.out

# --------------------------------------------------------------------------------------
# 6. final report
# --------------------------------------------------------------------------------------
echo
c_ok "Deployment complete."
UI=$(awk -F= '/^NAI_UI=/{print $2}'         /tmp/nai-deploy-guest.out | tail -1)
AU=$(awk -F= '/^NAI_ADMIN_USER=/{print $2}' /tmp/nai-deploy-guest.out | tail -1)
AP=$(awk -F= '/^NAI_ADMIN_PASS=/{print $2}' /tmp/nai-deploy-guest.out | tail -1)
cat <<DONE

  ============================================================
   Nutanix Enterprise AI ${NAI_VERSION} - Scenario D
  ============================================================
   VM            : ${VM_NAME}  (${VM_IP})
   SSH           : ssh ${GUEST_USER}@${VM_IP}
   NAI UI        : ${UI}
   Super-admin   : ${AU} / ${AP}
  ------------------------------------------------------------
   NOTE: self-signed TLS - accept the browser warning.
   NOTE: control-plane only (no GPU). Attach a GPU + NVIDIA
         driver + 'microk8s enable nvidia' to serve models.
  ============================================================
DONE
