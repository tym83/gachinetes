# Gachinetes on the node: its binaries, built by hack/kuberoot.sh into
# $OUT/gachinetes, next to kuberoot's.
for b in gachi-tape gachi-referee gachi-scheduler gachi-fantasy gachictl; do
  install -m 0755 "$OUT/gachinetes/$b" "$r/usr/bin/$b"
done
