package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/aniki"
	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/bucks"
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

	// What the referee reads: belt policies, budgets, price lists, pods.
	policyStore, budgetStore, priceStore cache.Store
	pods                                 cache.Indexer

	guard *guard

	mu       sync.RWMutex
	policies []belts.Policy
}

func (h *holder) reload(ctx context.Context) {
	var ps []belts.Policy
	for _, o := range h.policyStore.List() {
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
	if err := h.configure(ctx, ps, len(h.budgetStore.List()) > 0); err != nil {
		log.Printf("webhook configuration: %v", err)
	}
}

// configure points the API server's admission at the referee for updates
// of the kinds the policies cover, and for pods made in namespaces when
// there are budgets; for nothing else. It fails open: a referee that is
// down lets writes through rather than stop the cluster.
func (h *holder) configure(ctx context.Context, ps []belts.Policy, budgets bool) error {
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
	client := func(path string) admregv1.WebhookClientConfig {
		cc := admregv1.WebhookClientConfig{CABundle: h.caBundle}
		if h.url != "" {
			cc.URL = ptr.To(strings.TrimSuffix(h.url, "/belts") + path)
		} else {
			cc.Service = &admregv1.ServiceReference{Namespace: h.namespace, Name: "gachi-referee", Path: ptr.To(path), Port: ptr.To(int32(443))}
		}
		return cc
	}
	hook := func(name, path string, rules []admregv1.RuleWithOperations) admregv1.ValidatingWebhook {
		return admregv1.ValidatingWebhook{
			Name: name, ClientConfig: client(path), Rules: rules,
			FailurePolicy: ptr.To(admregv1.Ignore), SideEffects: ptr.To(admregv1.SideEffectClassNone),
			AdmissionReviewVersions: []string{"v1"}, TimeoutSeconds: ptr.To(int32(5)),
			MatchPolicy: ptr.To(admregv1.Equivalent),
		}
	}
	want := &admregv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookName}}
	if len(rules) > 0 {
		want.Webhooks = append(want.Webhooks, hook("belts."+belts.Group, "/belts", rules))
	}
	// Contracts and spots always pass the referee, and so do deletions of
	// what may be spotted.
	op := func(ops []admregv1.OperationType, group string, resources ...string) admregv1.RuleWithOperations {
		return admregv1.RuleWithOperations{Operations: ops,
			Rule: admregv1.Rule{APIGroups: []string{group}, APIVersions: []string{"*"}, Resources: resources, Scope: ptr.To(admregv1.AllScopes)}}
	}
	cu := []admregv1.OperationType{admregv1.Create, admregv1.Update}
	del := []admregv1.OperationType{admregv1.Delete}
	want.Webhooks = append(want.Webhooks, hook("gym."+belts.Group, "/gym", []admregv1.RuleWithOperations{
		op(cu, belts.Group, "contracts", "spots"),
		op(del, "", "namespaces", "persistentvolumes"),
		op(del, "apiextensions.k8s.io", "customresourcedefinitions"),
	}))
	// Aniki speaks up, with warnings only, where a namespace asks him to.
	aniki := hook("aniki."+belts.Group, "/aniki", []admregv1.RuleWithOperations{
		op(cu, "apps", "deployments", "statefulsets", "daemonsets"),
		op(cu, "batch", "jobs", "cronjobs"),
	})
	aniki.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"gym.gachinetes.dev/aniki": "on"}}
	want.Webhooks = append(want.Webhooks, aniki)
	if budgets {
		want.Webhooks = append(want.Webhooks, hook("bucks."+belts.Group, "/bucks", []admregv1.RuleWithOperations{{
			Operations: []admregv1.OperationType{admregv1.Create},
			Rule: admregv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"},
				Resources: []string{"pods"}, Scope: ptr.To(admregv1.NamespacedScope)},
		}}))
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

// price answers an admission review of a pod made: refused when it takes
// its namespace over its budget.
func (h *holder) price(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return resp
	}
	if pod.Namespace == "" {
		pod.Namespace = req.Namespace
	}
	budget := -1.0
	for _, o := range h.budgetStore.List() {
		u, ok := o.(*unstructured.Unstructured)
		if !ok || u.GetNamespace() != pod.Namespace {
			continue
		}
		if b, err := bucks.BudgetFrom(u); err == nil && (budget < 0 || b.Monthly < budget) {
			budget = b.Monthly
		}
	}
	if budget < 0 {
		return resp
	}
	prices, ok := h.prices()
	if !ok {
		return resp
	}
	var existing []*corev1.Pod
	objs, _ := h.pods.ByIndex(cache.NamespaceIndex, pod.Namespace)
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var p corev1.Pod
		if runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p) == nil {
			existing = append(existing, &p)
		}
	}
	if fits, msg := bucks.Check(prices, budget, existing, &pod); !fits {
		resp.Allowed = false
		resp.Result = &metav1.Status{Status: metav1.StatusFailure, Message: msg,
			Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden}
	}
	return resp
}

// prices are the cluster's price list: the PriceList named gym, else the
// first.
func (h *holder) prices() (bucks.Prices, bool) {
	var first *unstructured.Unstructured
	for _, o := range h.priceStore.List() {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if first == nil || u.GetName() == "gym" {
			first = u
		}
	}
	if first == nil {
		return bucks.Prices{}, false
	}
	p, err := bucks.PricesFrom(first)
	return p, err == nil
}

func serveWebhook(addr string, cert tls.Certificate, h *holder) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	review := func(answer func(*admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
			review.Response = answer(review.Request)
			review.Request = nil
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(review)
		}
	}
	mux.HandleFunc("POST /bucks", review(h.price))
	mux.HandleFunc("POST /belts", review(h.review))
	mux.HandleFunc("POST /gym", review(h.guard.review))
	mux.HandleFunc("POST /aniki", review(func(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
		resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
		u := &unstructured.Unstructured{}
		if json.Unmarshal(req.Object.Raw, &u.Object) == nil {
			resp.Warnings = aniki.Warnings(aniki.Review(u))
		}
		return resp
	}))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
