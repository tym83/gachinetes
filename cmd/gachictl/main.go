// gachictl is the Gachinetes command line: the cluster's tape (rewind,
// history, graveyard, undelete) and, as it grows, the rest of the gym.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/tym83/gachinetes/pkg/tape"
)

type app struct {
	flags   *genericclioptions.ConfigFlags
	tapeURL string
	output  string
}

func main() {
	a := &app{flags: genericclioptions.NewConfigFlags(true)}
	root := &cobra.Command{
		Use:           "gachictl",
		Short:         "♂ Gachinetes ♂: rewind, undelete and the rest of the gym",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	a.flags.AddFlags(root.PersistentFlags())
	root.PersistentFlags().StringVar(&a.tapeURL, "tape-url", "", "the tape service directly, instead of through the API server")
	root.PersistentFlags().StringVarP(&a.output, "output", "o", "", "yaml or json; a table when empty")
	root.AddCommand(a.getCmd(), a.rewindCmd(), a.historyCmd(), a.graveyardCmd(), a.undeleteCmd(), a.infoCmd(), a.vhsCmd(), a.boutsCmd(), a.beltsCmd(), a.whyCmd(), a.billCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Oh shit, I'm sorry:", err)
		os.Exit(1)
	}
}

// call asks the tape: an aggregated API of the cluster, or, with
// --tape-url, a tape service directly.
func (a *app) call(ctx context.Context, path string, q url.Values, out any) error {
	var raw []byte
	var err error
	if a.tapeURL != "" {
		raw, err = httpGet(ctx, a.tapeURL+tape.Prefix+path+"?"+q.Encode())
	} else {
		raw, err = a.callAPI(ctx, path, q)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (a *app) callAPI(ctx context.Context, path string, q url.Values) ([]byte, error) {
	cfg, err := a.flags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	req := cs.CoreV1().RESTClient().Get().AbsPath(tape.Prefix + path)
	for k := range q {
		req = req.Param(k, q.Get(k))
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("the tape (%s): %w", tape.Group, err)
	}
	return raw, nil
}

// target is what a command reads: a kind (resolved from what the user
// typed, as kubectl resolves it), a name and a namespace.
type target struct {
	kind, name, namespace string
}

func (a *app) target(args []string, allNamespaces bool) (target, error) {
	var t target
	if !allNamespaces {
		ns, _, err := a.flags.ToRawKubeConfigLoader().Namespace()
		if err != nil {
			return t, err
		}
		t.namespace = ns
	}
	if len(args) == 0 {
		return t, nil
	}
	res, name, _ := strings.Cut(args[0], "/")
	if name == "" && len(args) > 1 {
		name = args[1]
	}
	t.name = name
	kind, namespaced, err := a.kindOf(res)
	if err != nil {
		return t, err
	}
	t.kind = kind
	switch {
	case kind == "Namespace":
		t.namespace = name
	case !namespaced:
		t.namespace = ""
	}
	return t, nil
}

// kindOf resolves what the user typed to a kind, and whether objects of
// it live in a namespace.
func (a *app) kindOf(res string) (string, bool, error) {
	if a.tapeURL != "" {
		// No API server to ask: take a kind as typed.
		return res, true, nil
	}
	m, err := a.flags.ToRESTMapper()
	if err != nil {
		return "", false, err
	}
	gvr, _ := schema.ParseResourceArg(res)
	if gvr == nil {
		gr := schema.ParseGroupResource(res)
		gvr = &schema.GroupVersionResource{Group: gr.Group, Resource: gr.Resource}
	}
	gvk, err := m.KindFor(*gvr)
	if err != nil {
		return "", false, err
	}
	mapping, err := m.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return "", false, err
	}
	namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	if gvk.Group != "" {
		return gvk.Kind + "." + gvk.Group, namespaced, nil
	}
	return gvk.Kind, namespaced, nil
}

func (t target) query(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if t.kind != "" {
		q.Set("kind", t.kind)
	}
	if t.name != "" && t.kind != "Namespace" {
		q.Set("name", t.name)
	}
	if t.namespace != "" {
		q.Set("namespace", t.namespace)
	}
	return q
}

func (a *app) getCmd() *cobra.Command {
	var at string
	var all bool
	c := &cobra.Command{
		Use:   "get KIND[/NAME] --at TIME|REVISION",
		Short: "Objects as they were at a moment",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, all)
			if err != nil {
				return err
			}
			p, err := parseWhen(at)
			if err != nil {
				return err
			}
			var res struct {
				Revision int64         `json:"revision"`
				Objects  []tape.Object `json:"objects"`
			}
			if err := a.call(cmd.Context(), "/objects", t.query(url.Values{"at": {p}}), &res); err != nil {
				return err
			}
			if a.output != "" {
				return a.print(objectsOf(res.Objects))
			}
			fmt.Printf("PLAY ▶ revision %d\n", res.Revision)
			return table(res.Objects, "WRITTEN", func(o tape.Object) int64 { return o.Time })
		},
	}
	c.Flags().StringVar(&at, "at", "", "a time (RFC 3339, or a duration ago such as 15m) or a revision")
	c.Flags().BoolVarP(&all, "all-namespaces", "A", false, "every namespace")
	_ = c.MarkFlagRequired("at")
	return c
}

func (a *app) rewindCmd() *cobra.Command {
	var to string
	var all bool
	c := &cobra.Command{
		Use:   "rewind [KIND[/NAME]] --to TIME|REVISION",
		Short: "◀◀ What changed between a moment and now",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, all)
			if err != nil {
				return err
			}
			p, err := parseWhen(to)
			if err != nil {
				return err
			}
			var d tape.Diff
			if err := a.call(cmd.Context(), "/diff", t.query(url.Values{"from": {p}}), &d); err != nil {
				return err
			}
			if a.output != "" {
				return a.print(d)
			}
			fmt.Printf("◀◀ REW  revision %d → %d\n", d.FromRev, d.ToRev)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "CHANGE\tKIND\tNAMESPACE\tNAME")
			for _, set := range []struct {
				mark string
				cs   []tape.Change
			}{{"+ added", d.Added}, {"- removed", d.Removed}, {"~ changed", d.Changed}} {
				for _, ch := range set.cs {
					o := ch.To
					if o == nil {
						o = ch.From
					}
					kind, ns, name := describe(*o)
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", set.mark, kind, ns, name)
				}
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&to, "to", "", "a time (RFC 3339, or a duration ago such as 15m) or a revision")
	c.Flags().BoolVarP(&all, "all-namespaces", "A", false, "every namespace")
	_ = c.MarkFlagRequired("to")
	return c
}

func (a *app) historyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history KIND/NAME",
		Short: "Every version of an object the tape holds",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, false)
			if err != nil {
				return err
			}
			// The key is found through the objects of the kind, now or in
			// the graveyard.
			key, err := a.keyOf(cmd.Context(), t)
			if err != nil {
				return err
			}
			var out []tape.Object
			if err := a.call(cmd.Context(), "/history", url.Values{"key": {key}}, &out); err != nil {
				return err
			}
			if a.output != "" {
				return a.print(objectsOf(out))
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "REVISION\tWRITTEN\tWHAT")
			for _, o := range out {
				what := "written"
				switch {
				case o.Deleted:
					what = "deleted — rest in peace"
				case o.Object != nil && (&unstructured.Unstructured{Object: o.Object}).GetDeletionTimestamp() != nil:
					what = "being deleted"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\n", o.Revision, when(o.Time), what)
			}
			return w.Flush()
		},
	}
}

// keyOf finds the store key of an object that exists or lies in the
// graveyard.
func (a *app) keyOf(ctx context.Context, t target) (string, error) {
	var res struct {
		Objects []tape.Object `json:"objects"`
	}
	if err := a.call(ctx, "/objects", t.query(nil), &res); err != nil {
		return "", err
	}
	if len(res.Objects) == 0 {
		var graves []tape.Object
		if err := a.call(ctx, "/graveyard", t.query(nil), &graves); err != nil {
			return "", err
		}
		res.Objects = graves
	}
	for _, o := range res.Objects {
		if t.kind != "Namespace" || (&unstructured.Unstructured{Object: o.Object}).GetKind() == "Namespace" {
			return o.Key, nil
		}
	}
	return "", fmt.Errorf("%s %s: not on the tape", t.kind, t.name)
}

func (a *app) graveyardCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "graveyard [KIND[/NAME]]",
		Short: "Deleted objects the tape can bring back",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, all)
			if err != nil {
				return err
			}
			var graves []tape.Object
			if err := a.call(cmd.Context(), "/graveyard", t.query(nil), &graves); err != nil {
				return err
			}
			if a.output != "" {
				return a.print(objectsOf(graves))
			}
			return table(graves, "DELETED", func(o tape.Object) int64 { return o.DeletedTime })
		},
	}
	c.Flags().BoolVarP(&all, "all-namespaces", "A", false, "every namespace")
	return c
}

func (a *app) infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tape",
		Short: "What the tape holds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var i tape.Info
			if err := a.call(cmd.Context(), "/info", url.Values{}, &i); err != nil {
				return err
			}
			if a.output != "" {
				return a.print(i)
			}
			fmt.Printf("TAPE  revisions %d … %d  (%s … %s)\n", i.From, i.Current, when(i.FromTime), when(i.CurrentTime))
			if !i.Started {
				fmt.Println("Not recording past compaction yet: is the store booted with kuberoot.store-tape?")
			}
			return nil
		},
	}
}

func (a *app) print(v any) error {
	switch a.output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		raw, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(raw)
		return err
	}
	return fmt.Errorf("-o %s: yaml or json", a.output)
}

func objectsOf(os []tape.Object) []map[string]any {
	out := []map[string]any{}
	for _, o := range os {
		if o.Object != nil {
			out = append(out, o.Object)
		}
	}
	return out
}

func describe(o tape.Object) (kind, namespace, name string) {
	if o.Object == nil {
		return o.Resource, "", o.Key
	}
	u := unstructured.Unstructured{Object: o.Object}
	return u.GetKind(), u.GetNamespace(), u.GetName()
}

func table(objs []tape.Object, column string, at func(tape.Object) int64) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "KIND\tNAMESPACE\tNAME\t%s\n", column)
	for _, o := range objs {
		kind, ns, name := describe(o)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", kind, ns, name, when(at(o)))
	}
	return w.Flush()
}

func when(ms int64) string {
	if ms == 0 {
		return "?"
	}
	t := time.UnixMilli(ms)
	return fmt.Sprintf("%s (%s ago)", t.Format(time.DateTime), time.Since(t).Round(time.Second))
}

// parseWhen takes a revision, an RFC 3339 time or a duration ago, and
// gives the tape service's point.
func parseWhen(v string) (string, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d).UTC().Format(time.RFC3339), nil
	}
	if _, err := tape.ParsePoint(v); err != nil {
		return "", fmt.Errorf("%q: a revision, an RFC 3339 time or a duration ago", v)
	}
	return v, nil
}
