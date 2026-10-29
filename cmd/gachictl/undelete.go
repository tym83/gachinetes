package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/tym83/gachinetes/pkg/tape"
)

func (a *app) undeleteCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "undelete KIND/NAME",
		Short: "Rest in peace, but come back: restore what was deleted",
		Long: `Restores a deleted object from the tape, with your own rights.
A namespace comes back with everything deleted with it; what the cluster
makes again by itself (pods of a restored ReplicaSet, events, tokens) is
left to the cluster.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.target(args, false)
			if err != nil {
				return err
			}
			graves, err := a.gravesFor(cmd.Context(), t)
			if err != nil {
				return err
			}
			if len(graves) == 0 {
				return fmt.Errorf("%s %s is not in the graveyard", t.kind, t.name)
			}
			steps, skipped := tape.Plan(graves)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "RESTORE\tKIND\tNAMESPACE\tNAME")
			for _, s := range steps {
				fmt.Fprintf(w, "+\t%s\t%s\t%s\n", s.Object.GetKind(), s.Object.GetNamespace(), s.Object.GetName())
			}
			for _, s := range skipped {
				fmt.Fprintf(w, "·\t%s\t\t(%s)\n", s.Key, s.Why)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if dryRun {
				return nil
			}
			return a.restore(cmd.Context(), steps)
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would come back, change nothing")
	return c
}

// gravesFor is what a restore of t brings back: the object, or for a
// namespace everything deleted from the moment it started being deleted.
func (a *app) gravesFor(ctx context.Context, t target) ([]tape.Object, error) {
	q := t.query(url.Values{"reveal": {"1"}})
	if t.kind == "Namespace" {
		q.Del("kind")
	}
	var graves []tape.Object
	if err := a.call(ctx, "/v1/graveyard", q, &graves); err != nil {
		return nil, err
	}
	if t.kind != "Namespace" {
		return graves, nil
	}
	var ns *tape.Object
	for i := range graves {
		if u := (unstructured.Unstructured{Object: graves[i].Object}); u.GetKind() == "Namespace" {
			ns = &graves[i]
		}
	}
	if ns == nil {
		return nil, fmt.Errorf("namespace %s is not in the graveyard (still terminating?)", t.name)
	}
	var history []tape.Object
	if err := a.call(ctx, "/v1/history", url.Values{"key": {ns.Key}}, &history); err != nil {
		return nil, err
	}
	// The deletion started at the first version with a deletion time
	// since the namespace was last created.
	start := ns.DeletedAt
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Deleted && h.Revision != ns.DeletedAt {
			break
		}
		if h.Object != nil && (&unstructured.Unstructured{Object: h.Object}).GetDeletionTimestamp() != nil {
			start = h.Revision
		}
	}
	var out []tape.Object
	for _, g := range graves {
		if g.DeletedAt >= start {
			out = append(out, g)
		}
	}
	return out, nil
}

func (a *app) restore(ctx context.Context, steps []tape.Step) error {
	cfg, err := a.flags.ToRESTConfig()
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	mapper, err := a.flags.ToRESTMapper()
	if err != nil {
		return err
	}
	newUIDs := map[types.UID]types.UID{}
	revived, failed := 0, 0
	for _, s := range steps {
		u := s.Object
		tape.Rewire(u, newUIDs)
		gvk := u.GroupVersionKind()
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			fmt.Printf("✗ %s %s: %v\n", gvk.Kind, u.GetName(), err)
			failed++
			continue
		}
		var ri dynamic.ResourceInterface = dyn.Resource(m.Resource)
		if m.Scope.Name() == meta.RESTScopeNameNamespace {
			ri = dyn.Resource(m.Resource).Namespace(u.GetNamespace())
		}
		created, err := ri.Create(ctx, u, metav1.CreateOptions{FieldManager: "gachictl-undelete"})
		switch {
		case apierrors.IsAlreadyExists(err):
			fmt.Printf("= %s %s: already there\n", gvk.Kind, u.GetName())
			if live, gerr := ri.Get(ctx, u.GetName(), metav1.GetOptions{}); gerr == nil {
				newUIDs[s.OldUID] = live.GetUID()
			}
		case err != nil:
			fmt.Printf("✗ %s %s: %v\n", gvk.Kind, u.GetName(), err)
			failed++
		default:
			newUIDs[s.OldUID] = created.GetUID()
			revived++
		}
	}
	fmt.Printf("♂ Rest in peace? Not today: %d back", revived)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d objects did not come back", failed)
	}
	return nil
}

func httpGet(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, raw)
	}
	return raw, nil
}
