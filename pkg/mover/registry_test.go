package mover

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "checkpoint.tar")
	if err := os.WriteFile(src, []byte("checkpoint bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Dir: filepath.Join(dir, "registry")}
	ref := ImageRef("shop", "web", "nginx", time.Unix(42, 0))
	if !strings.HasPrefix(ref, RegistryAddr+"/gachinetes/checkpoint/shop/web/nginx:") {
		t.Fatalf("ref: %s", ref)
	}
	if err := r.Put(src, ref, "nginx"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	get := func(path string) (int, []byte, http.Header) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	if code, _, _ := get("/v2/"); code != 200 {
		t.Fatalf("/v2/: %d", code)
	}
	name, tag, _ := strings.Cut(strings.TrimPrefix(ref, RegistryAddr+"/"), ":")
	code, raw, h := get("/v2/" + name + "/manifests/" + tag)
	if code != 200 || !strings.Contains(string(raw), CheckpointAnnotation) {
		t.Fatalf("manifest: %d %s", code, raw)
	}
	var m struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	_ = json.Unmarshal(raw, &m)
	if code, b, _ := get("/v2/" + name + "/blobs/" + m.Layers[0].Digest); code != 200 || string(b) != "checkpoint bytes" {
		t.Fatalf("layer: %d %q", code, b)
	}
	if code, _, _ := get("/v2/" + name + "/manifests/" + h.Get("Docker-Content-Digest")); code != 200 {
		t.Fatalf("manifest by digest: %d", code)
	}
	if code, _, _ := get("/v2/" + name + "/blobs/../../etc/passwd"); code != 404 {
		t.Fatalf("a path out of the store: %d", code)
	}
	if err := r.Put(src, "docker.io/evil:1", "x"); err == nil {
		t.Fatal("an image of another registry was kept")
	}
}
