package belts

import (
	"strings"
	"testing"
)

var policy = Policy{
	Name:  "replicas-to-hpa",
	Match: Match{Group: "apps", Kind: "Deployment", Namespaces: []string{"shop"}},
	Belts: []Belt{
		{Path: "spec.replicas", Holders: Holders{Managers: []string{"hpa"}}},
		{Path: "spec.template.spec.containers", Holders: Holders{Users: []string{"system:serviceaccount:ci:deployer"}}, Action: Warn},
	},
}

func write(manager, user string, oldReplicas, newReplicas int64, image string) Write {
	obj := func(r int64, img string) map[string]any {
		return map[string]any{"spec": map[string]any{
			"replicas": r,
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "web", "image": img}}}},
		}}
	}
	return Write{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web", Manager: manager, User: user,
		Old: obj(oldReplicas, "nginx:1"), New: obj(newReplicas, image)}
}

func TestJudge(t *testing.T) {
	ps := []Policy{policy}
	if v := Judge(ps, write("hpa", "system:serviceaccount:kube-system:hpa", 2, 5, "nginx:1")); !v.Allowed {
		t.Fatalf("the holder: %+v", v)
	}
	v := Judge(ps, write("argocd", "system:serviceaccount:argocd:app", 2, 3, "nginx:1"))
	if v.Allowed || !strings.Contains(v.Message, "spec.replicas") || !strings.Contains(v.Message, "manager hpa") {
		t.Fatalf("a challenger: %+v", v)
	}
	v = Judge(ps, write("kubectl-edit", "alice", 2, 2, "nginx:2"))
	if !v.Allowed || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "containers") {
		t.Fatalf("a warned belt: %+v", v)
	}
	if v := Judge(ps, write("argocd", "x", 2, 2, "nginx:1")); !v.Allowed || len(v.Warnings) != 0 {
		t.Fatalf("a write that touches no belt: %+v", v)
	}
	other := write("argocd", "x", 2, 3, "nginx:1")
	other.Namespace = "prod"
	if v := Judge(ps, other); !v.Allowed {
		t.Fatalf("another namespace: %+v", v)
	}
	created := write("argocd", "x", 2, 3, "nginx:1")
	created.Old = nil
	if v := Judge(ps, created); !v.Allowed {
		t.Fatalf("a create: %+v", v)
	}
	group := write("", "bob", 2, 3, "nginx:1")
	group.Groups = []string{"sre"}
	ps[0].Belts[0].Holders.Groups = []string{"sre"}
	if v := Judge(ps, group); !v.Allowed {
		t.Fatalf("a holder by group: %+v", v)
	}
}
