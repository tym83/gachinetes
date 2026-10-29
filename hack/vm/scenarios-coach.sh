#!/bin/bash
# Phase 8 on kuberoot VMs, run in the builder after scenarios-mover.sh:
# the coach resizes a working pod in place, up while it lifts and down
# after a light time. It needs metrics-server, put on the cluster here.
set -u
export PATH=/work/bin:$PATH
KC=/work/kube/g.conf
k() { kubectl --kubeconfig $KC -n coached "$@"; }
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -15; fi; }
waitfor() { local secs=$1; shift; for _ in $(seq 1 "$secs"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }
millicores() { k get pod -l app=lifter -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' | awk '/m$/ {sub("m", ""); print; next} {print $1 * 1000}'; }
export -f waitfor k millicores
export KC

echo "== metrics"
MS=${METRICS_SERVER_VERSION:-v0.8.0}
curl -fsSL "https://github.com/kubernetes-sigs/metrics-server/releases/download/$MS/components.yaml" |
  sed 's/- --metric-resolution=15s/- --metric-resolution=15s\n        - --kubelet-insecure-tls/' |
  kubectl --kubeconfig $KC apply -f - >/dev/null
check "metrics-server serves pod metrics" waitfor 300 kubectl --kubeconfig $KC top pods -A

echo "== the coach"
kubectl --kubeconfig $KC create namespace coached >/dev/null 2>&1
cat <<'EOF' | k apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: lifter}
spec:
  replicas: 1
  selector: {matchLabels: {app: lifter}}
  template:
    metadata: {labels: {app: lifter}}
    spec:
      containers:
      - name: lift
        image: busybox:1.37
        # Lifts while /tmp/rest is not there.
        command: [sh, -c, "while true; do if [ -e /tmp/rest ]; then sleep 1; else :; fi; done"]
        resources: {requests: {cpu: 100m}, limits: {cpu: "2"}}
---
apiVersion: gym.gachinetes.dev/v1
kind: TrainingPlan
metadata: {name: lifter}
spec:
  deployment: lifter
  container: lift
  cpu: {min: 50m, max: 1500m}
  step: 0.5
  rest: 20s
  deloadAfter: 1m
EOF
check "the lifter works" waitfor 120 bash -c "k get pod -l app=lifter -o jsonpath='{.items[0].status.phase}' | grep -qx Running"
pod=$(k get pod -l app=lifter -o jsonpath='{.items[0].metadata.name}')
uid=$(k get pod "$pod" -o jsonpath='{.metadata.uid}')
check "heavy sets raise the request" waitfor 300 bash -c "[ \$(millicores) -ge 200 ]"
check "and raise it again after the rest" waitfor 300 bash -c "[ \$(millicores) -ge 300 ]"
echo "      request now $(millicores)m; $(k get trainingplan lifter -o jsonpath='{.status.why.cpu}')"
check "in place: the same pod, never restarted" bash -c "[ \"\$(k get pod -l app=lifter -o jsonpath='{.items[0].metadata.uid} {.items[0].status.containerStatuses[0].restartCount}')\" = \"$uid 0\" ]"
check "the runtime gives the container what the coach asked" bash -c \
  "k get pod $pod -o jsonpath='{.status.containerStatuses[0].resources.requests.cpu}' | grep -q ."
peak=$(millicores)
k exec "$pod" -- touch /tmp/rest
check "a light time deloads the request" waitfor 420 bash -c "[ \$(millicores) -lt $peak ]"
echo "      request now $(millicores)m after $peak m; $(k get trainingplan lifter -o jsonpath='{.status.why.cpu}')"
check "the personal record is kept" bash -c "k get trainingplan lifter -o jsonpath='{.status.records.cpu.peak}' | grep -q '[1-9]'"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
