#!/usr/bin/env bash
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
# Lessons baked in (see NAI-2.7-ScenarioD-Deployment.md):
#   * containerd Docker Hub auth is configured BEFORE any NAI image is pulled.
#   * ClickHouse CPU request is right-sized via a Helm value (upgrade-safe).
#   * Generous Helm --timeout values; idempotent (safe to re-run).
#
set -euo pipefail

# --------------------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------------------
c_info(){ printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
c_ok(){   printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
c_warn(){ printf '\033[1;33m[!]\033[0m %s\n' "$*"; }
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
# preflight
# --------------------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || sudo -n true 2>/dev/null || \
  c_warn "passwordless sudo not detected; you may be prompted for your sudo password during the run."
command -v snap >/dev/null 2>&1 || die "snap not found - this script targets Ubuntu with snapd."
if ! grep -qi '24.04' /etc/os-release 2>/dev/null; then
  c_warn "This VM does not look like Ubuntu 24.04; continuing anyway."
fi

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
ask        METALLB_RANGE    "MetalLB address range on this VM's subnet (outside the DHCP pool), e.g. 10.0.0.240-10.0.0.250"
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

[ -n "${DOCKER_USERNAME}" ] && [ -n "${DOCKER_PAT}" ] && [ -n "${DOCKER_EMAIL}" ] || die "Docker Hub credentials are required."
[ -n "${METALLB_RANGE}" ] || die "METALLB_RANGE is required."

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
