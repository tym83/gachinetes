package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVHSGuards(t *testing.T) {
	a := &app{tapeURL: "http://127.0.0.1:1"}
	srv := httptest.NewServer(nil)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Config.Handler = a.vhsHandler(addr, "secret")

	get := func(path string, host string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/", ""); code != 200 || !strings.Contains(body, `content="secret"`) {
		t.Fatalf("page: %d", code)
	}
	if code, _ := get("/static/app.js", ""); code != 200 {
		t.Fatalf("script: %d", code)
	}
	// A page of another site that resolved its name to the loopback.
	if code, _ := get("/", "evil.example:8383"); code != http.StatusForbidden {
		t.Fatalf("another host: %d", code)
	}
	if code, _ := get("/api/tape/secrets", ""); code != http.StatusNotFound {
		t.Fatalf("not a tape path: %d", code)
	}
	// Changing the cluster takes the page's token.
	resp, err := http.Post(srv.URL+"/api/undelete?kind=ConfigMap&name=a&namespace=b", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("undelete without the token: %d", resp.StatusCode)
	}
}
