package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/belts"
)

const webhookName = "gachinetes-belts"

// holder keeps the belts: the policies, and the webhook configuration
// that sends writes of the kinds they cover here.
type holder struct {
	cs        kubernetes.Interface
	dyn       dynamic.Interface
	caBundle  []byte
	url       string
	namespace string

	mu       sync.RWMutex
	policies []belts.Policy
}

func (h *holder) reload(ctx context.Context, store cache.Store) {
	var ps []belts.Policy
	for _, o := range store.List() {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		p, err := belts.FromObject(u)
		if err != nil {
			log.Print(err)
			continue
		}
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	h.mu.Lock()
	h.policies = ps
	h.mu.Unlock()
	if err := h.configure(ctx, ps); err != nil {
		log.Printf("webhook configuration: %v", err)
	}
}

// configure points the API server's admission at the referee for updates
// of the kinds the policies cover, and for nothing else. It fails open:
// a referee that is down lets writes through rather than stop the cluster.
func (h *holder) configure(ctx context.Context, ps []belts.Policy) error {
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(h.cs.Discovery()))
	var rules []admregv1.RuleWithOperations
	seen := map[string]bool{}
	for _, p := range ps {
		m, err := mapper.RESTMapping(schema.GroupKind{Group: p.Match.Group, Kind: p.Match.Kind})
		if err != nil {
			log.Printf("BeltPolicy %s: %v", p.Name, err)
			continue
		}
		k := m.Resource.Group + "/" + m.Resource.Resource
		if seen[k] {
			continue
		}
		seen[k] = true
		rules = append(rules, admregv1.RuleWithOperations{
			Operations: []admregv1.OperationType{admregv1.Update},
			Rule: admregv1.Rule{APIGroups: []string{m.Resource.Group}, APIVersions: []string{"*"},
				Resources: []string{m.Resource.Resource}, Scope: ptr.To(admregv1.AllScopes)},
		})
	}
	api := h.cs.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	if len(rules) == 0 {
		err := api.Delete(ctx, webhookName, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	cc := admregv1.WebhookClientConfig{CABundle: h.caBundle}
	if h.url != "" {
		cc.URL = ptr.To(h.url)
	} else {
		cc.Service = &admregv1.ServiceReference{Namespace: h.namespace, Name: "gachi-referee", Path: ptr.To("/belts"), Port: ptr.To(int32(443))}
	}
	want := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: webhookName},
		Webhooks: []admregv1.ValidatingWebhook{{
			Name: "belts." + belts.Group, ClientConfig: cc, Rules: rules,
			FailurePolicy: ptr.To(admregv1.Ignore), SideEffects: ptr.To(admregv1.SideEffectClassNone),
			AdmissionReviewVersions: []string{"v1"}, TimeoutSeconds: ptr.To(int32(5)),
			MatchPolicy: ptr.To(admregv1.Equivalent),
		}},
	}
	cur, err := api.Get(ctx, webhookName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, want, metav1.CreateOptions{FieldManager: "gachi-referee"})
		return err
	}
	if err != nil {
		return err
	}
	want.ResourceVersion = cur.ResourceVersion
	_, err = api.Update(ctx, want, metav1.UpdateOptions{FieldManager: "gachi-referee"})
	return err
}

// review answers an admission review of an update.
func (h *holder) review(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	var old, new map[string]any
	if len(req.OldObject.Raw) > 0 {
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return resp
		}
	}
	if err := json.Unmarshal(req.Object.Raw, &new); err != nil {
		return resp
	}
	var opts struct {
		FieldManager string `json:"fieldManager"`
	}
	_ = json.Unmarshal(req.Options.Raw, &opts)
	h.mu.RLock()
	ps := h.policies
	h.mu.RUnlock()
	v := belts.Judge(ps, belts.Write{
		Group: req.Kind.Group, Kind: req.Kind.Kind, Namespace: req.Namespace, Name: req.Name,
		Manager: opts.FieldManager, User: req.UserInfo.Username, Groups: req.UserInfo.Groups,
		Old: old, New: new,
	})
	resp.Allowed, resp.Warnings = v.Allowed, v.Warnings
	if !v.Allowed {
		resp.Result = &metav1.Status{Status: metav1.StatusFailure, Message: v.Message,
			Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden}
	}
	return resp
}

func serveWebhook(addr string, cert tls.Certificate, h *holder) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /belts", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var review admissionv1.AdmissionReview
		if err := json.Unmarshal(raw, &review); err != nil || review.Request == nil {
			http.Error(w, "not an admission review", http.StatusBadRequest)
			return
		}
		review.Response = h.review(review.Request)
		review.Request = nil
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(review)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
