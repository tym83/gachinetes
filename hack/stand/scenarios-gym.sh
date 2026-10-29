#!/bin/bash
# Phase 3 on the stand: bouts, belts, why. Run after up.sh.
set -u
W=/work
R=${RUN:-$W/run}
export PATH=$W/bin:$PATH
export KUBECONFIG=$R/kube/super-admin.conf
IP=$(hostname -i | awk '{print $1}')
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -15; fi; }
waitfor() { local secs=$1; shift; for _ in $(seq 1 "$secs"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }
g() { gachictl "$@"; }
export -f g waitfor

kubectl create ns gym >/dev/null 2>&1
kubectl -n gym create deployment war --image=nginx:1.27 --replicas=1 >/dev/null
kubectl -n gym create deployment solo --image=nginx:1.27 --replicas=1 >/dev/null

echo "== a bout"
for i in 1 2 3 4 5 6; do
  if [ $((i % 2)) -eq 1 ]; then m=hpa; r=5; else m=argocd; r=2; fi
  kubectl -n gym patch deployment war --field-manager=$m -p "{\"spec\":{\"replicas\":$r}}" >/dev/null
  kubectl -n gym patch deployment solo --field-manager=hpa -p "{\"spec\":{\"replicas\":$i}}" >/dev/null
  sleep 2
done
check "the referee calls the bout" bash -c "waitfor 40 bash -c \"kubectl get bouts -o json | grep -q '\\\"path\\\": \\\"spec.replicas\\\"'\""
check "both fighters are in it" bash -c "g bouts | grep war | grep -q 'argocd ×3 vs hpa ×3'"
check "one manager alone makes no bout" bash -c "! g bouts | grep -q solo"
check "the object gets a Wrestling event" bash -c "kubectl -n gym get events --field-selector reason=Wrestling,involvedObject.name=war -o name | grep -q ."
check "the bout is over after the window" bash -c "waitfor 120 bash -c 'g bouts | grep war | grep -q Over'"

echo "== belts"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: BeltPolicy
metadata: {name: war-replicas}
spec:
  match: {group: apps, kind: Deployment, namespaces: [gym], names: [war]}
  belts:
  - path: spec.replicas
    holders: {managers: [hpa]}
  - path: spec.template.spec.containers
    holders: {users: [system:serviceaccount:ci:deployer]}
    action: Warn
YAML
check "the referee configures the webhook" bash -c "waitfor 20 kubectl get validatingwebhookconfiguration gachinetes-belts"
check "gachictl belts lists the holder" bash -c "g belts | grep war-replicas | grep -q 'manager hpa'"
check "a challenger is refused" bash -c "! kubectl -n gym patch deployment war --field-manager=argocd -p '{\"spec\":{\"replicas\":9}}' 2>&1 | tee /dev/stderr | grep -q patched"
check "the refusal names the holder" bash -c "kubectl -n gym patch deployment war --field-manager=argocd -p '{\"spec\":{\"replicas\":9}}' 2>&1 | grep -q 'belongs to manager hpa'"
check "the holder writes" bash -c "kubectl -n gym patch deployment war --field-manager=hpa -p '{\"spec\":{\"replicas\":3}}'"
check "a warned belt lets the write through with a warning" bash -c "kubectl -n gym set image deployment/war nginx=nginx:1.28 2>&1 | grep -q 'Warning: ♂'"
check "other objects are not held" bash -c "kubectl -n gym patch deployment solo --field-manager=argocd -p '{\"spec\":{\"replicas\":4}}'"
pkill -f "^$W/bin/gachi-referee"; while pgrep -f "^$W/bin/gachi-referee" >/dev/null; do sleep 1; done
check "a referee that is down lets writes through" bash -c "kubectl -n gym patch deployment war --field-manager=argocd -p '{\"spec\":{\"replicas\":8}}'"
setsid nohup $W/bin/gachi-referee --kubeconfig $KUBECONFIG --listen $IP:9443 --webhook-url https://$IP:9443/belts --window 60s \
  >>$R/logs/gachi-referee.log 2>&1 </dev/null &
check "back up, it refuses again" bash -c "waitfor 30 bash -c \"kubectl -n gym patch deployment war --field-manager=argocd -p '{\\\"spec\\\":{\\\"replicas\\\":7}}' 2>&1 | grep -q 'belongs to'\""
kubectl delete beltpolicy war-replicas >/dev/null
check "without policies the belts' webhook goes" bash -c "waitfor 20 bash -c '! kubectl get validatingwebhookconfiguration gachinetes-belts -o jsonpath={.webhooks[*].name} | grep -q belts'"

echo "== why"
pod=$(kubectl -n gym get pods -l app=war --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')
check "why a pod: its replica set, and the deployment's change before it" bash -c \
  "g why pod/$pod -n gym | tee /dev/stderr | grep -q 'ReplicaSet' && g why pod/$pod -n gym | grep -q 'Deployment gym/war made it.*after .*changed by'"
kubectl -n gym create configmap gone --from-literal=a=b >/dev/null; sleep 1; kubectl -n gym delete configmap gone >/dev/null
check "why a deleted object points to undelete" bash -c "g why configmap/gone -n gym | grep -q 'gachictl undelete'"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
