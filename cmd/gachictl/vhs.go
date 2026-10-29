package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tym83/gachinetes/pkg/tape"
)

//go:embed vhs
var vhsFiles embed.FS

// vhsCmd serves the tape as a VHS cassette in the browser. The page talks
// to this process only, on the loopback; this process talks to the cluster
// with the user's own credentials, as kubectl does.
func (a *app) vhsCmd() *cobra.Command {
	var port int
	var noOpen bool
	c := &cobra.Command{
		Use:   "vhs",
		Short: "▶ The tape as a VHS cassette, in the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return err
			}
			addr := l.Addr().String()
			token := make([]byte, 16)
			if _, err := rand.Read(token); err != nil {
				return err
			}
			h := a.vhsHandler(addr, hex.EncodeToString(token))
			u := "http://" + addr + "/"
			fmt.Printf("♂ VHS  PLAY ▶  %s  (Ctrl-C to eject)\n", u)
			if !noOpen {
				openBrowser(u)
			}
			srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
			return srv.Serve(l)
		},
	}
	c.Flags().IntVar(&port, "port", 8383, "the loopback port; 0 picks one")
	c.Flags().BoolVar(&noOpen, "no-open", false, "do not open a browser")
	return c
}

// vhsHandler serves the page and its API. Only requests to this very
// address are answered, against DNS rebinding; changes take the token the
// page was served with, against other sites posting to the loopback.
func (a *app) vhsHandler(addr, token string) http.Handler {
	static, _ := fs.Sub(vhsFiles, "vhs")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:")
		_, _ = w.Write([]byte(strings.Replace(string(page), "{{TOKEN}}", token, 1)))
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	// The tape, read-only, with the user's rights.
	mux.HandleFunc("GET /api/tape/{what}", func(w http.ResponseWriter, r *http.Request) {
		what := r.PathValue("what")
		switch what {
		case "info", "objects", "history", "graveyard", "diff":
		default:
			http.NotFound(w, r)
			return
		}
		raw, err := a.raw(r.Context(), "/"+what, r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	// What an undelete would bring back, and bringing it back.
	mux.HandleFunc("GET /api/undelete", func(w http.ResponseWriter, r *http.Request) {
		steps, skipped, err := a.planFor(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct{ Kind, Namespace, Name string }
		var plan []item
		for _, s := range steps {
			plan = append(plan, item{s.Object.GetKind(), s.Object.GetNamespace(), s.Object.GetName()})
		}
		writeJSONResponse(w, map[string]any{"restore": plan, "skipped": skipped})
	})
	mux.HandleFunc("POST /api/undelete", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gachi-Token")), []byte(token)) != 1 {
			http.Error(w, "no token: only the page this gachictl served may change the cluster", http.StatusForbidden)
			return
		}
		steps, _, err := a.planFor(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		revivals, err := a.revive(r.Context(), steps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSONResponse(w, revivals)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// planFor plans the undelete the page asks for: kind (Kind or
// Kind.group), name and namespace, as on the object it shows.
func (a *app) planFor(r *http.Request) ([]tape.Step, []tape.Skipped, error) {
	q := r.URL.Query()
	t := target{kind: q.Get("kind"), name: q.Get("name"), namespace: q.Get("namespace")}
	if t.kind == "" || t.name == "" {
		return nil, nil, fmt.Errorf("kind and name are needed")
	}
	if t.kind == "Namespace" {
		t.namespace = t.name
	}
	graves, err := a.gravesFor(r.Context(), t)
	if err != nil {
		return nil, nil, err
	}
	if len(graves) == 0 {
		return nil, nil, fmt.Errorf("%s %s is not in the graveyard", t.kind, t.name)
	}
	steps, skipped := tape.Plan(graves)
	return steps, skipped, nil
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// raw asks the tape and returns its answer as it is.
func (a *app) raw(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if a.tapeURL != "" {
		return httpGet(ctx, a.tapeURL+tape.Prefix+path+"?"+q.Encode())
	}
	return a.callAPI(ctx, path, q)
}

func openBrowser(u string) {
	cmd := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	}
	_ = exec.Command(cmd, u).Start()
}
