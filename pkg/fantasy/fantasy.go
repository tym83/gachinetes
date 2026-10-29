// Package fantasy runs a copy of a cluster's control plane, seeded from
// its tape, plays a change out on it and tells what came of it.
package fantasy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tym83/gachinetes/pkg/tape"
	"github.com/tym83/gachinetes/pkg/wrestling"
)

// Plantable tells whether a stored value goes into a fantasy: not what
// the cluster encrypted with keys the copy does not hold, nor events,
// leases of the real control plane and the API server's own bookkeeping,
// which the copy keeps afresh.
func Plantable(key string, value []byte) bool {
	if bytes.HasPrefix(value, []byte("k8s:enc:")) {
		return false
	}
	for _, p := range []string{
		"/registry/events/", "/registry/leases/", "/registry/masterleases/", "/registry/peerserverleases/",
		"/registry/ranges/", "/registry/apiregistration.k8s.io/", "/registry/events.k8s.io/",
	} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}

// Ignored are what a copy rewrites by itself all the time, and says
// nothing about the change: leases, events, node heartbeats.
func Ignored(key string) bool {
	for _, p := range []string{"/registry/leases/", "/registry/events/", "/registry/events.k8s.io/",
		"/registry/masterleases/", "/registry/peerserverleases/", "/registry/minions/"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// WaitQuiet waits until the store has had no write that matters for
// quiet, and returns the revision it rests at. It reads the tape: writes
// to ignored keys do not count.
func WaitQuiet(ctx context.Context, s *tape.Store, quiet, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	info, err := s.Info(ctx)
	if err != nil {
		return 0, err
	}
	rest, since := info.Current, time.Now()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		cur, err := s.Info(ctx)
		if err != nil {
			return 0, err
		}
		if cur.Current != rest {
			d, err := tape.DiffBetween(ctx, s, "/registry/", tape.Point{Rev: rest}, tape.Point{Rev: cur.Current}, tape.Filter{})
			if err != nil {
				return 0, err
			}
			if meaningful(d) {
				since = time.Now()
			}
			rest = cur.Current
		}
		if time.Since(since) >= quiet {
			return rest, nil
		}
		if time.Now().After(deadline) {
			return rest, fmt.Errorf("not quiet after %s", timeout)
		}
	}
}

func meaningful(d tape.Diff) bool {
	for _, set := range [][]tape.Change{d.Added, d.Removed, d.Changed} {
		for _, c := range set {
			if !Ignored(c.Key) {
				return true
			}
		}
	}
	return false
}

// Item is an object in the report.
type Item struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Node: where a pod landed, or was before it went.
	Node string `json:"node,omitempty"`
	// Fields that changed, for a changed object.
	Fields []string `json:"fields,omitempty"`
}

// Report is what came of a change.
type Report struct {
	Changes []string `json:"changes"`
	// Branch is the cluster's revision the copy started from; Settled and
	// Over are the copy's own, before and after the change.
	Branch  int64  `json:"branch"`
	Settled int64  `json:"settled"`
	Over    int64  `json:"over"`
	Added   []Item `json:"added"`
	Changed []Item `json:"changed"`
	Removed []Item `json:"removed"`
	// Pending are pods that came and found no place.
	Pending []Item `json:"pending"`
}

// Summarize turns the copy's diff over the change into a report.
func Summarize(d tape.Diff) *Report {
	r := &Report{Added: []Item{}, Changed: []Item{}, Removed: []Item{}, Pending: []Item{}}
	for _, c := range d.Added {
		if Ignored(c.Key) || c.To == nil || c.To.Object == nil {
			continue
		}
		it := item(c.To.Object)
		r.Added = append(r.Added, it)
		if it.Kind == "Pod" && it.Node == "" {
			r.Pending = append(r.Pending, it)
		}
	}
	for _, c := range d.Removed {
		if Ignored(c.Key) || c.From == nil || c.From.Object == nil {
			continue
		}
		r.Removed = append(r.Removed, item(c.From.Object))
	}
	for _, c := range d.Changed {
		if Ignored(c.Key) || c.To == nil || c.From == nil || c.To.Object == nil || c.From.Object == nil {
			continue
		}
		it := item(c.To.Object)
		it.Fields = fields(c.From.Object, c.To.Object)
		if len(it.Fields) == 0 {
			continue
		}
		r.Changed = append(r.Changed, it)
	}
	for _, set := range [][]Item{r.Added, r.Changed, r.Removed, r.Pending} {
		sort.Slice(set, func(i, j int) bool { return set[i].key() < set[j].key() })
	}
	return r
}

func (i Item) key() string { return i.Kind + "/" + i.Namespace + "/" + i.Name }

func item(obj map[string]any) Item {
	u := unstructured.Unstructured{Object: obj}
	it := Item{Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
	if it.Kind == "Pod" {
		it.Node, _, _ = unstructured.NestedString(obj, "spec", "nodeName")
	}
	return it
}

// fields are the top paths that changed, but for what every write
// changes.
func fields(a, b map[string]any) []string {
	var out []string
	for _, p := range wrestling.Changed(a, b) {
		if p == "metadata.resourceVersion" || p == "metadata.managedFields" || strings.HasPrefix(p, "metadata.managedFields.") ||
			p == "metadata.generation" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Text is the report for a person.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "♂ Deep dark fantasy from revision %d: %s\n\n", r.Branch, strings.Join(r.Changes, ", "))
	section := func(title, mark string, items []Item) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s (%d)\n", title, len(items))
		for _, it := range items {
			name := it.Name
			if it.Namespace != "" {
				name = it.Namespace + "/" + it.Name
			}
			line := fmt.Sprintf("  %s %s %s", mark, it.Kind, name)
			if it.Node != "" {
				line += " → " + it.Node
			}
			if len(it.Fields) > 0 {
				f := it.Fields
				if len(f) > 4 {
					f = append(f[:4:4], fmt.Sprintf("+%d", len(it.Fields)-4))
				}
				line += "  [" + strings.Join(f, ", ") + "]"
			}
			fmt.Fprintln(&b, line)
		}
		b.WriteString("\n")
	}
	section("Would be made", "+", r.Added)
	section("Would change", "~", r.Changed)
	section("Would go", "−", r.Removed)
	section("Would find no place", "!", r.Pending)
	if len(r.Added)+len(r.Changed)+len(r.Removed) == 0 {
		b.WriteString("Nothing would happen.\n")
	}
	return b.String()
}

// ControlPlane is the copy: a store, an API server, the controllers and
// the scheduler, and kwok for the nodes, as processes under Work.
type ControlPlane struct {
	Work, Bin string
	// Base is the first of the ports the copy takes.
	Base  int
	procs []*exec.Cmd
}

func (c *ControlPlane) port(n int) int { return c.Base + n }

// StoreURL is the copy's store, as an etcd client reaches it.
func (c *ControlPlane) StoreURL() string { return fmt.Sprintf("http://127.0.0.1:%d", c.port(1)) }

// TapeURL is the copy's tape.
func (c *ControlPlane) TapeURL() string { return fmt.Sprintf("http://127.0.0.1:%d", c.port(3)) }

// Kubeconfig is the copy's admin kubeconfig.
func (c *ControlPlane) Kubeconfig() string { return filepath.Join(c.Work, "kube", "super-admin.conf") }

func (c *ControlPlane) start(name string, args ...string) error {
	if err := os.MkdirAll(filepath.Join(c.Work, "logs"), 0o700); err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(c.Work, "logs", name+".log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(c.Bin, name), args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	c.procs = append(c.procs, cmd)
	return nil
}

// StartStore starts the copy's store, with its own tape.
func (c *ControlPlane) StartStore(ctx context.Context) error {
	if err := c.start("kuberoot-store", "--name", "fantasy", "--data-dir", filepath.Join(c.Work, "store"),
		"--listen-client-urls", c.StoreURL(), "--listen-peer-urls", fmt.Sprintf("http://127.0.0.1:%d", c.port(2)),
		"--keep-history", "--listen-tape-urls", c.TapeURL()); err != nil {
		return err
	}
	return waitFor(ctx, 30*time.Second, func() error {
		_, err := (&tape.Store{URL: c.TapeURL()}).Info(ctx)
		return err
	})
}

// StartControlPlane makes certificates, starts the API server, the
// controllers, the scheduler and kwok, and waits until the API answers.
func (c *ControlPlane) StartControlPlane(ctx context.Context) error {
	pki, kube := filepath.Join(c.Work, "pki"), filepath.Join(c.Work, "kube")
	api := fmt.Sprintf("127.0.0.1:%d", c.port(4))
	if _, err := os.Stat(filepath.Join(pki, "ca.crt")); err != nil {
		for _, phase := range [][]string{
			{"init", "phase", "certs", "ca", "--cert-dir", pki},
			{"init", "phase", "certs", "front-proxy-ca", "--cert-dir", pki},
			{"init", "phase", "certs", "front-proxy-client", "--cert-dir", pki},
			{"init", "phase", "certs", "sa", "--cert-dir", pki},
			{"init", "phase", "certs", "apiserver", "--cert-dir", pki, "--apiserver-advertise-address", "127.0.0.1"},
			{"init", "phase", "kubeconfig", "super-admin", "--cert-dir", pki, "--kubeconfig-dir", kube, "--control-plane-endpoint", api},
			{"init", "phase", "kubeconfig", "controller-manager", "--cert-dir", pki, "--kubeconfig-dir", kube, "--control-plane-endpoint", api},
			{"init", "phase", "kubeconfig", "scheduler", "--cert-dir", pki, "--kubeconfig-dir", kube, "--control-plane-endpoint", api},
		} {
			if out, err := exec.CommandContext(ctx, filepath.Join(c.Bin, "kubeadm"), phase...).CombinedOutput(); err != nil {
				return fmt.Errorf("kubeadm %s: %v: %s", strings.Join(phase[:4], " "), err, out)
			}
		}
	}
	p := func(f string) string { return filepath.Join(pki, f) }
	if err := c.start("kube-apiserver", "--etcd-servers="+c.StoreURL(), "--etcd-compaction-interval=0",
		"--advertise-address=127.0.0.1", "--bind-address=127.0.0.1", fmt.Sprintf("--secure-port=%d", c.port(4)),
		"--service-cluster-ip-range=10.96.0.0/16",
		"--client-ca-file="+p("ca.crt"), "--tls-cert-file="+p("apiserver.crt"), "--tls-private-key-file="+p("apiserver.key"),
		"--service-account-issuer=https://kubernetes.default.svc", "--service-account-key-file="+p("sa.pub"),
		"--service-account-signing-key-file="+p("sa.key"), "--authorization-mode=Node,RBAC",
		// Webhooks of the real cluster point at services the copy cannot
		// reach; admission by webhook is the real cluster's to do.
		"--disable-admission-plugins=ValidatingAdmissionWebhook,MutatingAdmissionWebhook",
		"--requestheader-client-ca-file="+p("front-proxy-ca.crt"), "--requestheader-allowed-names=front-proxy-client",
		"--requestheader-group-headers=X-Remote-Group", "--requestheader-username-headers=X-Remote-User",
		"--proxy-client-cert-file="+p("front-proxy-client.crt"), "--proxy-client-key-file="+p("front-proxy-client.key"),
	); err != nil {
		return err
	}
	admin := c.Kubeconfig()
	if err := waitFor(ctx, 2*time.Minute, func() error {
		return exec.CommandContext(ctx, filepath.Join(c.Bin, "kubectl"), "--kubeconfig", admin, "get", "--raw", "/readyz").Run()
	}); err != nil {
		return fmt.Errorf("the API server did not come up: %w", err)
	}
	cm := filepath.Join(kube, "controller-manager.conf")
	if err := c.start("kube-controller-manager", "--kubeconfig="+cm, "--authentication-kubeconfig="+cm,
		"--authorization-kubeconfig="+cm, "--service-account-private-key-file="+p("sa.key"), "--root-ca-file="+p("ca.crt"),
		"--cluster-signing-cert-file="+p("ca.crt"), "--cluster-signing-key-file="+p("ca.key"),
		"--use-service-account-credentials", "--leader-elect=false", fmt.Sprintf("--secure-port=%d", c.port(5))); err != nil {
		return err
	}
	sc := filepath.Join(kube, "scheduler.conf")
	if err := c.start("kube-scheduler", "--kubeconfig="+sc, "--authentication-kubeconfig="+sc, "--authorization-kubeconfig="+sc,
		"--leader-elect=false", fmt.Sprintf("--secure-port=%d", c.port(6))); err != nil {
		return err
	}
	// kwok plays every node of the copy: it keeps them Ready and runs
	// the pods bound to them.
	return c.start("kwok", "--kubeconfig="+admin, "--manage-all-nodes=true",
		fmt.Sprintf("--server-address=127.0.0.1:%d", c.port(7)))
}

// Stop stops the copy.
func (c *ControlPlane) Stop() {
	for i := len(c.procs) - 1; i >= 0; i-- {
		_ = c.procs[i].Process.Kill()
		_, _ = c.procs[i].Process.Wait()
	}
}

func waitFor(ctx context.Context, timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
