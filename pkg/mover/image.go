// Package mover moves running pods between nodes: a checkpoint of their
// containers on one node, restored on another — get your ass back here.
package mover

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
)

// CheckpointAnnotation marks an image as a container's checkpoint, named by
// the container it was taken of; container runtimes restore such an image
// instead of starting it (the annotation CRI-O and containerd share).
const CheckpointAnnotation = "io.kubernetes.cri-o.annotations.checkpoint.name"

// WriteCheckpointImage writes an OCI image archive, as ctr images import
// takes it, whose one layer is the kubelet's checkpoint archive at src,
// named ref, for container.
func WriteCheckpointImage(src, ref, container string, out io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// The checkpoint archive is an uncompressed tar: the layer itself.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	layerDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	annotations := map[string]string{CheckpointAnnotation: container}
	config, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"created":      time.Now().UTC().Format(time.RFC3339),
		"config":       map[string]any{"Labels": annotations},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{layerDigest}},
	})
	if err != nil {
		return err
	}
	configDigest := digest(config)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layerDigest, "size": st.Size(),
		}},
		"annotations": annotations,
	})
	if err != nil {
		return err
	}
	manifestDigest := digest(manifest)
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifestDigest, "size": len(manifest),
			"annotations": map[string]string{"io.containerd.image.name": ref, "org.opencontainers.image.ref.name": ref},
		}},
	})
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := put("index.json", index); err != nil {
		return err
	}
	if err := put(blob(configDigest), config); err != nil {
		return err
	}
	if err := put(blob(manifestDigest), manifest); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: blob(layerDigest), Mode: 0o644, Size: st.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return tw.Close()
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func blob(d string) string { return "blobs/sha256/" + d[len("sha256:"):] }

// ImageRef is the name a checkpoint of a pod's container gets on the node
// it is restored on.
func ImageRef(namespace, pod, container string, at time.Time) string {
	return fmt.Sprintf("localhost/gachinetes/checkpoint/%s/%s/%s:%d", namespace, pod, container, at.Unix())
}
