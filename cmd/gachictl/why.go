package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/tape"
	"github.com/tym83/gachinetes/pkg/wrestling"
)

var (
	boutsGVR    = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "bouts"}
	policiesGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "beltpolicies"}
)

func (a *app) clients() (dynamic.Interface, kubernetes.Interface, meta.RESTMapper, error) {
	cfg, err := a.flags.ToRESTConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := a.flags.ToRESTMapper()
	return dyn, cs, m, err
}

func (a *app) boutsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bouts",
		Short: "Fields controllers keep taking from each other",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dyn, _, _, err := a.clients()
			if err != nil {
				return err
			}
			list, err := dyn.Resource(boutsGVR).List(cmd.Context(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			if a.output != "" {
				return a.print(list.Items)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PHASE\tOBJECT\tFIELD\tFIGHT\tLAST")
			for _, b := range list.Items {
				phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
				path, _, _ := unstructured.NestedString(b.Object, "spec", "path")
				fight, _, _ := unstructured.NestedString(b.Object, "status", "summary")
				last, _, _ := unstructured.NestedString(b.Object, "status", "lastSeen")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", phase, boutObject(b), path, fight, last)
			}
			return w.Flush()
		},
	}
}

func boutObject(b unstructured.Unstructured) string {
	o, _, _ := unstructured.NestedStringMap(b.Object, "spec", "object")
	return wrestling.Ref{Group: o["group"], Kind: o["kind"], Namespace: o["namespace"], Name: o["name"]}.String()
}

func (a *app) beltsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "belts",
		Short: "Who holds which fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dyn, _, _, err := a.clients()
			if err != nil {
				return err
			}
			list, err := dyn.Resource(policiesGVR).List(cmd.Context(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "POLICY\tKIND\tFIELD\tHOLDER\tCHALLENGE")
			for i := range list.Items {
				p, err := belts.FromObject(&list.Items[i])
				if err != nil {
					return err
				}
				kind := p.Match.Kind
				if p.Match.Group != "" {
					kind += "." + p.Match.Group
				}
				for _, b := range p.Belts {
					action := b.Action
					if action == "" {
						action = belts.Deny
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, kind, b.Path, b.Holders, action)
				}
			}
			return w.Flush()
		},
	}
}

func (a *app) whyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "why KIND/NAME",
		Short: "Sorry for what? What is wrong with an object, and what led to it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, false)
			if err != nil {
				return err
			}
			out, err := a.why(cmd.Context(), t)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		},
	}
}

func (a *app) why(ctx context.Context, t target) (string, error) {
	dyn, cs, mapper, err := a.clients()
	if err != nil {
		return "", err
	}
	get := func(gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		var ri dynamic.ResourceInterface = dyn.Resource(m.Resource)
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			ri = dyn.Resource(m.Resource).Namespace(ns)
		}
		return ri.Get(ctx, name, metav1.GetOptions{})
	}
	kind, group, _ := strings.Cut(t.kind, ".")
	gvk, err := mapper.KindFor(schema.GroupVersionResource{Group: group, Resource: strings.ToLower(kind)})
	if err != nil {
		gvk = schema.GroupVersionKind{Group: group, Kind: kind}
		if ms, merr := mapper.RESTMappings(gvk.GroupKind()); merr == nil && len(ms) > 0 {
			gvk = ms[0].GroupVersionKind
		}
	}
	var b strings.Builder
	b.WriteString("Sorry for what?\n\n")
	obj, err := get(gvk, t.namespace, t.name)
	if apierrors.IsNotFound(err) {
		var graves []tape.Object
		if gerr := a.call(ctx, "/graveyard", t.query(nil), &graves); gerr == nil && len(graves) > 0 {
			g := graves[0]
			fmt.Fprintf(&b, "%s %s was deleted at %s (revision %d). Rest in peace — or: gachictl undelete %s/%s\n",
				t.kind, t.name, when(g.DeletedTime), g.DeletedAt, strings.ToLower(kind), t.name)
			return b.String(), nil
		}
		return "", fmt.Errorf("%s %s: not in the cluster, not in the graveyard", t.kind, t.name)
	}
	if err != nil {
		return "", err
	}
	events, _ := cs.CoreV1().Events(obj.GetNamespace()).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + obj.GetName() + ",involvedObject.kind=" + obj.GetKind()})
	var evs []corev1.Event
	if events != nil {
		evs = events.Items
	}
	problems := explain(obj, evs)
	fmt.Fprintf(&b, "%s %s\n", describeRef(obj), headline(obj, problems))
	for _, p := range problems {
		fmt.Fprintf(&b, "  ✗ %s\n", p)
	}

	// Who made it, and what change of theirs.
	chain := []string{}
	cur := obj
	for depth := 0; depth < 6; depth++ {
		ref := metav1.GetControllerOf(cur)
		if ref == nil {
			break
		}
		gv, _ := schema.ParseGroupVersion(ref.APIVersion)
		owner, err := get(gv.WithKind(ref.Kind), cur.GetNamespace(), ref.Name)
		if err != nil {
			chain = append(chain, fmt.Sprintf("← %s %s made it; it is gone now", ref.Kind, ref.Name))
			break
		}
		line := fmt.Sprintf("← %s made it at %s", describeRef(owner), cur.GetCreationTimestamp().Format(time.TimeOnly))
		if cause := a.causeOf(ctx, owner, cur.GetCreationTimestamp().Time); cause != "" {
			line += ", after " + cause
		}
		chain = append(chain, line)
		cur = owner
	}
	if len(chain) > 0 {
		b.WriteString("\nBecause:\n")
		for _, c := range chain {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	// Bouts over it or its owners.
	if list, err := dyn.Resource(boutsGVR).List(ctx, metav1.ListOptions{}); err == nil {
		for _, bt := range list.Items {
			o := boutObject(bt)
			if strings.Contains(o, " "+obj.GetNamespace()+"/") || o == wrestling.RefOf(obj).String() || strings.Contains(strings.Join(chain, " "), o) {
				path, _, _ := unstructured.NestedString(bt.Object, "spec", "path")
				fight, _, _ := unstructured.NestedString(bt.Object, "status", "summary")
				phase, _, _ := unstructured.NestedString(bt.Object, "status", "phase")
				fmt.Fprintf(&b, "\n♂ Bout (%s): %s of %s — %s\n", phase, path, o, fight)
			}
		}
	}
	return b.String(), nil
}

// causeOf tells which write of owner came just before at: what changed and
// who wrote it, from the tape. Empty when the tape cannot tell.
func (a *app) causeOf(ctx context.Context, owner *unstructured.Unstructured, at time.Time) string {
	var res struct {
		Objects []tape.Object `json:"objects"`
	}
	ref := wrestling.RefOf(owner)
	kind := ref.Kind
	if ref.Group != "" {
		kind += "." + ref.Group
	}
	q := url.Values{"kind": {kind}, "name": {ref.Name}, "namespace": {ref.Namespace}}
	if err := a.call(ctx, "/objects", q, &res); err != nil || len(res.Objects) == 0 {
		return ""
	}
	var history []tape.Object
	if err := a.call(ctx, "/history", url.Values{"key": {res.Objects[0].Key}}, &history); err != nil {
		return ""
	}
	return causeIn(history, at)
}

// causeIn finds, in an owner's history, its last write at or before at
// (a second of slack for clocks) that changed more than its status, and
// tells what it changed and who wrote it. Controllers write status right
// after a change; those writes are passed over.
func causeIn(history []tape.Object, at time.Time) string {
	cut := at.Add(time.Second).UnixMilli()
	last := -1
	for j, h := range history {
		if h.Time != 0 && h.Time <= cut && !h.Deleted {
			last = j
		}
	}
	for i := last; i > 0; i-- {
		if history[i-1].Object == nil || history[i].Object == nil {
			return ""
		}
		old := &unstructured.Unstructured{Object: history[i-1].Object}
		new := &unstructured.Unstructured{Object: history[i].Object}
		var paths []string
		for _, p := range wrestling.Changed(old.Object, new.Object) {
			if p != "status" && !strings.HasPrefix(p, "status.") && !strings.HasPrefix(p, "metadata.annotations.deployment.kubernetes.io") &&
				!(p == "metadata.annotations" && onlyRevision(new.GetAnnotations())) {
				paths = append(paths, p)
			}
		}
		if len(paths) == 0 {
			continue
		}
		if len(paths) > 3 {
			paths = append(paths[:3], fmt.Sprintf("and %d more", len(paths)-3))
		}
		writers := wrestling.Writers(old, new)
		by := ""
		if len(writers) > 0 {
			by = " by " + strings.Join(writers, ", ")
		}
		return fmt.Sprintf("%s changed%s at %s (revision %d)", strings.Join(paths, ", "), by,
			time.UnixMilli(history[i].Time).Format(time.TimeOnly), history[i].Revision)
	}
	return ""
}

func describeRef(u *unstructured.Unstructured) string {
	if u.GetNamespace() != "" {
		return u.GetKind() + " " + u.GetNamespace() + "/" + u.GetName()
	}
	return u.GetKind() + " " + u.GetName()
}

func headline(u *unstructured.Unstructured, problems []string) string {
	if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok {
		if len(problems) == 0 {
			if phase == "Pending" {
				return "is Pending, and nothing says why: is a scheduler running?"
			}
			return "is " + phase + ". Nothing to be sorry for."
		}
		return "is " + phase + "."
	}
	if len(problems) == 0 {
		return "looks fine. Nothing to be sorry for."
	}
	return "is not well."
}

// explain lists what is wrong with an object: its failing conditions,
// why its containers wait or died, and its recent warnings.
func explain(u *unstructured.Unstructured, events []corev1.Event) []string {
	var out []string
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		typ, _ := m["type"].(string)
		status, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		bad := false
		switch {
		case strings.HasSuffix(typ, "Failure") || strings.HasSuffix(typ, "Failed") || typ == "Stalled" || typ == "Degraded":
			bad = status == "True"
		case typ == "Ready" || typ == "Available" || typ == "PodScheduled" || typ == "Progressing" || typ == "Established":
			bad = status == "False"
		}
		if bad {
			out = append(out, strings.TrimSpace(fmt.Sprintf("%s=%s %s: %s", typ, status, reason, msg)))
		}
	}
	for _, field := range []string{"initContainerStatuses", "containerStatuses"} {
		cs, _, _ := unstructured.NestedSlice(u.Object, "status", field)
		for _, c := range cs {
			m, _ := c.(map[string]any)
			name, _ := m["name"].(string)
			if w, ok, _ := unstructured.NestedMap(m, "state", "waiting"); ok {
				out = append(out, fmt.Sprintf("container %s waits: %v %v", name, w["reason"], w["message"]))
			}
			if t, ok, _ := unstructured.NestedMap(m, "lastState", "terminated"); ok {
				out = append(out, fmt.Sprintf("container %s last died: %v, exit code %v", name, t["reason"], t["exitCode"]))
			}
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].LastTimestamp.After(events[j].LastTimestamp.Time) })
	seen := map[string]bool{}
	for _, e := range events {
		if e.Type != corev1.EventTypeWarning || seen[e.Reason+e.Message] || len(seen) == 5 {
			continue
		}
		seen[e.Reason+e.Message] = true
		out = append(out, fmt.Sprintf("%s: %s (×%d)", e.Reason, strings.TrimSpace(e.Message), max(e.Count, 1)))
	}
	return out
}

// onlyRevision: the annotations are only the ones the deployment controller
// keeps, a write of its own.
func onlyRevision(a map[string]string) bool {
	for k := range a {
		if !strings.HasPrefix(k, "deployment.kubernetes.io/") {
			return false
		}
	}
	return true
}
