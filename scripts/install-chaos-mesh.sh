#!/usr/bin/env bash
#
# Install Chaos Mesh into a kind cluster, reproducibly, for MaKlaude's chaos
# lifecycle tests.
#
# ONE script rather than a workflow step plus a README paragraph, because "installs
# reproducibly in CI and locally" is not a property two similar procedures can have.
# The CI job (.github/workflows/e2e.yml) and `task chaos:install` both call this file,
# so an operator reproducing a CI failure on their laptop runs the same code, and a
# fix to either path is a fix to both. The pinned version below is the single source
# of that pin: nothing else in the repo names a Chaos Mesh version.
#
# WHY kind NEEDS OVERRIDES
#
# The chart defaults to the Docker runtime (`chaosDaemon.runtime=docker`,
# socketPath=/var/run/docker.sock). A kind node is a container running containerd, so
# chaos-daemon must be pointed at containerd's socket instead. Get this wrong and the
# failure is not an install error: the daemon comes up, the controller accepts a
# PodChaos, and the fault never lands — an experiment that reports injected while
# nothing broke, which is the exact failure mode internal/chaos's closed action
# catalog exists to avoid on the CR side.
#
# WHAT IS TRIMMED, AND WHY IT IS SAFE
#
# The dashboard, the DNS server and two of three controller replicas are disabled.
# MaKlaude's catalog is PodChaos only (see internal/chaos/chaos.go), so DNSChaos
# infrastructure is unused; the dashboard is a human UI nothing here drives; and three
# controller replicas on a single-node kind cluster buy no availability while costing
# pull and scheduling time on every CI run. None of them is on the path an experiment
# takes.
#
# WHAT IS PINNED ON, AND WHY IT LOOKS UNRELATED
#
# `dashboard.securityMode=true` is set explicitly even though the dashboard itself is
# disabled, because that value does not only configure the dashboard: the chart feeds
# it to the CONTROLLER as SECURITY_MODE, and the controller uses it to enable
# `vauth.kb.io` — the webhook that authorizes an experiment against the namespaces its
# selector names (chaos-mesh v2.8.3, cmd/chaos-controller-manager/main.go). It defaults
# to true, so this changes nothing today; it is pinned so that MaKlaude's RBAC keeps
# being tested under the posture it was designed for, rather than silently losing a
# blast-radius bound to an upstream default flip. Under that posture MaKlaude needs a
# per-target-namespace grant — deploy/rbac/chaos/target-namespace-role.yaml — and
# without it every experiment is denied by admission. That denial is the bound working.
#
# Usage:
#   scripts/install-chaos-mesh.sh [--namespace NS] [--timeout DURATION] [--uninstall]
#
# Environment:
#   KUBECONFIG        as usual; the script asserts it points at a reachable cluster.
#   KUBE_CONTEXT      optional context to select.
#   CHAOS_MESH_VERSION  overrides the pinned chart version. Provided for bisecting an
#                       upstream regression, NOT for routine use — an unpinned install
#                       is the thing this script exists to prevent.

set -euo pipefail

# The pinned chart version. Chart version and appVersion move together in this repo's
# chart index, so one pin covers both the CRDs and the images.
CHAOS_MESH_VERSION="${CHAOS_MESH_VERSION:-2.8.3}"
CHAOS_MESH_REPO="https://charts.chaos-mesh.org"
RELEASE_NAME="chaos-mesh"

NAMESPACE="chaos-mesh"
TIMEOUT="10m"
UNINSTALL=0

die() { echo "install-chaos-mesh: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace) NAMESPACE="${2:?--namespace needs a value}"; shift 2 ;;
    --timeout)   TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --version)   echo "$CHAOS_MESH_VERSION"; exit 0 ;;
    -h|--help)   sed -n '2,40p' "$0"; exit 0 ;;
    *)           die "unknown argument: $1" ;;
  esac
done

command -v helm >/dev/null 2>&1 || die "helm not found on PATH"
command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"

KUBECTL=(kubectl)
HELM=(helm)
if [ -n "${KUBE_CONTEXT:-}" ]; then
  KUBECTL+=(--context "$KUBE_CONTEXT")
  HELM+=(--kube-context "$KUBE_CONTEXT")
fi

if [ "$UNINSTALL" -eq 1 ]; then
  # Uninstall leaves the CRDs behind, which is Helm's behaviour for crds/ and is the
  # right one here: removing a CRD cascades to every custom resource of that kind,
  # including a human's own experiments on a shared cluster. Removing them is a
  # deliberate act, not a side effect of tearing MaKlaude's install down.
  "${HELM[@]}" uninstall "$RELEASE_NAME" --namespace "$NAMESPACE" --wait --timeout "$TIMEOUT" || true
  "${KUBECTL[@]}" delete namespace "$NAMESPACE" --ignore-not-found
  echo "install-chaos-mesh: uninstalled (CRDs deliberately left in place)"
  exit 0
fi

"${KUBECTL[@]}" cluster-info >/dev/null 2>&1 \
  || die "no reachable cluster (check KUBECONFIG${KUBE_CONTEXT:+ and KUBE_CONTEXT=$KUBE_CONTEXT})"

# Refuse anything but kind. This installs a component that can kill pods cluster-wide,
# and a mistyped context is the whole risk: the guard is here rather than in the
# workflow because the workflow is not the only caller. Chaos Mesh on a cluster
# somebody cares about is their decision to make with their own tooling.
node_images="$("${KUBECTL[@]}" get nodes -o jsonpath='{.items[*].status.nodeInfo.osImage}{" "}{.items[*].spec.providerID}' 2>/dev/null || true)"
case "$node_images" in
  *kind*) : ;;
  *) die "this cluster does not look like kind (nodes: ${node_images:-none}); refusing to install a fault injector" ;;
esac

echo "install-chaos-mesh: installing chart $CHAOS_MESH_VERSION into namespace $NAMESPACE"

"${HELM[@]}" upgrade --install "$RELEASE_NAME" chaos-mesh \
  --repo "$CHAOS_MESH_REPO" \
  --version "$CHAOS_MESH_VERSION" \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --set chaosDaemon.runtime=containerd \
  --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
  --set controllerManager.replicaCount=1 \
  --set dashboard.create=false \
  --set dnsServer.create=false \
  --set dashboard.securityMode=true \
  --wait \
  --timeout "$TIMEOUT"

# `helm --wait` returns when the Deployments report ready, which is not the same as the
# API server being able to serve the custom resources. Both are asserted, because the
# thing that follows is a Go test creating a PodChaos and a race here reads as a flaky
# test rather than as an install that had not finished.
echo "install-chaos-mesh: waiting for the podchaos CRD to be Established"
"${KUBECTL[@]}" wait --for=condition=Established \
  crd/podchaos.chaos-mesh.org --timeout=120s

echo "install-chaos-mesh: waiting for the chaos-controller-manager webhook to answer"
deadline=$(( $(date +%s) + 180 ))
while :; do
  # A dry-run create of an intentionally empty PodChaos: the object is invalid, so
  # nothing is ever created, but reaching a VALIDATION error rather than a connection
  # error is proof the admission webhook is serving. This is the same
  # error-code-versus-outcome distinction the repo learned the hard way about
  # capability probes — what is being tested is "did the request reach the validator",
  # so the assertion is on which failure comes back, not on whether one does.
  out="$("${KUBECTL[@]}" create --dry-run=server -f - <<'EOF' 2>&1 || true
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: maklaude-webhook-probe
  namespace: chaos-mesh
spec: {}
EOF
)"
  case "$out" in
    *"failed calling webhook"*|*"connection refused"*|*"no endpoints available"*|*"context deadline exceeded"*)
      : ;;  # webhook not serving yet
    *)
      echo "install-chaos-mesh: webhook is serving (probe said: ${out%%$'\n'*})"
      break ;;
  esac
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "install-chaos-mesh: webhook did not become ready in time; last probe said:" >&2
    echo "$out" >&2
    "${KUBECTL[@]}" get pods -n "$NAMESPACE" >&2 || true
    exit 1
  fi
  sleep 5
done

"${KUBECTL[@]}" get pods -n "$NAMESPACE"
echo "install-chaos-mesh: Chaos Mesh $CHAOS_MESH_VERSION is installed and serving"
