# Gachinetes on the node: its binaries, built by hack/kuberoot.sh into
# $OUT/gachinetes, next to kuberoot's.
for b in gachi-tape gachi-referee gachi-scheduler gachi-fantasy gachi-mover gachictl; do
  install -m 0755 "$OUT/gachinetes/$b" "$r/usr/bin/$b"
done
# CRIU, Debian's, with its libraries (hack/kuberoot.sh).
cp -r "$OUT/gachinetes/criu/." "$r/"
# CRIU locks the network with nftables: the node has no iptables. runc
# reads runc.conf, CRIU itself default.conf.
mkdir -p "$r/etc/criu"
printf 'network-lock nftables\n' > "$r/etc/criu/default.conf"
printf 'network-lock nftables\n' > "$r/etc/criu/runc.conf"
