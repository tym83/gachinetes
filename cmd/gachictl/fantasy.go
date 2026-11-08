package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/fantasy"
)

// fantasyCmd runs fantasies as preview environments of pull requests.
func (a *app) fantasyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fantasy",
		Short: "Deep dark fantasies as preview environments of pull requests",
	}
	var ns string
	c.PersistentFlags().StringVar(&ns, "preview-namespace", fantasy.PreviewNamespace, "where previews run")
	clients := func() (kubernetes.Interface, dynamic.Interface, error) {
		cfg, err := a.flags.ToRESTConfig()
		if err != nil {
			return nil, nil, err
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, nil, err
		}
		dyn, err := dynamic.NewForConfig(cfg)
		return cs, dyn, err
	}
	nameOf := func(pr string) (fantasy.PullRequest, string, error) {
		p, err := fantasy.ParsePullRequest(pr)
		return p, p.PreviewName(), err
	}

	var pr, dir, image, at, kubeconfigOut, reportOut string
	var ttl, quiet, timeout, wait time.Duration
	preview := &cobra.Command{
		Use:   "preview --pr URL|N --manifests DIR",
		Short: "Play a pull request's manifests out on a branch of the cluster and keep it up",
		Long: `Starts a fantasy for the pull request, in place of the one it had: a branch
of the tape with its own API server, controllers, scheduler and kwok nodes,
with the request's manifests applied. Prints the report and where the
branch's API server is, and writes its kubeconfig. The preview goes away
after --ttl, or with gachictl fantasy discard.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, name, err := nameOf(pr)
			if err != nil {
				return err
			}
			manifests, keys, err := fantasy.Manifests(dir)
			if err != nil {
				return err
			}
			cs, _, err := clients()
			if err != nil {
				return err
			}
			pv := fantasy.Preview{Name: name, Namespace: ns, PR: p, Image: image, TTL: ttl, At: at, Quiet: quiet, Timeout: timeout}
			if err := fantasy.Launch(cmd.Context(), cs, pv, manifests, keys); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "♂ fantasy %s for %s: %d manifests, waiting for it to play out\n", name, p, len(keys))
			st, err := fantasy.Wait(cmd.Context(), cs, ns, name, wait, 5*time.Second)
			if reportOut != "" {
				if werr := os.WriteFile(reportOut, []byte(previewMarkdown(st, ns)), 0o600); werr != nil && err == nil {
					err = werr
				}
			}
			if err != nil {
				return err
			}
			fmt.Print(st.ReportText)
			fmt.Printf("\nAPI server: %s (in the cluster)\n", st.Endpoint)
			fmt.Printf("From here: kubectl -n %s port-forward svc/%s %d:6443\n", ns, name, fantasy.ForwardPort)
			if kubeconfigOut != "" {
				if err := os.WriteFile(kubeconfigOut, st.ForwardKubeconfig, 0o600); err != nil {
					return err
				}
				fmt.Printf("Kubeconfig (through the port-forward): %s\n", kubeconfigOut)
			}
			fmt.Printf("Expires %s. Merge with: gachictl fantasy merge --pr %s\n", st.Expires.Format(time.RFC3339), pr)
			return nil
		},
	}
	preview.Flags().StringVar(&pr, "pr", "", "the pull request: N, owner/repo#N or its URL")
	preview.Flags().StringVar(&dir, "manifests", "", "the directory of the request's manifests (*.yaml, *.yml, *.json)")
	preview.Flags().StringVar(&image, "image", fantasy.PreviewImage, "the preview's image (Dockerfile.preview)")
	preview.Flags().StringVar(&at, "at", "", "branch from a revision or an RFC 3339 time (default: now)")
	preview.Flags().DurationVar(&ttl, "ttl", 72*time.Hour, "how long the preview stays up")
	preview.Flags().DurationVar(&quiet, "until-quiet", 20*time.Second, "the change has played out once nothing changes this long")
	preview.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "and in any case after this long")
	preview.Flags().DurationVar(&wait, "wait", 20*time.Minute, "how long to wait for the report")
	preview.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "write the branch's kubeconfig here (it reaches the API server through kubectl port-forward)")
	preview.Flags().StringVar(&reportOut, "report-out", "", "write the report as Markdown here, for a comment on the pull request")
	_ = preview.MarkFlagRequired("pr")
	_ = preview.MarkFlagRequired("manifests")

	discard := &cobra.Command{
		Use:   "discard --pr URL|N",
		Short: "Discard a pull request's preview",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, name, err := nameOf(pr)
			if err != nil {
				return err
			}
			cs, _, err := clients()
			if err != nil {
				return err
			}
			if err := fantasy.Discard(cmd.Context(), cs, ns, name); err != nil {
				return err
			}
			fmt.Printf("♂ fantasy %s discarded\n", name)
			return nil
		},
	}
	discard.Flags().StringVar(&pr, "pr", "", "the pull request: N, owner/repo#N or its URL")
	_ = discard.MarkFlagRequired("pr")

	var keep bool
	merge := &cobra.Command{
		Use:   "merge --pr URL|N",
		Short: "Apply a preview's manifests to the cluster, unless it moved on since the branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, name, err := nameOf(pr)
			if err != nil {
				return err
			}
			cs, dyn, err := clients()
			if err != nil {
				return err
			}
			st, err := fantasy.Status(cmd.Context(), cs, ns, name)
			if err != nil {
				return err
			}
			if st.Report == nil {
				return fmt.Errorf("fantasy %s has no report (%s): nothing to merge", name, st.Status)
			}
			manifests, keys, err := fantasy.ManifestsOf(cmd.Context(), cs, ns, name)
			if err != nil {
				return err
			}
			objs, err := fantasy.ObjectsFrom(manifests, keys)
			if err != nil {
				return err
			}
			done, err := fantasy.Merge(cmd.Context(), cs, dyn, objs, st.Report.Branch)
			for _, d := range done {
				fmt.Printf("♂ merged: %s\n", d)
			}
			if err != nil {
				return err
			}
			if !keep {
				return fantasy.Discard(cmd.Context(), cs, ns, name)
			}
			return nil
		},
	}
	merge.Flags().StringVar(&pr, "pr", "", "the pull request: N, owner/repo#N or its URL")
	merge.Flags().BoolVar(&keep, "keep", false, "keep the preview after merging")
	_ = merge.MarkFlagRequired("pr")

	list := &cobra.Command{
		Use:   "list",
		Short: "The previews and when they expire",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cs, _, err := clients()
			if err != nil {
				return err
			}
			all, err := fantasy.List(cmd.Context(), cs, ns)
			if err != nil {
				return err
			}
			if a.output != "" {
				return a.print(all)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPULL REQUEST\tSTATUS\tBRANCH\tEXPIRES")
			for _, p := range all {
				branch := "-"
				if p.Report != nil {
					branch = strconv.FormatInt(p.Report.Branch, 10)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.PullRequest, p.Status, branch, time.Until(p.Expires).Round(time.Minute))
			}
			return w.Flush()
		},
	}
	c.AddCommand(preview, discard, merge, list)
	return c
}

// previewMarkdown is the preview as a comment on its pull request.
func previewMarkdown(st fantasy.PreviewStatus, ns string) string {
	if st.Status != fantasy.StatusReady {
		return fmt.Sprintf("### ♂ Deep dark fantasy `%s`: %s\n\n```\n%s\n```\n", st.Name, st.Status, st.Error)
	}
	return fmt.Sprintf("### ♂ Deep dark fantasy `%s`\n\n```\n%s```\n\nAPI server in the cluster: `%s`, or `kubectl -n %s port-forward svc/%s %d:6443`. Expires %s.\n",
		st.Name, st.ReportText, st.Endpoint, ns, st.Name, fantasy.ForwardPort, st.Expires.UTC().Format(time.RFC3339))
}
