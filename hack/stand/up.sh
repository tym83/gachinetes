#!/bin/sh
# Starts a control plane on the kuberoot store with its tape, and gachi-tape
# as its aggregated API, in the stand pod (hack/hetzner/stand.yaml). The
# sources are in /work/src/gachinetes.
set -eu
W=/work
# Each run keeps its own certificates, store and logs: RUN=/work/run-2 for
# a fresh one.
R=${RUN:-$W/run}
V=v1.37.1
IP=$(hostname -i | awk '{print $1}')
export PATH=$W/bin:$PATH
mkdir -p $W/bin $R/pki $R/kube $R/logs
cd $W/bin
for b in kube-apiserver kube-controller-manager kubectl kubeadm; do
  [ -x $b ] || { curl -fsSLo $b https://dl.k8s.io/release/$V/bin/linux/amd64/$b && chmod +x $b; }
done
# KUBEROOT_STORE=<commit> tries a store not merged yet.
GOBIN=$W/bin go install github.com/kuberoot-dev/kuberoot/cmd/kuberoot-store@${KUBEROOT_STORE:-main}
(cd $W/src/gachinetes && go build -o $W/bin/ ./cmd/...)

if [ ! -f $R/pki/ca.crt ]; then
  for c in ca front-proxy-ca front-proxy-client sa; do
    kubeadm init phase certs $c --cert-dir $R/pki
  done
  kubeadm init phase certs apiserver --cert-dir $R/pki --kubernetes-version $V \
    --apiserver-advertise-address $IP --apiserver-cert-extra-sans 127.0.0.1
  for k in super-admin controller-manager; do
    kubeadm init phase kubeconfig $k --cert-dir $R/pki --kubeconfig-dir $R/kube --kubernetes-version $V \
      --apiserver-advertise-address $IP --control-plane-endpoint $IP:6443
  done
fi

run() { name=$1; shift; pgrep -f "^$W/bin/$name" >/dev/null || setsid nohup "$W/bin/$name" "$@" >$R/logs/$name.log 2>&1 </dev/null & }

run kuberoot-store --name s1 --data-dir $R/store \
  --listen-client-urls http://127.0.0.1:2379 --listen-peer-urls http://127.0.0.1:2380 \
  --keep-history --listen-tape-urls http://127.0.0.1:2382
sleep 3
run kube-apiserver --etcd-servers=http://127.0.0.1:2379 --etcd-compaction-interval=1m \
  --advertise-address=$IP --secure-port=6443 --service-cluster-ip-range=10.96.0.0/16 \
  --client-ca-file=$R/pki/ca.crt --tls-cert-file=$R/pki/apiserver.crt --tls-private-key-file=$R/pki/apiserver.key \
  --service-account-issuer=https://kubernetes.default.svc --service-account-key-file=$R/pki/sa.pub \
  --service-account-signing-key-file=$R/pki/sa.key --authorization-mode=Node,RBAC \
  --requestheader-client-ca-file=$R/pki/front-proxy-ca.crt --requestheader-allowed-names=front-proxy-client \
  --requestheader-extra-headers-prefix=X-Remote-Extra- --requestheader-group-headers=X-Remote-Group \
  --requestheader-username-headers=X-Remote-User \
  --proxy-client-cert-file=$R/pki/front-proxy-client.crt --proxy-client-key-file=$R/pki/front-proxy-client.key \
  --enable-aggregator-routing=true
until kubectl --kubeconfig $R/kube/super-admin.conf get --raw /readyz >/dev/null 2>&1; do sleep 1; done
run kube-controller-manager --kubeconfig=$R/kube/controller-manager.conf \
  --authentication-kubeconfig=$R/kube/controller-manager.conf --authorization-kubeconfig=$R/kube/controller-manager.conf \
  --service-account-private-key-file=$R/pki/sa.key --root-ca-file=$R/pki/ca.crt \
  --cluster-signing-cert-file=$R/pki/ca.crt --cluster-signing-key-file=$R/pki/ca.key \
  --use-service-account-credentials --leader-elect=false
run gachi-tape --kubeconfig $R/kube/super-admin.conf --listen $IP:9282

# The aggregated API: the stand has no nodes, so the Service's endpoint is
# the pod itself, which the API server reaches directly.
kubectl --kubeconfig $R/kube/super-admin.conf apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata: {name: gachinetes}
---
apiVersion: v1
kind: Service
metadata: {name: gachi-tape, namespace: gachinetes}
spec:
  ports: [{port: 443, targetPort: 9282}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: gachi-tape-stand
  namespace: gachinetes
  labels: {kubernetes.io/service-name: gachi-tape}
addressType: IPv4
ports: [{port: 9282, protocol: TCP}]
endpoints: [{addresses: ["$IP"], conditions: {ready: true}}]
YAML
# The APIService and roles as deploy/ ships them.
awk 'BEGIN{RS="---\n"; ORS="---\n"} /kind: (APIService|ClusterRole)\n/' $W/src/gachinetes/deploy/tape.yaml \
  | kubectl --kubeconfig $R/kube/super-admin.conf apply -f -
echo "stand up: KUBECONFIG=$R/kube/super-admin.conf"
