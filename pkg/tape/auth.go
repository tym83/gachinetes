package tape

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Group is the API group the tape is served under, as an aggregated API:
// the API server authenticates the caller and passes who it is; the tape
// asks the API server whether that one may read what it asks for.
const (
	Group   = "tape.gachinetes.dev"
	Version = "v1"
	Prefix  = "/apis/" + Group + "/" + Version
)

// RequestHeader is how the API server, proxying to an aggregated API,
// vouches for the caller (the extension-apiserver-authentication
// ConfigMap in kube-system).
type RequestHeader struct {
	CA           *x509.CertPool
	AllowedNames []string
	UserHeaders  []string
	GroupHeaders []string
}

// LoadRequestHeader reads the API server's request header settings.
func LoadRequestHeader(ctx context.Context, cs kubernetes.Interface) (RequestHeader, error) {
	var rh RequestHeader
	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
	if err != nil {
		return rh, err
	}
	rh.CA = x509.NewCertPool()
	if !rh.CA.AppendCertsFromPEM([]byte(cm.Data["requestheader-client-ca-file"])) {
		return rh, fmt.Errorf("the API server has no request header authority: is --requestheader-client-ca-file set?")
	}
	list := func(k string) []string {
		var out []string
		_ = json.Unmarshal([]byte(cm.Data[k]), &out)
		return out
	}
	rh.AllowedNames = list("requestheader-allowed-names")
	rh.UserHeaders = list("requestheader-username-headers")
	rh.GroupHeaders = list("requestheader-group-headers")
	if len(rh.UserHeaders) == 0 {
		rh.UserHeaders = []string{"X-Remote-User"}
	}
	if len(rh.GroupHeaders) == 0 {
		rh.GroupHeaders = []string{"X-Remote-Group"}
	}
	return rh, nil
}

// caller is who the API server says made the request; ok is false when
// the request did not come through the API server.
func (rh RequestHeader) caller(r *http.Request) (user string, groups []string, ok bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", nil, false
	}
	leaf := r.TLS.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range r.TLS.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: rh.CA, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", nil, false
	}
	if len(rh.AllowedNames) > 0 && !slices.Contains(rh.AllowedNames, leaf.Subject.CommonName) {
		return "", nil, false
	}
	for _, h := range rh.UserHeaders {
		if user = r.Header.Get(h); user != "" {
			break
		}
	}
	for _, h := range rh.GroupHeaders {
		groups = append(groups, r.Header.Values(h)...)
	}
	return user, groups, user != ""
}

// Guard lets a request through when the API server vouches for its
// caller and the caller may get the tape resource the path names
// (objects, history, graveyard, diff, info). Revealing Secrets takes
// get on "secrets" of the tape group as well.
func Guard(rh RequestHeader, cs kubernetes.Interface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		user, groups, ok := rh.caller(r)
		if !ok {
			http.Error(w, "only through the API server", http.StatusUnauthorized)
			return
		}
		resource := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, Prefix), "/")
		need := []string{}
		if resource != "" {
			need = append(need, resource)
		}
		if r.URL.Query().Get("reveal") == "1" {
			need = append(need, "secrets")
		}
		for _, res := range need {
			allowed, err := may(r.Context(), cs, user, groups, res)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, fmt.Sprintf("%s may not get %s.%s", user, res, Group), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func may(ctx context.Context, cs kubernetes.Interface, user string, groups []string, resource string) (bool, error) {
	sar, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User: user, Groups: groups,
			ResourceAttributes: &authzv1.ResourceAttributes{Group: Group, Version: Version, Resource: resource, Verb: "get"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

// Discovery serves what the aggregator and kubectl ask of an API group.
func Discovery() http.Handler {
	mux := http.NewServeMux()
	resources := []metav1.APIResource{}
	for _, r := range []string{"info", "objects", "history", "graveyard", "diff", "secrets"} {
		resources = append(resources, metav1.APIResource{Name: r, Kind: "Tape", Verbs: metav1.Verbs{"get"}})
	}
	gv := metav1.GroupVersionForDiscovery{GroupVersion: Group + "/" + Version, Version: Version}
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /apis", func(w http.ResponseWriter, _ *http.Request) {
		write(w, metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
			Groups: []metav1.APIGroup{{Name: Group, Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv}}})
	})
	mux.HandleFunc("GET /apis/"+Group, func(w http.ResponseWriter, _ *http.Request) {
		write(w, metav1.APIGroup{TypeMeta: metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
			Name: Group, Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv})
	})
	mux.HandleFunc("GET "+Prefix, func(w http.ResponseWriter, _ *http.Request) {
		write(w, metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
			GroupVersion: Group + "/" + Version, APIResources: resources})
	})
	return mux
}
