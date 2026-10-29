package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/contracts"
)

var (
	contractsGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "contracts"}
	spotsGVR     = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "spots"}
	lockersGVR   = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "lockers"}
)

const (
	// SpottedLabel marks namespaces, CRDs and PVs whose deletion takes a
	// spotter.
	SpottedLabel = "gym.gachinetes.dev/spotted"
	// spotWindow: an approved Spot lets its deletion through this long.
	spotWindow = 15 * time.Minute
	epochMap   = "trust-epoch"
)

// guard is the referee's security: contracts, spots, lockers.
type guard struct {
	cs        kubernetes.Interface
	dyn       dynamic.Interface
	namespace string
}

func (g *guard) may(ctx context.Context, user string, groups []string, attrs authzv1.ResourceAttributes) (bool, error) {
	sar, err := g.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{User: user, Groups: groups, ResourceAttributes: &attrs},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

func deny(resp *admissionv1.AdmissionResponse, msg string) *admissionv1.AdmissionResponse {
	resp.Allowed = false
	resp.Result = &metav1.Status{Status: metav1.StatusFailure, Message: msg, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden}
	return resp
}

// review answers admission of contracts and spots, and of deletions of
// what is spotted.
func (g *guard) review(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	ctx := context.Background()
	user, groups := req.UserInfo.Username, req.UserInfo.Groups
	switch {
	case req.Kind.Group == belts.Group && req.Kind.Kind == "Contract":
		return g.reviewContract(ctx, req, resp, user, groups)
	case req.Kind.Group == belts.Group && req.Kind.Kind == "Spot":
		return g.reviewSpot(ctx, req, resp, user, groups)
	case req.Operation == admissionv1.Delete:
		return g.reviewDelete(ctx, req, resp, user)
	}
	return resp
}

func objects(req *admissionv1.AdmissionRequest) (old, cur *unstructured.Unstructured) {
	if len(req.OldObject.Raw) > 0 {
		old = &unstructured.Unstructured{}
		_ = json.Unmarshal(req.OldObject.Raw, &old.Object)
	}
	if len(req.Object.Raw) > 0 {
		cur = &unstructured.Unstructured{}
		_ = json.Unmarshal(req.Object.Raw, &cur.Object)
	}
	return old, cur
}

// reviewContract: whoever creates a contract names themselves as its
// requester; without a spotter, they must be allowed to grant the role
// themselves. A spotter approves by adding their name: someone else, who
// may grant the role. The terms never change.
func (g *guard) reviewContract(ctx context.Context, req *admissionv1.AdmissionRequest, resp *admissionv1.AdmissionResponse, user string, groups []string) *admissionv1.AdmissionResponse {
	old, cur := objects(req)
	if cur == nil {
		return resp
	}
	c, err := contracts.From(cur)
	if err != nil {
		return deny(resp, err.Error())
	}
	bind := authzv1.ResourceAttributes{Verb: "bind", Group: "rbac.authorization.k8s.io", Resource: "clusterroles", Name: c.Spec.ClusterRole, Namespace: c.Spec.Namespace}
	switch req.Operation {
	case admissionv1.Create:
		if err := c.Spec.Validate(); err != nil {
			return deny(resp, err.Error())
		}
		if c.RequestedBy != user {
			return deny(resp, fmt.Sprintf("annotation %s must name you (%s): gachictl contract sets it", contracts.RequestedBy, user))
		}
		if c.ApprovedBy != "" {
			return deny(resp, "a contract is approved after it is made, by someone else")
		}
		if !c.Spec.Spotter {
			ok, err := g.may(ctx, user, groups, bind)
			if err != nil {
				return deny(resp, err.Error())
			}
			if !ok {
				return deny(resp, fmt.Sprintf("you may not grant %s yourself: ask for a spotter (spotter: true)", c.Spec.ClusterRole))
			}
		}
	case admissionv1.Update:
		if old == nil {
			return resp
		}
		prev, _ := contracts.From(old)
		if fmt.Sprint(prev.Spec) != fmt.Sprint(c.Spec) || prev.RequestedBy != c.RequestedBy {
			return deny(resp, "the terms of a contract do not change: make another")
		}
		if c.ApprovedBy != prev.ApprovedBy {
			if prev.ApprovedBy != "" {
				return deny(resp, "a contract is approved once")
			}
			if c.ApprovedBy != user {
				return deny(resp, fmt.Sprintf("annotation %s must name you (%s)", contracts.ApprovedBy, user))
			}
			if user == c.RequestedBy {
				return deny(resp, "♂ nobody spots themselves")
			}
			ok, err := g.may(ctx, user, groups, bind)
			if err != nil {
				return deny(resp, err.Error())
			}
			if !ok {
				return deny(resp, fmt.Sprintf("a spotter must be allowed to grant %s", c.Spec.ClusterRole))
			}
		}
	}
	return resp
}

// spot is a Spot's spec: the deletion it approves.
type spot struct {
	Group    string `json:"group,omitempty"`
	Resource string `json:"resource"`
	Name     string `json:"name"`
}

func spotOf(u *unstructured.Unstructured) (spot, string, string) {
	var s spot
	raw, _ := json.Marshal(u.Object["spec"])
	_ = json.Unmarshal(raw, &s)
	a := u.GetAnnotations()
	return s, a[contracts.RequestedBy], a[contracts.ApprovedBy]
}

func (g *guard) reviewSpot(ctx context.Context, req *admissionv1.AdmissionRequest, resp *admissionv1.AdmissionResponse, user string, groups []string) *admissionv1.AdmissionResponse {
	old, cur := objects(req)
	if cur == nil {
		return resp
	}
	s, requester, approver := spotOf(cur)
	del := authzv1.ResourceAttributes{Verb: "delete", Group: s.Group, Resource: s.Resource, Name: s.Name}
	switch req.Operation {
	case admissionv1.Create:
		if s.Resource == "" || s.Name == "" {
			return deny(resp, "a spot names the resource and the name it is for")
		}
		if requester != user || approver != "" {
			return deny(resp, fmt.Sprintf("annotation %s must name you (%s), and nobody approves yet", contracts.RequestedBy, user))
		}
	case admissionv1.Update:
		ps, pr, pa := spotOf(old)
		if ps != s || pr != requester {
			return deny(resp, "a spot does not change: make another")
		}
		if approver != pa {
			if pa != "" || approver != user || user == requester {
				return deny(resp, "♂ a spot is approved once, by someone other than who asked")
			}
			ok, err := g.may(ctx, user, groups, del)
			if err != nil {
				return deny(resp, err.Error())
			}
			if !ok {
				return deny(resp, "a spotter must be allowed to delete it themselves")
			}
		}
	}
	return resp
}

// reviewDelete lets a spotted object go only with a spot, asked for by
// whoever deletes it and approved by someone else lately.
func (g *guard) reviewDelete(ctx context.Context, req *admissionv1.AdmissionRequest, resp *admissionv1.AdmissionResponse, user string) *admissionv1.AdmissionResponse {
	old, _ := objects(req)
	if old == nil || old.GetLabels()[SpottedLabel] != "true" {
		return resp
	}
	spots, err := g.dyn.Resource(spotsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return deny(resp, err.Error())
	}
	for i := range spots.Items {
		s, requester, approver := spotOf(&spots.Items[i])
		if s.Group != req.Resource.Group || s.Resource != req.Resource.Resource || s.Name != req.Name {
			continue
		}
		at, _, _ := unstructured.NestedString(spots.Items[i].Object, "status", "approvedAt")
		t, _ := time.Parse(time.RFC3339, at)
		if requester == user && approver != "" && time.Since(t) < spotWindow {
			return resp
		}
	}
	return deny(resp, fmt.Sprintf("♂ %s %s is spotted: gachictl spot delete %s/%s, and have someone else approve", req.Resource.Resource, req.Name, req.Resource.Resource, req.Name))
}

// epoch is the trust epoch: the safeword moves it on.
func (g *guard) epoch(ctx context.Context) int64 {
	cm, err := g.cs.CoreV1().ConfigMaps(g.namespace).Get(ctx, epochMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: epochMap, Namespace: g.namespace}, Data: map[string]string{"epoch": "1"}}
		if _, err := g.cs.CoreV1().ConfigMaps(g.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			log.Printf("trust epoch: %v", err)
		}
		return 1
	}
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(cm.Data["epoch"], 10, 64)
	return n
}

// reconcile keeps contracts' bindings as their states say, stamps spots'
// approval times, and keeps lockers' roles.
func (g *guard) reconcile(ctx context.Context) {
	epoch := g.epoch(ctx)
	if epoch == 0 {
		return
	}
	now := time.Now()
	list, err := g.dyn.Resource(contractsGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range list.Items {
			g.contract(ctx, &list.Items[i], epoch, now)
		}
	}
	if spots, err := g.dyn.Resource(spotsGVR).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range spots.Items {
			s := &spots.Items[i]
			_, _, approver := spotOf(s)
			if at, _, _ := unstructured.NestedString(s.Object, "status", "approvedAt"); approver != "" && at == "" {
				_ = unstructured.SetNestedField(s.Object, now.UTC().Format(time.RFC3339), "status", "approvedAt")
				_, _ = g.dyn.Resource(spotsGVR).UpdateStatus(ctx, s, metav1.UpdateOptions{FieldManager: "gachi-referee"})
			}
		}
	}
	g.lockers(ctx)
}

func (g *guard) contract(ctx context.Context, u *unstructured.Unstructured, epoch int64, now time.Time) {
	c, err := contracts.From(u)
	if err != nil {
		return
	}
	state, msg := contracts.Judge(c, epoch, now)
	name := "contract-" + c.Name
	switch state {
	case contracts.Active:
		if err := g.bind(ctx, name, c); err != nil {
			msg = err.Error()
		}
		if c.Activated.IsZero() {
			c.Activated = now
		}
		if c.Epoch == 0 {
			c.Epoch = epoch
		}
	default:
		g.unbind(ctx, name, c.Spec.Namespace)
	}
	status := map[string]any{"phase": string(state), "message": msg, "epoch": c.Epoch}
	if !c.Activated.IsZero() {
		status["activated"] = c.Activated.UTC().Format(time.RFC3339)
	}
	if fmt.Sprint(u.Object["status"]) == fmt.Sprint(status) {
		return
	}
	u.Object["status"] = status
	if _, err := g.dyn.Resource(contractsGVR).UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: "gachi-referee"}); err != nil {
		log.Printf("contract %s: %v", c.Name, err)
	}
}

func subject(s contracts.Spec) rbacv1.Subject {
	switch {
	case s.Group != "":
		return rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: s.Group}
	case s.ServiceAccount != "":
		ns, name := splitNS(s.ServiceAccount)
		return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: name}
	}
	return rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: s.User}
}

func splitNS(s string) (string, string) {
	for i := range s {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return "default", s
}

func (g *guard) bind(ctx context.Context, name string, c contracts.Contract) error {
	meta := metav1.ObjectMeta{Name: name, Labels: map[string]string{contracts.Label: c.Name}}
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: c.Spec.ClusterRole}
	subj := []rbacv1.Subject{subject(c.Spec)}
	if c.Spec.Namespace != "" {
		meta.Namespace = c.Spec.Namespace
		_, err := g.cs.RbacV1().RoleBindings(c.Spec.Namespace).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: meta, RoleRef: ref, Subjects: subj}, metav1.CreateOptions{FieldManager: "gachi-referee"})
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	_, err := g.cs.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: meta, RoleRef: ref, Subjects: subj}, metav1.CreateOptions{FieldManager: "gachi-referee"})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (g *guard) unbind(ctx context.Context, name, namespace string) {
	var err error
	if namespace != "" {
		err = g.cs.RbacV1().RoleBindings(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		err = g.cs.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		log.Printf("unbinding %s: %v", name, err)
	}
}

// lockers keep each Locker's Secret readable by its service accounts: a
// Role with the one Secret, bound to them.
func (g *guard) lockers(ctx context.Context) {
	list, err := g.dyn.Resource(lockersGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, l := range list.Items {
		secret, _, _ := unstructured.NestedString(l.Object, "spec", "secret")
		sas, _, _ := unstructured.NestedStringSlice(l.Object, "spec", "serviceAccounts")
		ns, name := l.GetNamespace(), "locker-"+l.GetName()
		owner := []metav1.OwnerReference{{APIVersion: belts.Group + "/" + belts.Version, Kind: "Locker", Name: l.GetName(), UID: l.GetUID()}}
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: owner},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{secret}, Verbs: []string{"get"}}}}
		var subj []rbacv1.Subject
		for _, sa := range sas {
			subj = append(subj, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: sa})
		}
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: owner},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}, Subjects: subj}
		if _, err := g.cs.RbacV1().Roles(ns).Update(ctx, role, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
			_, _ = g.cs.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{})
		}
		if _, err := g.cs.RbacV1().RoleBindings(ns).Update(ctx, rb, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
			_, _ = g.cs.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{})
		}
	}
}
