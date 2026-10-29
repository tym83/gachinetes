package mover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// RegistryAddr is where a node's agent serves checkpoint images to the
// node's own runtime, plain HTTP on the loopback.
const RegistryAddr = "localhost:5001"

// Registry keeps checkpoint images on a node and serves them read-only in
// the OCI distribution API, so that the kubelet pulls a checkpoint as it
// pulls any image and the runtime restores it.
type Registry struct {
	Dir string
}

var (
	refPattern    = regexp.MustCompile(`^[a-z0-9]+(?:[._/-][a-z0-9]+)*$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func (r *Registry) blob(d string) string {
	return filepath.Join(r.Dir, "blobs", strings.TrimPrefix(d, "sha256:"))
}

func (r *Registry) tag(repo, tag string) string {
	return filepath.Join(r.Dir, "manifests", repo, tag)
}

func (r *Registry) putBlob(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	d := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(r.Dir, "blobs"), 0o700); err != nil {
		return "", err
	}
	return d, os.WriteFile(r.blob(d), data, 0o600)
}

// Put keeps the kubelet's checkpoint archive at src as the image ref
// (localhost:5001/repo:tag) of container.
func (r *Registry) Put(src, ref, container string) error {
	name, tag, ok := strings.Cut(strings.TrimPrefix(ref, RegistryAddr+"/"), ":")
	if !ok || !refPattern.MatchString(name) || !refPattern.MatchString(tag) {
		return fmt.Errorf("%s is not a checkpoint image of this node", ref)
	}
	if err := os.MkdirAll(filepath.Join(r.Dir, "blobs"), 0o700); err != nil {
		return err
	}
	// The layer: the checkpoint archive itself, moved into the store.
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	layer := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(src, r.blob(layer)); err != nil {
		return err
	}
	annotations := map[string]string{CheckpointAnnotation: container}
	config, _ := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH, "os": "linux", "created": time.Now().UTC().Format(time.RFC3339),
		"config": map[string]any{"Labels": annotations},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{layer}},
	})
	cfgDigest, err := r.putBlob(config)
	if err != nil {
		return err
	}
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": len(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layer, "size": size}},
		"annotations": annotations,
	})
	mDigest, err := r.putBlob(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.tag(name, tag)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.tag(name, tag), []byte(mDigest), 0o600)
}

// Handler serves the images, read-only: /v2/, manifests by tag or digest,
// blobs by digest.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(req.URL.Path, "/v2/")
		if p == "" || p == req.URL.Path {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			return
		}
		var file, contentType, digest string
		switch {
		case strings.Contains(p, "/manifests/"):
			name, ref, _ := strings.Cut(p, "/manifests/")
			if !refPattern.MatchString(name) {
				http.NotFound(w, req)
				return
			}
			digest = ref
			if !digestPattern.MatchString(ref) {
				if !refPattern.MatchString(ref) {
					http.NotFound(w, req)
					return
				}
				raw, err := os.ReadFile(r.tag(name, ref))
				if err != nil {
					http.NotFound(w, req)
					return
				}
				digest = string(raw)
			}
			file, contentType = r.blob(digest), "application/vnd.oci.image.manifest.v1+json"
		case strings.Contains(p, "/blobs/"):
			_, d, _ := strings.Cut(p, "/blobs/")
			if !digestPattern.MatchString(d) {
				http.NotFound(w, req)
				return
			}
			file, contentType, digest = r.blob(d), "application/octet-stream", d
		default:
			http.NotFound(w, req)
			return
		}
		f, err := os.Open(file)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
		if req.Method == http.MethodGet {
			_, _ = io.Copy(w, f)
		}
	})
}
