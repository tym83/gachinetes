package fantasy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// A preview is a fantasy kept running for a pull request: a Job in the
// previews' namespace plays the request's manifests out on a branch of the
// tape, publishes the report and a kubeconfig of the branch's API server,
// and keeps the branch up until its time to live runs out. Everything else
// of the preview (its manifests, Service, report and kubeconfig) is owned
// by the Job, so that deleting the Job, or the Job running out, takes it
// all away.

const (
	// PreviewNamespace is where previews run by default.
	PreviewNamespace = "gachi-fantasies"
	// PreviewServiceAccount may read the raw tape and write its preview's
	// report and kubeconfig (deploy/fantasy-preview.yaml).
	PreviewServiceAccount = "gachi-fantasy-preview"
	// PreviewImage holds gachi-fantasy, kuberoot-store, the control plane's
	// binaries and kwok (Dockerfile.preview).
	PreviewImage = "ghcr.io/tym83/gachinetes/gachi-fantasy-preview:latest"

	PreviewLabel      = "fantasy.gachinetes.dev/preview"
	PullRequestAnno   = "fantasy.gachinetes.dev/pull-request"
	ExpiresAnno       = "fantasy.gachinetes.dev/expires"
	previewAPIPort    = 6443
	previewPortBase   = 23000
	ForwardPort       = 16443
	manifestsLimit    = 900 << 10
	statusPending     = "pending"
	StatusReady       = "ready"
	StatusFailed      = "failed"
	keyStatus         = "status"
	keyError          = "error"
	keyReport         = "report.json"
	keyReportText     = "report.txt"
	keyKubeconfig     = "kubeconfig"
	keyForwardConfig  = "kubeconfig-port-forward"
	finishedRetention = 10 * time.Minute
)

// PullRequest is what a preview is for.
type PullRequest struct {
	// Repo is owner/name, when known.
	Repo   string
	Number int
}

var (
	prURL   = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)/?$`)
	prShort = regexp.MustCompile(`^(?:([\w.-]+/[\w.-]+))?#?(\d+)$`)
	notName = regexp.MustCompile(`[^a-z0-9-]+`)
	notKey  = regexp.MustCompile(`[^-._a-zA-Z0-9]`)
)

// ParsePullRequest reads 12, #12, owner/repo#12 or a pull request's URL.
func ParsePullRequest(s string) (PullRequest, error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[2])
		return PullRequest{Repo: m[1], Number: n}, nil
	}
	if m := prShort.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[2])
		if n > 0 {
			return PullRequest{Repo: m[1], Number: n}, nil
		}
	}
	return PullRequest{}, fmt.Errorf("%q: a pull request's number, owner/repo#N or URL expected", s)
}

func (p PullRequest) String() string {
	if p.Repo == "" {
		return "#" + strconv.Itoa(p.Number)
	}
	return p.Repo + "#" + strconv.Itoa(p.Number)
}

// PreviewName is the name of the request's preview: pr-<repo>-<n>, short
// enough for the names derived from it.
func (p PullRequest) PreviewName() string {
	repo := ""
	if _, name, ok := strings.Cut(p.Repo, "/"); ok {
		repo = strings.Trim(notName.ReplaceAllString(strings.ToLower(name), "-"), "-")
	}
	n := strconv.Itoa(p.Number)
	if max := 40 - len("pr--") - len(n); len(repo) > max {
		repo = strings.Trim(repo[:max], "-")
	}
	if repo == "" {
		return "pr-" + n
	}
	return "pr-" + repo + "-" + n
}

// Manifests reads the manifests under dir (*.yaml, *.yml, *.json, at any
// depth), keyed for a ConfigMap by their paths, and the keys in the order
// they are applied: by path.
func Manifests(dir string) (map[string]string, []string, error) {
	out := map[string]string{}
	var keys []string
	size := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		key := strings.ReplaceAll(filepath.ToSlash(rel), "/", "__")
		key = notKey.ReplaceAllString(key, "_")
		if _, dup := out[key]; dup {
			return fmt.Errorf("%s: two manifests come to the key %s", path, key)
		}
		size += len(raw)
		out[key] = string(raw)
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("%s holds no manifests (*.yaml, *.yml, *.json)", dir)
	}
	if size > manifestsLimit {
		return nil, nil, fmt.Errorf("%s: %d bytes of manifests, more than a ConfigMap holds (%d)", dir, size, manifestsLimit)
	}
	sort.Strings(keys)
	return out, keys, nil
}

// Preview is a preview to launch.
type Preview struct {
	Name, Namespace string
	PR              PullRequest
	Image           string
	TTL             time.Duration
	// At is where the branch starts: a revision or an RFC 3339 time;
	// empty is now.
	At             string
	Quiet, Timeout time.Duration
}

func (p Preview) labels() map[string]string { return map[string]string{PreviewLabel: p.Name} }

// Job is the preview's Job, running gachi-fantasy on the manifests under
// keys.
func (p Preview) Job(keys []string, now time.Time) *batchv1.Job {
	var files []string
	var items []corev1.KeyToPath
	for _, k := range keys {
		files = append(files, "/manifests/"+k)
		items = append(items, corev1.KeyToPath{Key: k, Path: k})
	}
	svc := p.Name + "." + p.Namespace + ".svc"
	args := []string{"--preview=" + p.Name, "--preview-namespace=" + p.Namespace, "-f=" + strings.Join(files, ","),
		"--work=/work", "--bin=/usr/local/bin", "--listen=0.0.0.0", "--san=" + svc, "--san=" + svc + ".cluster.local",
		"--port-base=" + strconv.Itoa(previewPortBase), "--ttl=" + p.TTL.String(),
		"--until-quiet=" + p.Quiet.String(), "--timeout=" + p.Timeout.String()}
	if p.At != "" {
		args = append(args, "--at="+p.At)
	}
	// The Job's own deadline is the backstop for the TTL: setting up and
	// playing out come on top of it.
	deadline := int64((p.TTL + p.Timeout + 15*time.Minute) / time.Second)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.labels(),
			Annotations: map[string]string{PullRequestAnno: p.PR.String(), ExpiresAnno: now.Add(p.TTL).UTC().Format(time.RFC3339)}},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: ptr.To(int32(finishedRetention / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: p.labels()},
				Spec: corev1.PodSpec{
					ServiceAccountName: PreviewServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext:    &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "fantasy", Image: p.Image, Command: []string{"/usr/local/bin/gachi-fantasy"}, Args: args,
						Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: int32(previewPortBase + 4)}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "manifests", MountPath: "/manifests", ReadOnly: true},
							{Name: "work", MountPath: "/work"},
							{Name: "tmp", MountPath: "/tmp"},
						},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, ReadOnlyRootFilesystem: ptr.To(true)},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "manifests", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: p.Name + "-manifests"}, Items: items}}},
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// Owned are the preview's objects besides the Job, owned by it: the
// manifests, the Service in front of the branch's API server, the report
// and the kubeconfig, the last two pending until the Job fills them.
func (p Preview) Owned(job *batchv1.Job, manifests map[string]string) (*corev1.ConfigMap, *corev1.Service, *corev1.ConfigMap, *corev1.Secret) {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: p.labels(),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}
	}
	cm := &corev1.ConfigMap{ObjectMeta: meta(p.Name + "-manifests"), Data: manifests}
	svc := &corev1.Service{ObjectMeta: meta(p.Name), Spec: corev1.ServiceSpec{
		Selector: p.labels(),
		Ports:    []corev1.ServicePort{{Name: "https", Port: previewAPIPort, TargetPort: intstr.FromInt32(previewPortBase + 4)}},
	}}
	report := &corev1.ConfigMap{ObjectMeta: meta(p.Name + "-report"), Data: map[string]string{keyStatus: statusPending}}
	kc := &corev1.Secret{ObjectMeta: meta(p.Name + "-kubeconfig"), Type: corev1.SecretTypeOpaque}
	return cm, svc, report, kc
}

// Launch starts the preview, in place of the one the request had: a new
// push plays out from scratch.
func Launch(ctx context.Context, cs kubernetes.Interface, p Preview, manifests map[string]string, keys []string) error {
	if err := Discard(ctx, cs, p.Namespace, p.Name); err != nil {
		return err
	}
	// The old preview's objects go with its Job, in the background; the
	// new ones take the same names.
	if err := waitGone(ctx, cs, p.Namespace, p.Name); err != nil {
		return err
	}
	job, err := cs.BatchV1().Jobs(p.Namespace).Create(ctx, p.Job(keys, time.Now()), metav1.CreateOptions{FieldManager: "gachictl"})
	if err != nil {
		return err
	}
	cm, svc, report, kc := p.Owned(job, manifests)
	if _, err := cs.CoreV1().ConfigMaps(p.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return err
	}
	if _, err := cs.CoreV1().Services(p.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return err
	}
	if _, err := cs.CoreV1().ConfigMaps(p.Namespace).Create(ctx, report, metav1.CreateOptions{}); err != nil {
		return err
	}
	_, err = cs.CoreV1().Secrets(p.Namespace).Create(ctx, kc, metav1.CreateOptions{})
	return err
}

// waitGone waits until the preview's Job and the objects it owned are gone.
func waitGone(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, e1 := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		_, e2 := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-report", metav1.GetOptions{})
		_, e3 := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(e1) && apierrors.IsNotFound(e2) && apierrors.IsNotFound(e3) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous preview %s is still going away", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Discard deletes the preview: its Job and what it owns. The garbage
// collector would take the owned objects too; deleting them here lets a new
// preview take their names at once.
func Discard(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	bg := metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}
	for _, del := range []func() error{
		func() error { return cs.BatchV1().Jobs(ns).Delete(ctx, name, bg) },
		func() error { return cs.CoreV1().Services(ns).Delete(ctx, name, bg) },
		func() error { return cs.CoreV1().ConfigMaps(ns).Delete(ctx, name+"-manifests", bg) },
		func() error { return cs.CoreV1().ConfigMaps(ns).Delete(ctx, name+"-report", bg) },
		func() error { return cs.CoreV1().Secrets(ns).Delete(ctx, name+"-kubeconfig", bg) },
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// PreviewStatus is where a preview is.
type PreviewStatus struct {
	Name        string    `json:"name"`
	PullRequest string    `json:"pullRequest"`
	Status      string    `json:"status"`
	Expires     time.Time `json:"expires"`
	Error       string    `json:"error,omitempty"`
	Report      *Report   `json:"report,omitempty"`
	ReportText  string    `json:"-"`
	// Endpoint is the branch's API server in the cluster.
	Endpoint   string `json:"endpoint"`
	Kubeconfig []byte `json:"-"`
	// ForwardKubeconfig reaches it through kubectl port-forward.
	ForwardKubeconfig []byte `json:"-"`
}

// Status reads the preview's state.
func Status(ctx context.Context, cs kubernetes.Interface, ns, name string) (PreviewStatus, error) {
	st := PreviewStatus{Name: name, Endpoint: fmt.Sprintf("https://%s.%s.svc:%d", name, ns, previewAPIPort)}
	job, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return st, err
	}
	st.PullRequest = job.Annotations[PullRequestAnno]
	st.Expires, _ = time.Parse(time.RFC3339, job.Annotations[ExpiresAnno])
	st.Status = statusPending
	if cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-report", metav1.GetOptions{}); err == nil {
		st.Status, st.Error, st.ReportText = cm.Data[keyStatus], cm.Data[keyError], cm.Data[keyReportText]
		if raw := cm.Data[keyReport]; raw != "" {
			st.Report = &Report{}
			if err := json.Unmarshal([]byte(raw), st.Report); err != nil {
				return st, fmt.Errorf("the report of %s: %w", name, err)
			}
		}
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && st.Status != StatusFailed {
			st.Status = StatusFailed
			if st.Error == "" {
				st.Error = "the preview's Job failed: " + c.Reason + ": " + c.Message
			}
		}
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			st.Status = "expired"
		}
	}
	if st.Status == StatusReady {
		if s, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-kubeconfig", metav1.GetOptions{}); err == nil {
			st.Kubeconfig, st.ForwardKubeconfig = s.Data[keyKubeconfig], s.Data[keyForwardConfig]
		}
	}
	return st, nil
}

// Wait waits until the preview is ready or failed.
func Wait(ctx context.Context, cs kubernetes.Interface, ns, name string, timeout time.Duration, poll time.Duration) (PreviewStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := Status(ctx, cs, ns, name)
		if err != nil {
			return st, err
		}
		switch st.Status {
		case StatusReady:
			return st, nil
		case StatusFailed, "expired":
			return st, fmt.Errorf("preview %s: %s", name, st.Error)
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("preview %s not ready after %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// List is every preview in ns, by name.
func List(ctx context.Context, cs kubernetes.Interface, ns string) ([]PreviewStatus, error) {
	jobs, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: PreviewLabel})
	if err != nil {
		return nil, err
	}
	var out []PreviewStatus
	for _, j := range jobs.Items {
		st, err := Status(ctx, cs, ns, j.Name)
		if err != nil {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ManifestsOf reads a preview's manifests back, in the order applied.
func ManifestsOf(ctx context.Context, cs kubernetes.Interface, ns, name string) (map[string]string, []string, error) {
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-manifests", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return cm.Data, keys, nil
}

// PublishReport is the preview's side: the report, or why there is none.
func PublishReport(ctx context.Context, cs kubernetes.Interface, ns, name string, rep *Report, failure error) error {
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-report", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if failure != nil {
		cm.Data[keyStatus], cm.Data[keyError] = StatusFailed, failure.Error()
	} else {
		raw, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		cm.Data[keyStatus], cm.Data[keyReport], cm.Data[keyReportText] = StatusReady, string(raw), rep.Text()
	}
	_, err = cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "gachi-fantasy"})
	return err
}

// PublishKubeconfig is the preview's side: the admin kubeconfig of the
// branch, pointed at the Service, and at a port-forward of it.
func PublishKubeconfig(ctx context.Context, cs kubernetes.Interface, ns, name string, admin []byte) error {
	host := name + "." + ns + ".svc"
	inCluster, err := pointAt(admin, fmt.Sprintf("https://%s:%d", host, previewAPIPort), "")
	if err != nil {
		return err
	}
	forward, err := pointAt(admin, fmt.Sprintf("https://127.0.0.1:%d", ForwardPort), host)
	if err != nil {
		return err
	}
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return err
	}
	s.Data = map[string][]byte{keyKubeconfig: inCluster, keyForwardConfig: forward}
	_, err = cs.CoreV1().Secrets(ns).Update(ctx, s, metav1.UpdateOptions{FieldManager: "gachi-fantasy"})
	return err
}

// pointAt is the kubeconfig with every cluster at server, checked against
// the name tlsName when given.
func pointAt(kubeconfig []byte, server, tlsName string) ([]byte, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, err
	}
	if _, err := url.Parse(server); err != nil {
		return nil, err
	}
	for _, c := range cfg.Clusters {
		c.Server, c.TLSServerName = server, tlsName
	}
	return clientcmd.Write(*cfg)
}
