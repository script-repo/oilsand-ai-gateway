#!/usr/bin/env bash
# --- CRLF self-heal: if this file was saved/transferred with Windows (CRLF) line -----
# --- endings, transparently re-exec a cleaned copy. You should never need dos2unix. --
[ -z "${NAI_DECRLF:-}" ] && grep -q $'\r' "$0" 2>/dev/null && NAI_DECRLF=1 exec bash <(tr -d '\r' < "$0") "$@"
#
# deploy-nai-scenario-d-inguest.sh
# ================================
# Deploy Nutanix Enterprise AI (NAI) 2.7 - "Scenario D" (MicroK8s) INSIDE an existing
# Ubuntu 24.04 VM. Run this ON the VM itself.
#
# This is the simplified sibling of deploy-nai-scenario-d.sh: it drops all Prism Central
# VM provisioning, SSH, and scp. You bring your own Ubuntu VM (already sized with enough
# CPU/RAM/disk and on a subnet where you can carve out a MetalLB range); this script does
# only the in-guest install: MicroK8s -> add-ons -> AI-gateway prereqs -> NAI 2.7 -> TLS/UI.
#
# Platform: LINUX ONLY. Run this on the target Ubuntu VM itself (not on your workstation).
# No dos2unix required - the script detects and self-heals Windows line endings above.
#
# Requirements on the VM:
#   * Ubuntu 24.04 with snap + sudo (passwordless sudo recommended for a hands-off run)
#   * Outbound internet (Docker Hub, ghcr.io, snapcraft, Nutanix Helm repo)
#   * A MetalLB IP range on the VM's L2 subnet, OUTSIDE the DHCP pool
#   * A Docker Hub account/PAT entitled to the private nutanix/nai-* images
#
# Usage:
#   chmod +x deploy-nai-scenario-d-inguest.sh
#   ./deploy-nai-scenario-d-inguest.sh                 # interactive prompts
#   # or pre-seed any answer via env, e.g.:
#   DOCKER_USERNAME=me DOCKER_PAT=xxx DOCKER_EMAIL=me@x.com METALLB_RANGE=10.0.0.240-10.0.0.250 \
#     NONINTERACTIVE=yes ./deploy-nai-scenario-d-inguest.sh
#
#   NOTE on line endings: if this file was hand-copied/edited through a Windows tool and
#   ended up with CRLF on every line (including the #! line itself), the kernel can fail
#   to even locate bash before our CRLF self-heal code (above) gets a chance to run - no
#   script can fix that from the inside. If `./deploy-nai-scenario-d-inguest.sh` ever
#   errors immediately with something like "bad interpreter", run it as
#   `bash deploy-nai-scenario-d-inguest.sh` instead: that bypasses the OS's #! lookup
#   entirely, and the self-heal line still cleans up and re-execs correctly.
#
# Lessons baked in (see NAI-2.7-ScenarioD-Deployment.md):
#   * containerd Docker Hub auth is configured BEFORE any NAI image is pulled.
#   * ClickHouse CPU request is right-sized via a Helm value (upgrade-safe).
#   * Generous Helm --timeout values; idempotent (safe to re-run).
#   * The requested MetalLB range is probed (ping + TCP) before use; MetalLB itself does
#     not check for collisions and will happily ARP-announce an IP someone else already owns.
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
# helpers
# --------------------------------------------------------------------------------------
c_info(){ printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
c_ok(){   printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
c_warn(){ printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
c_err(){  printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; }
die(){ c_err "$*"; exit 1; }
log(){ echo; echo "=========== $* ==========="; }

NONINTERACTIVE="${NONINTERACTIVE:-no}"

# ask VAR "Prompt" "default" : uses existing env value if set, else prompts (unless NONINTERACTIVE)
ask(){ local __v="$1" __p="$2" __d="${3:-}" __cur __in
  __cur="${!__v:-}"; [ -n "$__cur" ] && { printf -v "$__v" '%s' "$__cur"; return; }
  if [ "$NONINTERACTIVE" = "yes" ]; then printf -v "$__v" '%s' "$__d"; return; fi
  if [ -n "$__d" ]; then read -r -p "$__p [$__d]: " __in || true; __in="${__in:-$__d}";
  else read -r -p "$__p: " __in || true; fi
  printf -v "$__v" '%s' "$__in"; }

# ask_secret VAR "Prompt" : uses existing env value if set, else prompts hidden
ask_secret(){ local __v="$1" __p="$2" __cur __in
  __cur="${!__v:-}"; [ -n "$__cur" ] && return
  [ "$NONINTERACTIVE" = "yes" ] && die "$__v must be provided via env in NONINTERACTIVE mode"
  read -r -s -p "$__p: " __in || true; echo; printf -v "$__v" '%s' "$__in"; }

ask_yn(){ local __v="$1" __p="$2" __d="${3:-y}" __cur __in
  __cur="${!__v:-}"; [ -n "$__cur" ] && { printf -v "$__v" '%s' "$__cur"; return; }
  if [ "$NONINTERACTIVE" = "yes" ]; then __in="$__d"; else read -r -p "$__p (y/n) [$__d]: " __in || true; __in="${__in:-$__d}"; fi
  case "$__in" in [Yy]*) printf -v "$__v" 'yes';; *) printf -v "$__v" 'no';; esac; }

# --------------------------------------------------------------------------------------
# MetalLB range collision check (ping + TCP; MetalLB does not check for collisions itself)
# --------------------------------------------------------------------------------------
ip_to_int(){ local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(( (a<<24) + (b<<16) + (c<<8) + d )); }
int_to_ip(){ local i="$1"; echo "$(( (i>>24)&255 )).$(( (i>>16)&255 )).$(( (i>>8)&255 )).$(( i&255 ))"; }

# host_responds IP : returns 0 (in use) if ping OR a quick TCP connect on common ports succeeds
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
    if [ "${METALLB_FORCE:-no}" = "yes" ]; then
      c_warn "METALLB_FORCE=yes set - proceeding despite the warning above."
    elif [ "$NONINTERACTIVE" = "yes" ]; then
      die "Aborting (NONINTERACTIVE mode). Pick a different METALLB_RANGE or re-run with METALLB_FORCE=yes."
    else
      ask_yn PROCEED_DESPITE_CONFLICT "Continue anyway with this potentially conflicting range?" "n"
      [ "$PROCEED_DESPITE_CONFLICT" = "yes" ] || die "Aborted - choose a different METALLB_RANGE and re-run."
    fi
  else
    c_ok "No responses from any address in ${sip}-$(int_to_ip "$ei") - range looks free."
  fi
}

# --------------------------------------------------------------------------------------
# preflight
# --------------------------------------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || die "This script is Linux-only. Run it on the target Ubuntu VM itself, not on your workstation."
[ "$(id -u)" -eq 0 ] || sudo -n true 2>/dev/null || \
  c_warn "passwordless sudo not detected; you may be prompted for your sudo password during the run."
command -v snap >/dev/null 2>&1 || die "snap not found - this script targets Ubuntu with snapd."
if ! grep -qi '24.04' /etc/os-release 2>/dev/null; then
  c_warn "This VM does not look like Ubuntu 24.04; continuing anyway."
fi
c_info "Started: ${DEPLOY_START_HUMAN}"

# --------------------------------------------------------------------------------------
# variables (prompt, with env pre-seed)
# --------------------------------------------------------------------------------------
echo
c_info "NAI 2.7 - Scenario D in-guest installer. Answer the prompts (Enter accepts the default)."
echo

ask        DOCKER_USERNAME  "Docker Hub username (NAI entitlement)"
ask_secret DOCKER_PAT       "Docker Hub password / PAT"
ask        DOCKER_EMAIL     "Docker Hub email"
ask        REGISTRY_SERVER  "Registry server" "https://index.docker.io/v1/"
[ -n "${DOCKER_USERNAME}" ] && [ -n "${DOCKER_PAT}" ] && [ -n "${DOCKER_EMAIL}" ] || die "Docker Hub credentials are required."

ask        METALLB_RANGE    "MetalLB address range on this VM's subnet (outside the DHCP pool), e.g. 10.0.0.240-10.0.0.250"
[ -n "${METALLB_RANGE}" ] || die "METALLB_RANGE is required."
check_metallb_range "${METALLB_RANGE}"

ask        NAI_VERSION      "NAI chart version" "2.7.0"
ask        MICROK8S_CHANNEL "MicroK8s snap channel" "1.32/stable"
ask        STORAGE_CLASS    "Kubernetes StorageClass" "microk8s-hostpath"
ask        CH_CPU_REQUEST   "ClickHouse CPU request (cores) - keep small on tight nodes" "1"
ask        OBS_NAMESPACE    "Observability namespace" "observability"
ask        NAI_NAMESPACE    "NAI namespace" "nai-system"
ask        REGCRED_NAME     "Image pull secret name" "nai-regcred"

# AI-gateway prerequisite versions (rarely changed)
ENVOY_GW_VERSION="${ENVOY_GW_VERSION:-v1.7.0}"
KSERVE_VERSION="${KSERVE_VERSION:-v0.15.0}"
OTEL_OP_VERSION="${OTEL_OP_VERSION:-0.102.0}"

echo
c_info "Deploying NAI ${NAI_VERSION} on this VM:"
cat <<SUMMARY
  Registry      : ${DOCKER_USERNAME} @ ${REGISTRY_SERVER}
  MicroK8s      : ${MICROK8S_CHANNEL}
  MetalLB range : ${METALLB_RANGE}
  StorageClass  : ${STORAGE_CLASS}   ClickHouse CPU req: ${CH_CPU_REQUEST}
  Namespaces    : ${NAI_NAMESPACE} (NAI), ${OBS_NAMESPACE} (observability)
SUMMARY
ask_yn CONFIRM "Proceed?" "y"
[ "$CONFIRM" = "yes" ] || die "Aborted by user."

K="sudo microk8s kubectl"
H="sudo microk8s helm3"

# --------------------------------------------------------------------------------------
# D.2 - MicroK8s
# --------------------------------------------------------------------------------------
log "D.2 Install MicroK8s ${MICROK8S_CHANNEL}"
snap list microk8s >/dev/null 2>&1 || sudo snap install microk8s --classic --channel="${MICROK8S_CHANNEL}"
sudo usermod -a -G microk8s "$(whoami)" || true
sudo snap alias microk8s.kubectl kubectl >/dev/null 2>&1 || true
sudo snap alias microk8s.helm3  helm     >/dev/null 2>&1 || true
sudo microk8s status --wait-ready >/dev/null

# --------------------------------------------------------------------------------------
# LESSON 1 - containerd Docker Hub auth BEFORE pulling NAI images
# --------------------------------------------------------------------------------------
log "Configure containerd Docker Hub auth (avoids anonymous rate-limit on private nutanix/nai-* images)"
TEMPLATE=/var/snap/microk8s/current/args/containerd-template.toml
if ! sudo grep -q 'registry-1.docker.io".auth' "$TEMPLATE"; then
  sudo cp "$TEMPLATE" "${TEMPLATE}.bak.$(date +%s)"
  sudo tee -a "$TEMPLATE" >/dev/null <<EOF

[plugins."io.containerd.grpc.v1.cri".registry.configs."registry-1.docker.io".auth]
  username = "${DOCKER_USERNAME}"
  password = "${DOCKER_PAT}"
EOF
  sudo microk8s stop
  sudo microk8s start
  sudo microk8s status --wait-ready >/dev/null
else
  echo "containerd auth already present; skipping."
fi

# --------------------------------------------------------------------------------------
# D.3 - Add-ons + ClusterIssuer (no GPU)
# --------------------------------------------------------------------------------------
log "D.3 Enable add-ons (no GPU)"
sudo microk8s enable hostpath-storage
sudo microk8s enable cert-manager
sudo microk8s enable "metallb:${METALLB_RANGE}"
sudo microk8s enable ingress
sudo microk8s enable metrics-server
sudo microk8s enable observability

log "Self-signed ClusterIssuer"
$K apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: { name: selfsigned-issuer }
spec: { selfSigned: {} }
EOF

# --------------------------------------------------------------------------------------
# D.5 - AI Gateway prerequisites
# --------------------------------------------------------------------------------------
log "D.5 Envoy Gateway CRDs ${ENVOY_GW_VERSION}"
$H template eg oci://docker.io/envoyproxy/gateway-crds-helm --version "${ENVOY_GW_VERSION}" \
  --set crds.gatewayAPI.enabled=true --set crds.envoyGateway.enabled=true \
  | $K apply --server-side --force-conflicts -f -

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
log "D.5 Envoy Gateway ${ENVOY_GW_VERSION} (AI Gateway mode)"
$H upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version "${ENVOY_GW_VERSION}" \
  -n envoy-gateway-system --create-namespace --skip-crds -f /tmp/eg-config.yaml
$K wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

log "D.5 KServe ${KSERVE_VERSION}"
$H upgrade --install kserve-crd oci://ghcr.io/kserve/charts/kserve-crd --version "${KSERVE_VERSION}" \
  -n kserve --create-namespace --wait
$H upgrade --install kserve oci://ghcr.io/kserve/charts/kserve --version "${KSERVE_VERSION}" \
  -n kserve --create-namespace --wait \
  --set kserve.controller.deploymentMode=RawDeployment \
  --set kserve.controller.gateway.disableIngressCreation=true

log "D.5 OpenTelemetry Operator ${OTEL_OP_VERSION}"
$H upgrade --install opentelemetry-operator opentelemetry-operator \
  --repo https://open-telemetry.github.io/opentelemetry-helm-charts --version "${OTEL_OP_VERSION}" \
  -n opentelemetry --create-namespace --wait

# --------------------------------------------------------------------------------------
# D.6 - NAI 2.7
# --------------------------------------------------------------------------------------
log "D.6 Registry pull secret in ${NAI_NAMESPACE} + envoy-gateway-system"
$K create namespace "${NAI_NAMESPACE}" --dry-run=client -o yaml | $K apply -f -
for NS in "${NAI_NAMESPACE}" envoy-gateway-system; do
  $K -n "$NS" create secret docker-registry "${REGCRED_NAME}" \
    --docker-server="${REGISTRY_SERVER}" --docker-username="${DOCKER_USERNAME}" \
    --docker-password="${DOCKER_PAT}" --docker-email="${DOCKER_EMAIL}" \
    --dry-run=client -o yaml | $K apply -f -
done

log "D.6 Helm repo ntnx-charts"
$H repo add ntnx-charts https://nutanix.github.io/helm-releases >/dev/null 2>&1 || true
$H repo update ntnx-charts

log "D.6 nai-operators ${NAI_VERSION}"
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

# --------------------------------------------------------------------------------------
# D.7 - TLS + expose UI
# --------------------------------------------------------------------------------------
log "D.7 Discover ingress gateway LoadBalancer IP"
LB_IP=""
for i in $(seq 1 30); do
  LB_IP=$($K -n envoy-gateway-system get svc \
    -l "gateway.envoyproxy.io/owning-gateway-name=nai-ingress-gateway,gateway.envoyproxy.io/owning-gateway-namespace=${NAI_NAMESPACE}" \
    -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
  [ -n "$LB_IP" ] && break
  sleep 5
done
[ -n "$LB_IP" ] || die "Could not determine gateway LoadBalancer IP."
NAI_HOST="nai.${LB_IP}.nip.io"
echo "Gateway LB IP: ${LB_IP}   Host: ${NAI_HOST}"

log "D.7 Ingress certificate (populates Secret 'ingress-certificate' the gateway expects)"
$K apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: nai-ingress-cert
  namespace: ${NAI_NAMESPACE}
spec:
  issuerRef:
    name: selfsigned-issuer
    kind: ClusterIssuer
  secretName: ingress-certificate
  commonName: ${NAI_HOST}
  dnsNames:
    - ${NAI_HOST}
  ipAddresses:
    - ${LB_IP}
EOF
$K -n "${NAI_NAMESPACE}" wait --for=condition=Ready certificate/nai-ingress-cert --timeout=3m

# --------------------------------------------------------------------------------------
# summary
# --------------------------------------------------------------------------------------
log "Deployment summary"
ADMIN_USER=$($K -n "${NAI_NAMESPACE}" get secret nai-default-user -o jsonpath='{.data.NAI_SUPERADMIN_USERNAME}' | base64 -d)
ADMIN_PASS=$($K -n "${NAI_NAMESPACE}" get secret nai-default-user -o jsonpath='{.data.NAI_SUPERADMIN_PASSWORD}' | base64 -d)
ADMIN_MAIL=$($K -n "${NAI_NAMESPACE}" get secret nai-default-user -o jsonpath='{.data.NAI_SUPERADMIN_EMAIL}' | base64 -d)
HTTP_CODE=$(curl -sk -o /dev/null -w '%{http_code}' "https://${NAI_HOST}/nai" || true)
cat <<DONE

  ============================================================
   Nutanix Enterprise AI ${NAI_VERSION} - Scenario D (in-guest)
  ============================================================
   NAI UI        : https://${NAI_HOST}/nai   (HTTP probe=${HTTP_CODE})
   Super-admin   : ${ADMIN_USER} / ${ADMIN_PASS}   (email ${ADMIN_MAIL})
   Gateway LB IP : ${LB_IP}
  ------------------------------------------------------------
   NOTE: self-signed TLS - accept the browser warning.
   NOTE: control-plane only (no GPU). Attach a GPU + NVIDIA
         driver + 'microk8s enable nvidia' to serve models.
  ============================================================
DONE
