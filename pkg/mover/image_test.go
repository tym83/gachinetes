package mover

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckpointImage(t *testing.T) {
	// A checkpoint archive: any tar.
	var ck bytes.Buffer
	tw := tar.NewWriter(&ck)
	_ = tw.WriteHeader(&tar.Header{Name: "spec.dump", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	src := filepath.Join(t.TempDir(), "checkpoint.tar")
	if err := os.WriteFile(src, ck.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := ImageRef("shop", "web", "nginx", time.Unix(42, 0))
	var out bytes.Buffer
	if err := WriteCheckpointImage(src, ref, "nginx", &out); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("index: %s %v", files["index.json"], err)
	}
	if index.Manifests[0].Annotations["io.containerd.image.name"] != ref {
		t.Fatalf("name: %v", index.Manifests[0].Annotations)
	}
	// Every blob is where its digest says.
	for name, b := range files {
		if !strings.HasPrefix(name, "blobs/sha256/") {
			continue
		}
		s := sha256.Sum256(b)
		if hex.EncodeToString(s[:]) != strings.TrimPrefix(name, "blobs/sha256/") {
			t.Errorf("%s: wrong digest", name)
		}
	}
	m := files[blob(index.Manifests[0].Digest)]
	if !bytes.Contains(m, []byte(CheckpointAnnotation)) || !bytes.Contains(m, []byte(`"nginx"`)) {
		t.Fatalf("manifest: %s", m)
	}
	if ref != "localhost:5001/gachinetes/checkpoint/shop/web/nginx:42" {
		t.Fatalf("ref: %s", ref)
	}
}
