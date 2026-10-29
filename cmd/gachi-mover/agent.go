package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/mover"
)

// checkpointDir is where the kubelet writes checkpoints.
const checkpointDir = "/var/lib/kubelet/checkpoints"

// Agent hands out the kubelet's checkpoints on its node and takes
// checkpoints in as images for the runtime. A caller must be allowed to
// checkpoint on the node, as the kubelet asks of its own callers (create
// nodes/checkpoint); the agent asks the API server with the kubelet's
// credentials.
type Agent struct {
	cs   kubernetes.Interface
	node string
	dir  string
	// ctr imports images; tests swap it.
	ctr func(ctx context.Context, archive string) error
}

func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /checkpoint", a.serveCheckpoint)
	mux.HandleFunc("DELETE /checkpoint", a.removeCheckpoint)
	mux.HandleFunc("POST /import", a.importCheckpoint)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.allowed(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// allowed checks the caller's bearer token and its right to checkpoint on
// this node.
func (a *Agent) allowed(r *http.Request) error {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return fmt.Errorf("a bearer token is needed")
	}
	ctx := r.Context()
	tr, err := a.cs.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	if !tr.Status.Authenticated {
		return fmt.Errorf("the token is not good: %s", tr.Status.Error)
	}
	u := tr.Status.User
	extra := map[string]authzv1.ExtraValue{}
	for k, v := range u.Extra {
		extra[k] = authzv1.ExtraValue(v)
	}
	sar, err := a.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User: u.Username, Groups: u.Groups, UID: u.UID, Extra: extra,
		ResourceAttributes: &authzv1.ResourceAttributes{Verb: "create", Resource: "nodes", Subresource: "checkpoint", Name: a.node},
	}}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	if !sar.Status.Allowed {
		return fmt.Errorf("%s may not checkpoint on %s", u.Username, a.node)
	}
	return nil
}

// checkpointPath is a checkpoint file the kubelet wrote, and nothing else.
func (a *Agent) checkpointPath(p string) (string, error) {
	clean := filepath.Clean(p)
	if filepath.Dir(clean) != a.dir || !strings.HasSuffix(clean, ".tar") {
		return "", fmt.Errorf("%s is not a checkpoint of the kubelet", p)
	}
	return clean, nil
}

func (a *Agent) serveCheckpoint(w http.ResponseWriter, r *http.Request) {
	p, err := a.checkpointPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, p)
}

func (a *Agent) removeCheckpoint(w http.ResponseWriter, r *http.Request) {
	p, err := a.checkpointPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// importCheckpoint takes a checkpoint archive in the body and makes it
// the image ref, for container, in the runtime's k8s.io namespace.
func (a *Agent) importCheckpoint(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref, container := q.Get("ref"), q.Get("container")
	if !strings.HasPrefix(ref, "localhost/gachinetes/checkpoint/") || container == "" {
		http.Error(w, "ref and container are needed", http.StatusBadRequest)
		return
	}
	tmp, err := os.MkdirTemp("", "gachi-mover-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)
	ck := filepath.Join(tmp, "checkpoint.tar")
	f, err := os.Create(ck)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.Close()
	archive := filepath.Join(tmp, "image.tar")
	out, err := os.Create(archive)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = mover.WriteCheckpointImage(ck, ref, container, out)
	out.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctr := a.ctr
	if ctr == nil {
		ctr = importWithCtr
	}
	if err := ctr(r.Context(), archive); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("imported %s", ref)
}

func importWithCtr(ctx context.Context, archive string) error {
	out, err := exec.CommandContext(ctx, "ctr", "-n", "k8s.io", "images", "import", "--no-unpack=false", archive).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ctr images import: %v: %s", err, out)
	}
	return nil
}
