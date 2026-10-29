#!/bin/bash
# Phase 8 on the stand: sparring, Aniki, away matches. Run after up.sh;
# kwok plays the nodes, gachi-scheduler schedules, a second empty cluster
# is the away gym. The coach needs metrics-server: it is tried on the VMs.
set -u
W=/work
R=${RUN:-$W/run}
export PATH=$W/bin:$PATH
export KUBECONFIG=$R/kube/super-admin.conf
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -15; fi; }
waitfor() { local secs=$1; shift; for _ in $(seq 1 "$secs"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }
mkns() { kubectl create ns "$1" >/dev/null && waitfor 30 kubectl -n "$1" get serviceaccount default >/dev/null; }
node() { # NAME CPU [KUBECONFIG]
  kubectl --kubeconfig "${3:-$KUBECONFIG}" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Node
metadata: {name: $1, labels: {kubernetes.io/hostname: $1, type: kwok}}
YAML
  kubectl --kubeconfig "${3:-$KUBECONFIG}" patch node "$1" --subresource=status --type=merge -p "{\"status\":{\"capacity\":{\"cpu\":\"$2\",\"memory\":\"8Gi\",\"pods\":\"110\"},\"allocatable\":{\"cpu\":\"$2\",\"memory\":\"8Gi\",\"pods\":\"110\"}}}" >/dev/null
}
export -f waitfor

node home-1 2
pgrep -f "^$W/bin/kwok" >/dev/null || setsid nohup $W/bin/kwok --kubeconfig=$KUBECONFIG --manage-all-nodes=true --server-address=127.0.0.1:10247 >$R/logs/kwok.log 2>&1 </dev/null &
pgrep -f "^$W/bin/gachi-scheduler" >/dev/null || setsid nohup $W/bin/gachi-scheduler --gym-kubeconfig=$KUBECONFIG --secure-port=10259 >$R/logs/gachi-scheduler.log 2>&1 </dev/null &
check "the home node is Ready" waitfor 60 bash -c "kubectl get node home-1 | grep -q ' Ready'"

deploy() { # NS NAME REPLICAS CPU
kubectl apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: $2, namespace: $1}
spec:
  replicas: $3
  selector: {matchLabels: {app: $2}}
  template:
    metadata: {labels: {app: $2}}
    spec:
      tolerations: [{operator: Exists}]
      containers: [{name: c, image: nginx:1.27, resources: {requests: {cpu: "$4"}}}]
YAML
}

echo "== sparring"
mkns ring
deploy ring tough 2 100m
check "the service is ready" waitfor 60 bash -c "[ \"\$(kubectl -n ring get deploy tough -o jsonpath='{.status.readyReplicas}')\" = 2 ]"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: Sparring
metadata: {name: tough, namespace: ring}
spec: {deployment: tough, every: 15s, recoverWithin: 30s}
YAML
check "it takes punches and gets up" waitfor 90 bash -c "[ \"\$(kubectl -n ring get sparring tough -o jsonpath='{.status.wins}')\" -ge 2 ]"
check "it is ranked" bash -c "kubectl -n ring get sparring tough -o jsonpath='{.status.rank}' | grep -qvx Unranked"
kubectl -n ring get sparring tough
kubectl -n ring delete sparring tough >/dev/null

echo "== Aniki"
cat > $R/sloppy.yaml <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: sloppy}
spec:
  selector: {matchLabels: {app: sloppy}}
  template:
    metadata: {labels: {app: sloppy}}
    spec:
      tolerations: [{operator: Exists}]
      containers: [{name: c, image: nginx}]
YAML
check "Aniki reviews a manifest" bash -c "gachictl aniki -f $R/sloppy.yaml | grep -q 'right version'"
mkns mentored
kubectl label ns mentored gym.gachinetes.dev/aniki=on >/dev/null
sleep 3
check "Aniki speaks up when it is applied where asked" bash -c "kubectl -n mentored apply -f $R/sloppy.yaml 2>&1 | grep -q 'Warning: ♂ Aniki'"
mkns quiet
check "and keeps quiet elsewhere" bash -c "! kubectl -n quiet apply -f $R/sloppy.yaml 2>&1 | grep -q Aniki"
check "Aniki talks a failure through" bash -c "gachictl aniki why deployment/sloppy -n mentored | grep -q 'Sorry for what?'"

echo "== away match"
setsid nohup $W/bin/gachi-fantasy --empty --work $R/away --bin $W/bin --port-base 24000 >$R/logs/away.log 2>&1 </dev/null &
AWAY=$R/away/kube/super-admin.conf
check "the away gym is up" waitfor 120 kubectl --kubeconfig $AWAY get --raw /readyz
node away-1 8 $AWAY
check "its node is Ready" waitfor 60 bash -c "kubectl --kubeconfig $AWAY get node away-1 | grep -q ' Ready'"
mkns team
kubectl -n team create secret generic away --from-file=kubeconfig=$AWAY >/dev/null
deploy team squad 3 1
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: AwayMatch
metadata: {name: squad, namespace: team}
spec: {deployment: squad, kubeconfigSecret: away, after: 15s}
YAML
check "two play at home, one finds no place" waitfor 60 bash -c "[ \$(kubectl -n team get pods -l app=squad --field-selector=status.phase=Running -o name | wc -l) -eq 2 ]"
check "one goes to play away" waitfor 90 bash -c "[ \"\$(kubectl --kubeconfig $AWAY -n team get deploy squad-away -o jsonpath='{.status.readyReplicas}')\" = 1 ]"
kubectl -n team scale deployment squad --replicas=2 >/dev/null
check "with room at home, the away player comes home" waitfor 90 bash -c "[ \"\$(kubectl --kubeconfig $AWAY -n team get deploy squad-away -o jsonpath='{.spec.replicas}')\" = 0 ]"
kubectl -n team get awaymatch squad

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
