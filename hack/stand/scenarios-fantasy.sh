#!/bin/bash
# Phase 4 on the stand: deep dark fantasies. Run after up.sh.
set -u
W=/work
R=${RUN:-$W/run}
export PATH=$W/bin:$PATH
export KUBECONFIG=$R/kube/super-admin.conf
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -20; fi; }

echo "== a cluster with two nodes of 4 CPUs and a workload"
for n in n1 n2; do
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Node
metadata:
  name: $n
  labels: {kubernetes.io/hostname: $n, type: kwok}
status:
  capacity: {cpu: "4", memory: 8Gi, pods: "110"}
  allocatable: {cpu: "4", memory: 8Gi, pods: "110"}
YAML
kubectl patch node $n --subresource=status --type=merge -p '{"status":{"capacity":{"cpu":"4","memory":"8Gi","pods":"110"},"allocatable":{"cpu":"4","memory":"8Gi","pods":"110"}}}' >/dev/null
done
kubectl create ns dream >/dev/null 2>&1
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: base, namespace: dream}
spec:
  replicas: 2
  selector: {matchLabels: {app: base}}
  template:
    metadata: {labels: {app: base}}
    spec:
      containers: [{name: c, image: nginx:1.27, resources: {requests: {cpu: "1"}}}]
YAML
sleep 5
cat > $R/big.yaml <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: big, namespace: dream}
spec:
  replicas: 3
  selector: {matchLabels: {app: big}}
  template:
    metadata: {labels: {app: big}}
    spec:
      containers: [{name: c, image: nginx:1.27, resources: {requests: {cpu: "3"}}}]
YAML

echo "== the fantasy"
gachi-fantasy -f $R/big.yaml --work $R/fantasy-1 --bin $W/bin --until-quiet 15s --timeout 5m --report $R/fantasy-1.json \
  >$R/fantasy-1.txt 2>$R/fantasy-1.log
rc=$?
cat $R/fantasy-1.txt
check "the fantasy plays out" test $rc -eq 0
check "the deployment would be made" grep -q "+ Deployment dream/big" $R/fantasy-1.txt
check "its replica set too" grep -q "+ ReplicaSet dream/big-" $R/fantasy-1.txt
check "three pods would come" bash -c "[ \$(grep -c '+ Pod dream/big-' $R/fantasy-1.txt) -eq 3 ]"
check "two would land on nodes" bash -c "[ \$(grep -E '\\+ Pod dream/big-.* → n[12]' $R/fantasy-1.txt | wc -l) -eq 2 ]"
check "one would find no place" bash -c "grep -A3 'Would find no place (1)' $R/fantasy-1.txt | grep -q 'Pod dream/big-'"
check "the real cluster did not change" bash -c "! kubectl -n dream get deployment big"
check "the fantasy stopped" bash -c "! pgrep -f '^/work/bin/kuberoot-store --name fantasy'"

echo "== a fantasy from the past"
before=$(kubectl get --raw /apis/tape.gachinetes.dev/v1/info | grep -o '"current":[0-9]*' | cut -d: -f2)
kubectl -n dream scale deployment base --replicas=4 >/dev/null
sleep 3
gachi-fantasy -f $R/big.yaml --at $before --work $R/fantasy-2 --bin $W/bin --until-quiet 15s --timeout 5m \
  >$R/fantasy-2.txt 2>$R/fantasy-2.log
cat $R/fantasy-2.txt
check "branched from the revision asked" grep -q "from revision $before" $R/fantasy-2.txt
check "with base at 2, one big pod finds no place, as before" grep -q "Would find no place (1)" $R/fantasy-2.txt

echo "== merging"
cat > $R/base-3.yaml <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: base, namespace: dream}
spec:
  replicas: 3
  selector: {matchLabels: {app: base}}
  template:
    metadata: {labels: {app: base}}
    spec:
      containers: [{name: c, image: nginx:1.27, resources: {requests: {cpu: "1"}}}]
YAML
gachi-fantasy -f $R/base-3.yaml --at $before --merge --work $R/fantasy-3 --bin $W/bin --until-quiet 15s --timeout 5m \
  >$R/fantasy-3.txt 2>$R/fantasy-3.log
rc=$?
check "a merge over what the cluster wrote since is refused" bash -c "[ $rc -ne 0 ] && grep -q 'the cluster moved on' $R/fantasy-3.log"
check "and the cluster keeps its own" bash -c "[ \$(kubectl -n dream get deploy base -o jsonpath='{.spec.replicas}') -eq 4 ]"
gachi-fantasy -f $R/big.yaml --merge --work $R/fantasy-4 --bin $W/bin --until-quiet 15s --timeout 5m \
  >$R/fantasy-4.txt 2>$R/fantasy-4.log
check "a clean merge applies the change" bash -c "kubectl -n dream get deployment big && grep -q 'merged' $R/fantasy-4.txt"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
