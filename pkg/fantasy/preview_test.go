package fantasy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
)

func TestParsePullRequest(t *testing.T) {
	for in, want := range map[string]string{
		"12":                 "pr-12",
		"#12":                "pr-12",
		"tym83/gachinetes#7": "pr-gachinetes-7",
		"https://github.com/tym83/gachinetes/pull/7":  "pr-gachinetes-7",
		"https://github.com/Org/My_Repo.v2/pull/123/": "pr-my-repo-v2-123",
		"o/" + strings.Repeat("x", 80) + "#5":         "pr-" + strings.Repeat("x", 35) + "-5",
	} {
		p, err := ParsePullRequest(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := p.PreviewName(); got != want || len(got) > 40 {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "0", "main", "https://github.com/a/b/issues/3"} {
		if _, err := ParsePullRequest(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.yaml", "kind: ConfigMap")
	write("apps/web deploy.yml", "kind: Deployment")
	write("README.md", "not a manifest")
	write(".git/config.yaml", "kind: Hidden")
	m, keys, err := Manifests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "apps__web_deploy.yml,b.yaml" || len(m) != 2 || m["b.yaml"] != "kind: ConfigMap" {
		t.Fatalf("keys %v, manifests %v", keys, m)
	}
	if _, _, err := Manifests(t.TempDir()); err == nil {
		t.Fatal("an empty directory")
	}
	write("big.json", strings.Repeat("x", manifestsLimit))
	if _, _, err := Manifests(dir); err == nil {
		t.Fatal("more than a ConfigMap holds")
	}
}

func testPreview() Preview {
	return Preview{Name: "pr-gachinetes-7", Namespace: PreviewNamespace, PR: PullRequest{Repo: "tym83/gachinetes", Number: 7},
		Image: "img", TTL: 2 * time.Hour, Quiet: 20 * time.Second, Timeout: 10 * time.Minute}
}

func TestPreviewJob(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	j := testPreview().Job([]string{"a.yaml", "b.yaml"}, now)
	if *j.Spec.ActiveDeadlineSeconds != int64((2*time.Hour+25*time.Minute)/time.Second) || *j.Spec.BackoffLimit != 0 ||
		*j.Spec.TTLSecondsAfterFinished != 600 {
		t.Fatalf("job spec: %+v", j.Spec)
	}
	if j.Annotations[ExpiresAnno] != "2026-11-08T14:00:00Z" || j.Annotations[PullRequestAnno] != "tym83/gachinetes#7" {
		t.Fatalf("annotations: %v", j.Annotations)
	}
	args := strings.Join(j.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"--preview=pr-gachinetes-7", "-f=/manifests/a.yaml,/manifests/b.yaml", "--ttl=2h0m0s",
		"--listen=0.0.0.0", "--san=pr-gachinetes-7.gachi-fantasies.svc"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %s: no %s", args, want)
		}
	}
	if strings.Contains(args, "--at") {
		t.Error("--at without a point")
	}
}

// The whole round: launched, played out (here by hand, as the Job's pod
// does it), read back, launched again for a new push, discarded.
func TestPreviewRound(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	p := testPreview()
	manifests := map[string]string{"a.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n"}
	if err := Launch(ctx, cs, p, manifests, []string{"a.yaml"}); err != nil {
		t.Fatal(err)
	}
	job, err := cs.BatchV1().Jobs(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cs.CoreV1().Services(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil || svc.Spec.Selector[PreviewLabel] != p.Name || svc.Spec.Ports[0].TargetPort.IntValue() != previewPortBase+4 {
		t.Fatalf("service: %+v %v", svc, err)
	}
	if job.Spec.Template.Labels[PreviewLabel] != p.Name {
		t.Fatal("the Service does not select the Job's pod")
	}
	for _, name := range []string{p.Name + "-manifests", p.Name + "-report"} {
		cm, err := cs.CoreV1().ConfigMaps(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil || len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "Job" || cm.OwnerReferences[0].Name != p.Name {
			t.Fatalf("%s: %+v %v", name, cm, err)
		}
	}
	if st, err := Status(ctx, cs, p.Namespace, p.Name); err != nil || st.Status != statusPending {
		t.Fatalf("before playing out: %+v %v", st, err)
	}

	// The pod's side.
	dir := t.TempDir()
	if err := makePKI(dir+"/pki", dir+"/kube", 23004, nil); err != nil {
		t.Fatal(err)
	}
	admin, _ := os.ReadFile(dir + "/kube/super-admin.conf")
	if err := PublishKubeconfig(ctx, cs, p.Namespace, p.Name, admin); err != nil {
		t.Fatal(err)
	}
	rep := &Report{Changes: []string{"/manifests/a.yaml"}, Branch: 41, Added: []Item{{Kind: "ConfigMap", Namespace: "default", Name: "a"}}}
	if err := PublishReport(ctx, cs, p.Namespace, p.Name, rep, nil); err != nil {
		t.Fatal(err)
	}

	st, err := Wait(ctx, cs, p.Namespace, p.Name, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if st.Report == nil || st.Report.Branch != 41 || !strings.Contains(st.ReportText, "+ ConfigMap default/a") || st.PullRequest != "tym83/gachinetes#7" {
		t.Fatalf("status: %+v", st)
	}
	in, err := clientcmd.Load(st.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	fwd, err := clientcmd.Load(st.ForwardKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range in.Clusters {
		if c.Server != "https://pr-gachinetes-7.gachi-fantasies.svc:6443" || c.TLSServerName != "" {
			t.Fatalf("in the cluster: %+v", c)
		}
	}
	for _, c := range fwd.Clusters {
		if c.Server != "https://127.0.0.1:16443" || c.TLSServerName != "pr-gachinetes-7.gachi-fantasies.svc" {
			t.Fatalf("through a port-forward: %+v", c)
		}
	}
	if all, err := List(ctx, cs, p.Namespace); err != nil || len(all) != 1 || all[0].Status != StatusReady {
		t.Fatalf("list: %+v %v", all, err)
	}

	// A new push: the preview starts over.
	manifests["a.yaml"] += "data: {v: '2'}\n"
	if err := Launch(ctx, cs, p, manifests, []string{"a.yaml"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := Status(ctx, cs, p.Namespace, p.Name); st.Status != statusPending {
		t.Fatalf("after a new push: %s", st.Status)
	}
	got, _, err := ManifestsOf(ctx, cs, p.Namespace, p.Name)
	if err != nil || !strings.Contains(got["a.yaml"], "v: '2'") {
		t.Fatalf("manifests after a new push: %v %v", got, err)
	}

	// A failed play-out says why.
	if err := PublishReport(ctx, cs, p.Namespace, p.Name, nil, errTest("not quiet after 10m")); err != nil {
		t.Fatal(err)
	}
	if _, err := Wait(ctx, cs, p.Namespace, p.Name, time.Second, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not quiet") {
		t.Fatalf("a failed preview: %v", err)
	}

	if err := Discard(ctx, cs, p.Namespace, p.Name); err != nil {
		t.Fatal(err)
	}
	if all, _ := List(ctx, cs, p.Namespace); len(all) != 0 {
		t.Fatalf("after discard: %+v", all)
	}
	if err := Discard(ctx, cs, p.Namespace, p.Name); err != nil {
		t.Fatalf("discarding twice: %v", err)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestJobFailureIsAFailedPreview(t *testing.T) {
	ctx := context.Background()
	p := testPreview()
	job := p.Job([]string{"a.yaml"}, time.Now())
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	cs := fake.NewSimpleClientset(job)
	st, err := Status(ctx, cs, p.Namespace, p.Name)
	if err != nil || st.Status != StatusFailed || !strings.Contains(st.Error, "DeadlineExceeded") {
		t.Fatalf("%+v %v", st, err)
	}
}

// Merging over an object the cluster wrote after the branch is refused,
// before anything is applied.
func TestMergeRefusesWhatMovedOn(t *testing.T) {
	ctx := context.Background()
	objs, err := ObjectsFrom(map[string]string{
		"a.yaml": "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: a, namespace: shop}\n",
	}, []string{"a.yaml"})
	if err != nil || len(objs) != 1 || objs[0].GetName() != "a" {
		t.Fatalf("objects: %v %v", objs, err)
	}
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{
		{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: []string{"get", "patch"}}}}}
	live := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "shop", "resourceVersion": "50"}}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{{Version: "v1", Resource: "configmaps"}: "ConfigMapList"}, live)
	if _, err := Merge(ctx, cs, dyn, objs, 41); err == nil || !strings.Contains(err.Error(), "after the fantasy branched at 41") {
		t.Fatalf("merge over a newer object: %v", err)
	}
	cfls, err := Conflicts(ctx, cs, dyn, objs, 60)
	if err != nil || len(cfls) != 0 {
		t.Fatalf("an object older than the branch: %v %v", cfls, err)
	}
}
