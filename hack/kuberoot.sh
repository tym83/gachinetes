#!/usr/bin/env bash
# Builds kuberoot as the distribution Gachinetes: the sources of kuberoot
# with distros/gachinetes added (edge's profile with gachi-scheduler in
# place of kube-scheduler and the tape and the referee as control plane
# services, CRIU, the gym's add-ons), driven by kuberoot's own
# hack/hetzner.sh in a builder of the tenant gachinetes.
#
#   hack/kuberoot.sh build amd64
#   hack/kuberoot.sh media amd64 [kernel args]
#   hack/kuberoot.sh node <name> amd64
#   hack/kuberoot.sh console <name>
#
#   KUBEROOT_SRC  a checkout of kuberoot (default: ../kuberoot)
#   KUBEROOT_REF  what of it to build (default: HEAD)
#   NS            the builder's namespace (default: tenant-gachinetes)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SRC=${KUBEROOT_SRC:-$ROOT/../kuberoot}
REF=${KUBEROOT_REF:-HEAD}
NS=${NS:-tenant-gachinetes}
export KUBECONFIG=${KUBECONFIG_WORKSHOP:-$HOME/workshop.txt}
CTX=${KUBE_CONTEXT:-admin@workshop}

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

git -C "${SRC}" archive "${REF}" | tar -x -C "${work}"
# hetzner.sh ships what git sees
git -C "${work}" init -q

d="${work}/distros/gachinetes"
mkdir -p "${d}/addons"
python3 "${ROOT}/distro/kuberoot/gachinetes/profile.py" "${work}/distros/edge/profile.yaml" > "${d}/profile.yaml"
cp "${work}"/distros/edge/addons/* "${ROOT}"/distro/kuberoot/gachinetes/addons/* "${d}/addons/"
cp "${ROOT}/distro/kuberoot/gachinetes/extras.sh" "${ROOT}/distro/kuberoot/gachinetes/kernel.config" "${ROOT}/distro/kuberoot/gachinetes/apk" "${d}/"

for f in "${work}"/hack/hetzner.sh "${work}"/hack/hetzner/*.yaml; do
  sed -i.bak "s/tenant-kuberoot/${NS}/g" "${f}" && rm "${f}.bak"
done

if [ "${1:-}" = build ]; then
  arch=${2:?arch required}
  # Gachinetes' binaries, static, where extras.sh takes them from.
  b() { kubectl --context "$CTX" -n "$NS" exec -i builder -c tools -- "$@"; }
  (cd "${ROOT}" && git ls-files -co --exclude-standard -z | grep -zv '^SESSION_LOG.md$' |
    COPYFILE_DISABLE=1 tar --null -T - -czf -) |
    b sh -c 'rm -rf /work/src/gachinetes && mkdir -p /work/src/gachinetes && tar -xzf - -C /work/src/gachinetes'
  b sh -c "set -e; cd /work/src/gachinetes; out=/work/out/${arch}/gachinetes; mkdir -p \$out
    export CGO_ENABLED=0 GOARCH=${arch} GOFLAGS=-mod=mod
    go build -trimpath -o \$out/ ./cmd/gachi-tape ./cmd/gachi-referee ./cmd/gachi-fantasy ./cmd/gachi-mover ./cmd/gachictl
    cd scheduler && go build -trimpath -o \$out/gachi-scheduler ./cmd/gachi-scheduler
    ls -la \$out"
  # CRIU, which Alpine does not package, built from source: Debian's 3.17
  # cannot read the vDSO of the kernels kuberoot runs. Its libraries come
  # along, beside the glibc kuberoot already ships from the same Debian.
  b sh -c "set -e; v=${CRIU_VERSION:-v4.3}; bin=/work/cache/criu-\$v-${arch}
    if [ ! -x \$bin ]; then
      apt-get -qq update && apt-get -qq install -y build-essential pkg-config libprotobuf-dev libprotobuf-c-dev \\
        protobuf-c-compiler protobuf-compiler python3-protobuf libnl-3-dev libnet1-dev libcap-dev libbsd-dev \\
        libnftables-dev libgnutls28-dev uuid-dev >/dev/null
      rm -rf /work/tmp/criu && git clone -q --depth 1 --branch \$v https://github.com/checkpoint-restore/criu /work/tmp/criu
      make -s -C /work/tmp/criu -j\$(nproc) criu >/dev/null
      mkdir -p /work/cache && cp /work/tmp/criu/criu/criu \$bin
    fi
    root=/work/out/${arch}/gachinetes/criu; rm -rf \$root; mkdir -p \$root/usr/sbin
    cp \$bin \$root/usr/sbin/criu
    ldd \$bin | awk '/=> \\//{print \$3}' | while read -r l; do
      case \$(basename \$l) in libc.so.*|libm.so.*|libpthread.so.*|libdl.so.*|librt.so.*|libresolv.so.*) continue;; esac
      mkdir -p \$root\$(dirname \$l); cp -L \$l \$root\$l
    done
    \$bin --version"
fi

KUBEROOT_DISTRO=gachinetes KUBEROOT_STORE_LAB=1 "${work}/hack/hetzner.sh" "$@"
