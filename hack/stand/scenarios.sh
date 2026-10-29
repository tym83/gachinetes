#!/bin/bash
# Every tape scenario on the stand (up.sh), end to end. Prints PASS/FAIL per
# check and exits non-zero when any failed.
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
info() { kubectl get --raw /apis/tape.gachinetes.dev/v1/info; }
num() { info | grep -o "\"$1\":[0-9]*" | cut -d: -f2; }
g() { gachictl "$@"; }
export -f g info num

echo "== the aggregated API"
until kubectl get apiservice v1.tape.gachinetes.dev -o jsonpath='{.status.conditions[0].status}' | grep -q True; do sleep 1; done
check "APIService available" kubectl get --raw /apis/tape.gachinetes.dev/v1
check "direct call without the API server's certificate is refused" \
  bash -c "curl -sk -o /dev/null -w '%{http_code}' https://$IP:9282/apis/tape.gachinetes.dev/v1/info | grep -qx 401"

echo "== access"
kubectl create clusterrole demo-reader --verb=get --resource=configmaps >/dev/null 2>&1
check "a user without rights is refused" bash -c "! g tape --as alice"
kubectl create clusterrolebinding alice-tape --clusterrole=gachinetes:tape-reader --user=alice >/dev/null 2>&1
check "a tape reader reads" g tape --as alice
check "a tape reader may not reveal Secrets" bash -c "! kubectl get --raw '/apis/tape.gachinetes.dev/v1/graveyard?reveal=1' --as alice"

echo "== a namespace to lose"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: web, image: nginx:1.27}]}
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}
spec: {selector: {app: web}, ports: [{port: 80}]}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: app, namespace: shop}
data: {greeting: "boy next door"}
---
apiVersion: v1
kind: Secret
metadata: {name: db, namespace: shop}
stringData: {password: three-hundred-bucks}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: belts.gym.gachinetes.dev}
spec:
  group: gym.gachinetes.dev
  scope: Namespaced
  names: {plural: belts, singular: belt, kind: Belt}
  versions:
  - name: v1
    served: true
    storage: true
    schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: boss-of-this-gym}
rules: [{apiGroups: [""], resources: [pods], verbs: [get]}]
YAML
until kubectl get crd belts.gym.gachinetes.dev -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' | grep -q True; do sleep 1; done
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gym.gachinetes.dev/v1
kind: Belt
metadata: {name: champion, namespace: shop}
spec: {holder: billy}
YAML
until [ "$(kubectl -n shop get rs -o name | wc -l)" -ge 1 ]; do sleep 1; done
check "the controller made the deployment's replica set" kubectl -n shop get rs

echo "== the tape starts with a compaction (every minute here)"
start_rev=$(num current)
until [ "$(num compacted)" -gt "$start_rev" ]; do sleep 5; done
check "the tape has started and the store compacted past the objects" bash -c "info | grep -q '\"started\":true'"

echo "== reading the past"
sleep 2
T1=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 2
kubectl -n shop patch configmap app --type merge -p '{"data":{"greeting":"deep dark fantasies"}}' >/dev/null
until [ "$(num compacted)" -gt "$(kubectl -n shop get cm app -o jsonpath='{.metadata.resourceVersion}')" ]; do sleep 5; done
check "the etcd API refuses the compacted past" bash -c "kubectl get --raw '/api/v1/namespaces/shop/configmaps?resourceVersion=$start_rev&resourceVersionMatch=Exact' 2>&1 | grep -qi 'too old\|compacted\|gone'"
check "get --at reads the old value from the archive" bash -c "g get configmap/app -n shop --at $T1 -o yaml | grep -q 'boy next door'"
check "get without --at is refused" bash -c "! g get configmap -n shop"
check "rewind shows the change" bash -c "g rewind -n shop --to $T1 | grep -q 'changed.*ConfigMap.*app'"
check "rewind with a duration ago works" bash -c "g rewind -n shop --to 30s | grep -q REW"
check "history lists both versions" bash -c "[ \$(g history configmap/app -n shop | grep -c written) -ge 2 ]"
check "a moment before the tape is refused" bash -c "! g get configmap -n shop --at 2020-01-01T00:00:00Z"
check "Secrets are redacted for a tape reader" bash -c "! kubectl get --raw '/apis/tape.gachinetes.dev/v1/objects?namespace=shop&kind=Secret' --as alice | grep -q dGhyZWU"

echo "== undelete one object"
kubectl -n shop delete configmap app >/dev/null
check "the graveyard has it" bash -c "g graveyard configmap -n shop | grep -q app"
check "undelete brings it back with its last data" bash -c "g undelete configmap/app -n shop && kubectl -n shop get cm app -o jsonpath='{.data.greeting}' | grep -qx 'deep dark fantasies'"
check "a key written again leaves the graveyard" bash -c "! g graveyard configmap -n shop | grep -qw app"
check "undelete of a live object finds nothing to bring back" bash -c "g undelete configmap/app -n shop 2>&1 | grep -q 'not in the graveyard'"

echo "== undelete a cluster-scoped object"
kubectl delete clusterrole boss-of-this-gym >/dev/null
check "undelete a ClusterRole" bash -c "g undelete clusterrole/boss-of-this-gym && kubectl get clusterrole boss-of-this-gym"

echo "== undelete a custom resource (stored as JSON)"
kubectl -n shop delete belt champion >/dev/null
check "undelete a custom resource" bash -c "g undelete belt/champion -n shop && kubectl -n shop get belt champion -o jsonpath='{.spec.holder}' | grep -qx billy"

echo "== lose the whole namespace"
svc_ip=$(kubectl -n shop get svc web -o jsonpath='{.spec.clusterIP}')
kubectl delete ns shop --wait=false >/dev/null
until ! kubectl get ns shop >/dev/null 2>&1; do sleep 2; done
check "the graveyard holds the namespace" bash -c "g graveyard -n shop | grep -q Namespace"
check "dry run plans without changing anything" bash -c "g undelete ns/shop --dry-run | grep -q Deployment && ! kubectl get ns shop"
check "a tape reader may not undelete Secrets" bash -c "! g undelete ns/shop --as alice"
check "undelete the namespace" g undelete ns/shop
check "deployment back" kubectl -n shop get deploy web
check "its replica set made again by the controller, one" bash -c "until [ \$(kubectl -n shop get rs -o name | wc -l) -eq 1 ]; do sleep 1; done"
check "service back with a new cluster IP" bash -c "kubectl -n shop get svc web -o jsonpath='{.spec.clusterIP}' | grep -v '^$svc_ip\$'"
check "secret back with its data" bash -c "kubectl -n shop get secret db -o jsonpath='{.data.password}' | base64 -d | grep -qx three-hundred-bucks"
check "configmap back" bash -c "kubectl -n shop get cm app -o jsonpath='{.data.greeting}' | grep -q 'deep dark'"
check "custom resource back" kubectl -n shop get belt champion
check "one default service account, not two" bash -c "[ \$(kubectl -n shop get sa default -o name | wc -l) -eq 1 ]"

echo "== the tape survives a restart of the store"
before=$(g history configmap/app -n shop | wc -l)
pkill -f "^$W/bin/kuberoot-store"; sleep 2
setsid nohup $W/bin/kuberoot-store --name s1 --data-dir $R/store --listen-client-urls http://127.0.0.1:2379 \
  --listen-peer-urls http://127.0.0.1:2380 --keep-history --listen-tape-urls http://127.0.0.1:2382 \
  >>$R/logs/kuberoot-store.log 2>&1 </dev/null &
until kubectl get --raw /readyz >/dev/null 2>&1 && info >/dev/null 2>&1; do sleep 2; done
check "history after a restart" bash -c "[ \$(g history configmap/app -n shop | wc -l) -eq $before ]"

echo "== a snapshot carries the tape"
if [ ! -x $W/bin/etcdctl ]; then
  curl -fsSL https://github.com/etcd-io/etcd/releases/download/v3.6.5/etcd-v3.6.5-linux-amd64.tar.gz | tar -xz -C /tmp
  cp /tmp/etcd-v3.6.5-linux-amd64/etcdctl $W/bin/
fi
snap=$R/snap.db
etcdctl --endpoints http://127.0.0.1:2379 snapshot save $snap >/dev/null 2>&1
restored=$R/restored
kuberoot-store snapshot restore $snap --data-dir $restored --name r1 --initial-cluster r1=http://127.0.0.1:12380 >/dev/null 2>&1
setsid nohup $W/bin/kuberoot-store --name r1 --data-dir $restored --listen-client-urls http://127.0.0.1:12379 \
  --listen-peer-urls http://127.0.0.1:12380 --keep-history --listen-tape-urls http://127.0.0.1:12382 \
  >$R/logs/restored.log 2>&1 </dev/null &
until curl -sf http://127.0.0.1:12382/tape/v1/info >/dev/null; do sleep 1; done
check "the restored store's graveyard keeps the namespace's past" bash -c \
  "curl -sf 'http://127.0.0.1:12382/tape/v1/history?key=/registry/namespaces/shop' | grep -q deleted"
pkill -f "^$W/bin/kuberoot-store --name r1"

echo "== retention cuts the tape"
from_before=$(num from)
pkill -f "^$W/bin/kuberoot-store --name s1"; while pgrep -f "^$W/bin/kuberoot-store --name s1" >/dev/null; do sleep 1; done
setsid nohup $W/bin/kuberoot-store --name s1 --data-dir $R/store --listen-client-urls http://127.0.0.1:2379 \
  --listen-peer-urls http://127.0.0.1:2380 --keep-history --history-retention=30s --listen-tape-urls http://127.0.0.1:2382 \
  >>$R/logs/kuberoot-store.log 2>&1 </dev/null &
until kubectl get --raw /readyz >/dev/null 2>&1 && info >/dev/null 2>&1; do sleep 2; done
kubectl -n shop patch configmap app --type merge -p '{"data":{"greeting":"ass we can"}}' >/dev/null
until [ "$(num from)" -gt "$from_before" ]; do sleep 5; done
check "the tape starts later after a cut" sh -c "[ $(num from) -gt $from_before ]"
check "a moment before the cut is refused" bash -c "! g get configmap/app -n shop --at $T1"
check "now still reads" bash -c "g get configmap/app -n shop --at 1s -o yaml | grep -q 'ass we can'"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
