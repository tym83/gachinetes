#!/bin/bash
# Phase 7 on the stand: contracts, the safeword, spotters, the dungeon,
# leather, lockers. Run after up.sh. People are impersonated.
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
export -f waitfor

# People: alice and bob may make contracts and spots; carol is an admin of
# prod who may grant admin there.
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: gym-member}
rules:
- apiGroups: [gym.gachinetes.dev]
  resources: [contracts, spots]
  verbs: [get, list, create, patch, update]
- apiGroups: [authentication.k8s.io]
  resources: [selfsubjectreviews]
  verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: gym-members}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: gym-member}
subjects:
- {kind: User, name: alice, apiGroup: rbac.authorization.k8s.io}
- {kind: User, name: bob, apiGroup: rbac.authorization.k8s.io}
- {kind: User, name: carol, apiGroup: rbac.authorization.k8s.io}
YAML
mkns prod
# carol may grant admin (the bind verb on it) and delete namespaces; bob
# may delete namespaces too.
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: grant-admin}
rules:
- apiGroups: [rbac.authorization.k8s.io]
  resources: [clusterroles]
  resourceNames: [admin]
  verbs: [bind]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: delete-namespaces}
rules:
- apiGroups: [""]
  resources: [namespaces]
  verbs: [get, delete]
YAML
kubectl create clusterrolebinding carol-grant --clusterrole=grant-admin --user=carol >/dev/null
kubectl create clusterrolebinding carol-delete --clusterrole=delete-namespaces --user=carol >/dev/null
kubectl create clusterrolebinding bob-delete --clusterrole=delete-namespaces --user=bob >/dev/null

echo "== contracts"
check "alice cannot read prod's secrets on her own" bash -c "! kubectl -n prod get secrets --as alice"
check "alice may not grant herself admin" bash -c "! gachictl contract fix-1 --role admin -s prod --for 10m --as alice"
check "alice asks with a spotter" gachictl contract fix-2 --role admin -s prod --for 10m --spotter --as alice
check "the contract waits for its spotter" waitfor 15 bash -c "kubectl get contract fix-2 -o jsonpath='{.status.phase}' | grep -qx Pending"
check "alice cannot spot herself" bash -c "! gachictl spot contract/fix-2 --as alice"
check "bob, who may not grant admin, cannot spot it" bash -c "! gachictl spot contract/fix-2 --as bob"
check "carol spots it" gachictl spot contract/fix-2 --as carol
check "the contract holds" waitfor 15 bash -c "kubectl get contract fix-2 -o jsonpath='{.status.phase}' | grep -qx Active"
check "alice reads prod's secrets now" waitfor 15 kubectl -n prod get secrets --as alice
check "nobody changes its terms" bash -c "! kubectl patch contract fix-2 --type merge -p '{\"spec\":{\"duration\":\"24h\"}}'"
check "a short contract runs out" bash -c "gachictl contract fix-3 --role view -s prod --for 5s && sleep 12 && kubectl get contract fix-3 -o jsonpath='{.status.phase}' | grep -qx Expired"

echo "== the safeword"
gachictl safeword >/dev/null
check "every contract ends" waitfor 15 bash -c "kubectl get contract fix-2 -o jsonpath='{.status.phase}' | grep -qx Revoked"
check "alice is back to what she had" waitfor 15 bash -c "! kubectl -n prod get secrets --as alice"
check "a contract after the safeword holds" bash -c "gachictl contract fix-4 --role view -s prod --for 10m && waitfor 15 bash -c \"kubectl get contract fix-4 -o jsonpath='{.status.phase}' | grep -qx Active\""

echo "== spotters"
kubectl label ns prod gym.gachinetes.dev/spotted=true >/dev/null
check "a spotted namespace is not deleted without a spot" bash -c "! kubectl delete ns prod --wait=false --as bob 2>&1 | tee /dev/stderr | grep -q deleted"
spot=$(gachictl spot delete namespaces/prod --as bob 2>/dev/null | grep -o 'spot/[a-z0-9-]*' | cut -d/ -f2)
check "bob asks for a spot" test -n "$spot"
check "without a spotter it still stands" bash -c "! kubectl delete ns prod --wait=false --as bob"
check "bob cannot approve his own spot" bash -c "! gachictl spot spot/$spot --as bob"
check "alice, who may not delete it, cannot approve" bash -c "! gachictl spot spot/$spot --as alice"
check "carol approves" gachictl spot spot/$spot --as carol
check "only bob, who asked, deletes with it" bash -c "! kubectl delete ns prod --wait=false --as carol"
check "with the spot, it goes" waitfor 15 kubectl delete ns prod --wait=false --as bob

echo "== the dungeon"
mkns cell
kubectl -n cell create deployment web --image=nginx --replicas=1 >/dev/null
waitfor 30 bash -c "[ \$(kubectl -n cell get pods -o name | wc -l) -eq 1 ]"
pod=$(kubectl -n cell get pods -o jsonpath='{.items[0].metadata.name}')
check "into the dungeon" gachictl dungeon pod/$pod -n cell
check "it keeps for a look, out of its controller" bash -c "kubectl -n cell get pod $pod -o jsonpath='{.metadata.labels}' | grep -q dungeon && [ -z \"\$(kubectl -n cell get pod $pod -o jsonpath='{.metadata.ownerReferences}')\" ]"
check "its controller makes a new one" waitfor 30 bash -c "[ \$(kubectl -n cell get pods -l app=web -o name | wc -l) -eq 1 ]"
check "it is closed off" kubectl -n cell get networkpolicy gachinetes-dungeon

echo "== leather"
kubectl apply -f $W/src/gachinetes/deploy/leather.yaml >/dev/null
mkns tough
kubectl label ns tough gym.gachinetes.dev/leather=on >/dev/null
sleep 3
priv='{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx","securityContext":{"privileged":true}}]}}'
check "a privileged pod is refused where leather is on" bash -c "! echo '$priv' | kubectl -n tough apply -f - 2>&1 | tee /dev/stderr | grep -q created"
check "and the refusal says leather" bash -c "echo '$priv' | kubectl -n tough apply -f - 2>&1 | grep -q leather"
check "an ordinary pod is fine" bash -c "kubectl -n tough run ok --image=nginx >/dev/null"
mkns soft
check "elsewhere it is let be" bash -c "echo '$priv' | kubectl -n soft apply -f -"

echo "== lockers"
mkns lockers
kubectl -n lockers create secret generic key --from-literal=k=v >/dev/null
kubectl -n lockers create serviceaccount app >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: Locker
metadata: {name: key, namespace: lockers}
spec: {secret: key, serviceAccounts: [app]}
YAML
check "the locker's account reads its secret" waitfor 20 kubectl -n lockers get secret key --as system:serviceaccount:lockers:app
check "and nothing else" bash -c "kubectl -n lockers create secret generic other --from-literal=a=b >/dev/null; ! kubectl -n lockers get secret other --as system:serviceaccount:lockers:app"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
