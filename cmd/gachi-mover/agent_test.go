package main

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func agentFor(t *testing.T, allowed bool) (*Agent, string) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true,
			User: authnv1.UserInfo{Username: "system:serviceaccount:gachinetes:gachi-mover"}}}, nil
	})
	cs.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	dir := t.TempDir()
	return &Agent{cs: cs, node: "n1", dir: dir}, dir
}

func TestAgent(t *testing.T) {
	a, dir := agentFor(t, true)
	var imported string
	a.ctr = func(_ context.Context, archive string) error {
		b, _ := os.ReadFile(archive)
		imported = string(b)
		return nil
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	do := func(method, path string, body []byte, token string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	var ck bytes.Buffer
	tw := tar.NewWriter(&ck)
	_ = tw.WriteHeader(&tar.Header{Name: "spec.dump", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	path := filepath.Join(dir, "checkpoint-web_shop-nginx-1.tar")
	_ = os.WriteFile(path, ck.Bytes(), 0o600)

	if code, _ := do("GET", "/checkpoint?path="+path, nil, ""); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	if code, body := do("GET", "/checkpoint?path="+path, nil, "t"); code != 200 || body != ck.String() {
		t.Fatalf("get: %d", code)
	}
	if code, _ := do("GET", "/checkpoint?path=/etc/shadow", nil, "t"); code != http.StatusBadRequest {
		t.Fatalf("a file that is not a checkpoint: %d", code)
	}
	if code, _ := do("GET", "/checkpoint?path="+dir+"/../x.tar", nil, "t"); code != http.StatusBadRequest {
		t.Fatalf("out of the directory: %d", code)
	}
	if code, body := do("POST", "/import?ref=localhost/gachinetes/checkpoint/shop/web/nginx:1&container=nginx", ck.Bytes(), "t"); code != 200 || !strings.Contains(imported, "index.json") {
		t.Fatalf("import: %d %s", code, body)
	}
	if code, _ := do("POST", "/import?ref=docker.io/evil:1&container=x", ck.Bytes(), "t"); code != http.StatusBadRequest {
		t.Fatalf("import under another name: %d", code)
	}
	if code, _ := do("DELETE", "/checkpoint?path="+path, nil, "t"); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}

	denied, _ := agentFor(t, false)
	srv2 := httptest.NewServer(denied.Handler())
	defer srv2.Close()
	req, _ := http.NewRequest("GET", srv2.URL+"/checkpoint?path=x", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a caller who may not checkpoint: %d", resp.StatusCode)
	}
}
