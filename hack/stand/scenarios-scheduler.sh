#!/bin/bash
# Phase 5 on the stand: weight classes, tag teams, boys next door, budgets.
# Run after up.sh; kwok plays two nodes and gachi-scheduler schedules.
set -u
W=/work
R=${RUN:-$W/run}
export PATH=$W/bin:$PATH
export KUBECONFIG=$R/kube/super-admin.conf
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -20; fi; }
waitfor() { local secs=$1; shift; for _ in $(seq 1 "$secs"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }
nodeof() { kubectl -n "$1" get pod "$2" -o jsonpath='{.spec.nodeName}'; }
export -f waitfor nodeof

for n in n-a n-b; do
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Node
metadata: {name: $n, labels: {kubernetes.io/hostname: $n, type: kwok}}
YAML
kubectl patch node $n --subresource=status --type=merge -p '{"status":{"capacity":{"cpu":"4","memory":"8Gi","pods":"110"},"allocatable":{"cpu":"4","memory":"8Gi","pods":"110"}}}' >/dev/null
done
pgrep -f "^$W/bin/kwok" >/dev/null || setsid nohup $W/bin/kwok --kubeconfig=$KUBECONFIG --manage-all-nodes=true --server-address=127.0.0.1:10247 \
  >$R/logs/kwok.log 2>&1 </dev/null &
cat > $R/scheduler.yaml <<YAML
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection: {kubeconfig: $KUBECONFIG}
leaderElection: {leaderElect: false}
profiles:
- schedulerName: default-scheduler
  plugins:
    preFilter: {enabled: [{name: TagTeam}]}
    filter: {enabled: [{name: WeightClass}]}
    permit: {enabled: [{name: TagTeam}]}
    score: {enabled: [{name: BoyNextDoor, weight: 5}]}
YAML
pgrep -f "^$W/bin/gachi-scheduler" >/dev/null || setsid nohup $W/bin/gachi-scheduler --config=$R/scheduler.yaml --secure-port=10259 \
  >$R/logs/gachi-scheduler.log 2>&1 </dev/null &
check "kwok makes the nodes Ready" waitfor 60 bash -c "kubectl get nodes n-a n-b | grep -c ' Ready' | grep -qx 2"

run() { # run NS NAME CPU [LABELS] [ANNOTATIONS]
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $2
  namespace: $1
  labels: {${4:-}}
  annotations: {${5:-}}
spec:
  tolerations: [{operator: Exists}]
  containers: [{name: c, image: nginx, resources: {requests: {cpu: "$3"}}}]
YAML
}

echo "== weight classes"
kubectl create ns weights >/dev/null
run weights sh1 1 "gym.gachinetes.dev/weight: super-heavyweight"
check "a super-heavyweight lands" waitfor 30 bash -c "[ -n \"\$(nodeof weights sh1)\" ]"
run weights p1 100m
check "an ordinary pod lands on the other node" waitfor 30 bash -c "[ -n \"\$(nodeof weights p1)\" ] && [ \"\$(nodeof weights p1)\" != \"\$(nodeof weights sh1)\" ]"
run weights sh2 1 "gym.gachinetes.dev/weight: super-heavyweight"
check "a second super-heavyweight finds no node of its own" waitfor 30 bash -c \
  "kubectl -n weights get events --field-selector involvedObject.name=sh2,reason=FailedScheduling -o jsonpath='{.items[*].message}' | grep -q '♂'"
kubectl delete ns weights --wait=false >/dev/null

echo "== tag teams"
kubectl create ns teams >/dev/null
for i in 1 2 3; do run teams big-$i 3 "gym.gachinetes.dev/team: big" "gym.gachinetes.dev/team-size: \"3\""; done
sleep 20
check "a team that cannot all fit does not go in at all" bash -c \
  "[ \$(kubectl -n teams get pods -l gym.gachinetes.dev/team=big -o jsonpath='{range .items[*]}{.spec.nodeName}{\"\\n\"}{end}' | grep -c .) -eq 0 ]"
kubectl -n teams delete pods -l gym.gachinetes.dev/team=big --wait=false >/dev/null
for i in 1 2; do run teams duo-$i 3 "gym.gachinetes.dev/team: duo" "gym.gachinetes.dev/team-size: \"2\""; done
check "a team that fits goes in together" waitfor 60 bash -c \
  "[ \$(kubectl -n teams get pods -l gym.gachinetes.dev/team=duo -o jsonpath='{range .items[*]}{.spec.nodeName}{\"\\n\"}{end}' | grep -c .) -eq 2 ]"
run teams trio-1 100m "gym.gachinetes.dev/team: trio" "gym.gachinetes.dev/team-size: \"3\""
run teams trio-2 100m "gym.gachinetes.dev/team: trio" "gym.gachinetes.dev/team-size: \"3\""
check "a team still coming waits, and says how many are in" waitfor 30 bash -c \
  "kubectl -n teams get events --field-selector involvedObject.name=trio-1,reason=FailedScheduling -o jsonpath='{.items[*].message}' | grep -q '2 of 3 in the gym'"
run teams trio-3 100m "gym.gachinetes.dev/team: trio" "gym.gachinetes.dev/team-size: \"3\""
check "the third member brings the team in" waitfor 90 bash -c \
  "[ \$(kubectl -n teams get pods -l gym.gachinetes.dev/team=trio -o jsonpath='{range .items[*]}{.spec.nodeName}{\"\\n\"}{end}' | grep -c .) -eq 3 ]"
kubectl delete ns teams --wait=false >/dev/null

echo "== boys next door"
kubectl create ns door >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: api, namespace: door, labels: {app: api}}
spec:
  nodeName: n-b
  tolerations: [{operator: Exists}]
  containers: [{name: c, image: nginx, resources: {requests: {cpu: 100m}}}]
YAML
for i in 1 2 3; do run door web-$i 100m "app: web" "gym.gachinetes.dev/talks-to: app=api"; done
check "pods land next to the peer they talk to" waitfor 30 bash -c \
  "[ \"\$(nodeof door web-1)\$(nodeof door web-2)\$(nodeof door web-3)\" = n-bn-bn-b ]"
kubectl delete ns door --wait=false >/dev/null

echo "== three hundred bucks"
kubectl create ns cheap >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: PriceList
metadata: {name: gym}
spec: {currency: "$", cpu: 0.04, memoryGi: 0.005}
---
apiVersion: gym.gachinetes.dev/v1
kind: Budget
metadata: {name: monthly, namespace: cheap}
spec: {monthly: 100}
YAML
check "the referee prices pods once there is a budget" waitfor 20 bash -c \
  "kubectl get validatingwebhookconfiguration gachinetes-belts -o jsonpath='{.webhooks[*].name}' | grep -q bucks"
kubectl -n cheap create deployment spend --image=nginx --replicas=3 >/dev/null
kubectl -n cheap set resources deployment spend --requests=cpu=1 >/dev/null
check "three replicas fit the budget" waitfor 60 bash -c "[ \$(kubectl -n cheap get pods -o name | wc -l) -eq 3 ]"
kubectl -n cheap scale deployment spend --replicas=4 >/dev/null
check "the fourth is refused, and says why" waitfor 60 bash -c \
  "kubectl -n cheap get events --field-selector reason=FailedCreate -o jsonpath='{.items[*].message}' | grep -q 'three hundred bucks'"
check "gachictl bill shows the namespace" bash -c "gachictl bill | grep cheap | grep -q '\$100.00'"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
