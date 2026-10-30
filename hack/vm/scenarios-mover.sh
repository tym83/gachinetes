#!/bin/bash
# Phase 6 on kuberoot VMs, run in the builder: g1 a control plane, g2 and
# g3 join as workers; live migration and rounds of a pod that counts in
# memory. G1, G2, G3: the VMs' addresses.
set -u
export PATH=/work/bin:$PATH
NODEAPI="kubectl --kubeconfig /work/trust/admin.kubeconfig"
KC=/work/kube/g.conf
k() { kubectl --kubeconfig $KC -n default "$@"; }
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/tmp/check.out 2>&1; then pass "$what"; else fail "$what"; sed 's/^/      /' /tmp/check.out | tail -15; fi; }
waitfor() { local secs=$1; shift; for _ in $(seq 1 "$secs"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }
export -f waitfor k
export KC

echo "== a cluster of three"
for ip in $G1 $G2 $G3; do waitfor 300 $NODEAPI --server https://$ip:50000 get osconfigs >/dev/null; done
mkdir -p /work/kube
$NODEAPI --server https://$G1:50000 get kubeconfig admin -o jsonpath='{.kubeconfig}' > $KC
waitfor 300 kubectl --kubeconfig $KC get namespace kube-system
for ip in $G2 $G3; do
  name=$($NODEAPI --server https://$ip:50000 get osconfigs -o jsonpath='{.items[0].metadata.name}')
  printf 'apiVersion: node.kuberoot.dev/v1alpha1\nkind: JoinTicket\nmetadata: {name: %s}\nspec: {nodeName: %s, role: Worker, address: %s}\n' "$name" "$name" "$ip" |
    $NODEAPI --server https://$G1:50000 apply -f - >/dev/null
  waitfor 30 bash -c "$NODEAPI --server https://$G1:50000 get jointicket $name -o jsonpath='{.status.membership}' | grep -q Membership"
  $NODEAPI --server https://$G1:50000 get jointicket "$name" -o jsonpath='{.status.membership}' | $NODEAPI --server https://$ip:50000 apply -f - >/dev/null
done
check "three nodes Ready" waitfor 300 bash -c "[ \$(kubectl --kubeconfig $KC get nodes | grep -c ' Ready') -eq 3 ]"
kubectl --kubeconfig $KC get nodes -o wide
N1=$(kubectl --kubeconfig $KC get nodes -o jsonpath="{.items[?(@.status.addresses[0].address=='$G1')].metadata.name}")
N2=$(kubectl --kubeconfig $KC get nodes -o jsonpath="{.items[?(@.status.addresses[0].address=='$G2')].metadata.name}")
N3=$(kubectl --kubeconfig $KC get nodes -o jsonpath="{.items[?(@.status.addresses[0].address=='$G3')].metadata.name}")
check "every node service runs" waitfor 120 bash -c "! $NODEAPI --server https://$G1:50000 get nodeservices --no-headers | grep -v ' Running '"
check "the cluster has DNS" waitfor 300 bash -c "kubectl --kubeconfig $KC get pods -A | grep -q 'coredns.* Running '"
check "the scheduler is gachi-scheduler" bash -c "$NODEAPI --server https://$G1:50000 get nodeservice kube-scheduler -o yaml | grep -q gachi-scheduler || $NODEAPI --server https://$G1:50000 get --raw '/apis/node.kuberoot.dev/v1alpha1/nodeservices/kube-scheduler/log?tailLines=200' | grep -qi 'gachi\|TagTeam'"

counter() { # NAME NODE [ANNOTATIONS]
cat <<EOF | k apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: $1, annotations: {${3:-}}}
spec:
  nodeName: $2
  containers:
  - name: count
    image: busybox:1.37
    command: [sh, -c, "i=0; while true; do i=\$((i+1)); echo \$i; sleep 1; done"]
EOF
}
last() { k logs "$1" 2>/dev/null | tail -1; }
# The first count the container wrote since it last started: a restored
# container brings its log along, so the log's own first line is older.
first() {
  local t; t=$(k get pod "$1" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}')
  k logs "$1" --timestamps 2>/dev/null | awk -v t="${t:0:19}" 'substr($1, 1, 19) >= t {print $2; exit}'
}
export -f last first

echo "== live migration"
counter counter $N2
check "the counter runs on $N2" waitfor 180 bash -c "k get pod counter -o jsonpath='{.status.phase}' | grep -qx Running"
sleep 20
before=$(last counter)
echo "      counted $before on $N2"
printf 'apiVersion: gym.gachinetes.dev/v1\nkind: Migration\nmetadata: {name: counter-to-g3}\nspec: {pod: counter, node: %s}\n' "$N3" | k apply -f - >/dev/null
check "the migration is done" waitfor 300 bash -c "k get migration counter-to-g3 -o jsonpath='{.status.phase}' | grep -qx Done"
k get migration counter-to-g3 -o jsonpath='{.status.phase}: {.status.message}{"\n"}'
check "the counter runs on $N3" waitfor 120 bash -c "[ \"\$(k get pod counter -o jsonpath='{.spec.nodeName} {.status.phase}')\" = \"$N3 Running\" ]"
sleep 5
check "it counts on from where it was, not from 1" bash -c "[ \"\$(first counter)\" -gt $before ]"
echo "      first count after the move: $(first counter), now $(last counter)"

echo "== rounds"
counter longjob $N2 'gym.gachinetes.dev/round: "20s"'
check "the long job runs" waitfor 180 bash -c "k get pod longjob -o jsonpath='{.status.phase}' | grep -qx Running"
check "a round is taken" waitfor 120 bash -c "kubectl --kubeconfig $KC -n gachinetes get configmap round-default-longjob"
sleep 25
held=$(kubectl --kubeconfig $KC -n gachinetes get configmap round-default-longjob -o jsonpath='{.data.round}' | grep -o '"node":"[^"]*"' | cut -d'"' -f4)
reached=$(last longjob)
k delete pod longjob --grace-period=0 --force >/dev/null 2>&1
check "it comes back" waitfor 120 bash -c "k get pod longjob -o jsonpath='{.status.phase}' | grep -qx Running"
check "on the node that held its round" bash -c "[ \"\$(k get pod longjob -o jsonpath='{.spec.nodeName}')\" = \"$held\" ]"
sleep 3
check "from its last round, not from 1" bash -c "[ \"\$(first longjob)\" -gt 1 ]"
echo "      reached $reached before it went; came back at $(first longjob)"

echo
[ $fails -eq 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit $fails
