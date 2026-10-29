package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/contracts"
)

var (
	contractsGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "contracts"}
	spotsGVR     = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "spots"}
)

// whoami is the user the cluster takes the caller for.
func (a *app) whoami(ctx context.Context) (string, error) {
	_, cs, _, err := a.clients()
	if err != nil {
		return "", err
	}
	r, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return r.Status.UserInfo.Username, nil
}

func (a *app) contractCmd() *cobra.Command {
	var role, ns, dur, reason string
	var spotter bool
	c := &cobra.Command{
		Use:   "contract NAME --role ROLE --for 1h [--in NS] [--spotter]",
		Short: "Elevated access for a while: a cluster role bound to you, taken back on time or by the safeword",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			me, err := a.whoami(ctx)
			if err != nil {
				return err
			}
			dyn, _, _, err := a.clients()
			if err != nil {
				return err
			}
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": belts.Group + "/" + belts.Version, "kind": "Contract",
				"metadata": map[string]any{"name": args[0], "annotations": map[string]any{contracts.RequestedBy: me}},
				"spec":     map[string]any{"user": me, "clusterRole": role, "namespace": ns, "duration": dur, "reason": reason, "spotter": spotter},
			}}
			if _, err := dyn.Resource(contractsGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
				return err
			}
			if spotter {
				fmt.Printf("♂ contract %s asked: someone who may grant %s says gachictl spot contract/%s\n", args[0], role, args[0])
			} else {
				fmt.Printf("♂ contract %s: %s for %s\n", args[0], role, dur)
			}
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "", "the cluster role")
	c.Flags().StringVar(&ns, "in", "", "bind it in this namespace only")
	c.Flags().StringVar(&dur, "for", "1h", "how long, up to 24h")
	c.Flags().StringVar(&reason, "reason", "", "why, for the record")
	c.Flags().BoolVar(&spotter, "spotter", false, "a second person approves it")
	_ = c.MarkFlagRequired("role")
	return c
}

func (a *app) spotCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "spot contract/NAME | spot/NAME | spot delete RESOURCE/NAME",
		Short: "The spotter: approve someone's contract or deletion, or ask for a deletion of what is spotted",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			me, err := a.whoami(ctx)
			if err != nil {
				return err
			}
			dyn, _, _, err := a.clients()
			if err != nil {
				return err
			}
			if args[0] == "delete" && len(args) == 2 {
				res, name, ok := strings.Cut(args[1], "/")
				if !ok {
					return fmt.Errorf("delete RESOURCE/NAME, such as namespaces/prod")
				}
				group := ""
				if r, g, found := strings.Cut(res, "."); found {
					res, group = r, g
				}
				u := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": belts.Group + "/" + belts.Version, "kind": "Spot",
					"metadata": map[string]any{"generateName": "delete-" + name + "-", "annotations": map[string]any{contracts.RequestedBy: me}},
					"spec":     map[string]any{"group": group, "resource": res, "name": name},
				}}
				out, err := dyn.Resource(spotsGVR).Create(ctx, u, metav1.CreateOptions{})
				if err != nil {
					return err
				}
				fmt.Printf("♂ asked: someone else says gachictl spot spot/%s; then delete within 15 minutes\n", out.GetName())
				return nil
			}
			kind, name, _ := strings.Cut(args[0], "/")
			gvr := contractsGVR
			if kind == "spot" {
				gvr = spotsGVR
			}
			patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, contracts.ApprovedBy, me)
			if _, err := dyn.Resource(gvr).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
				return err
			}
			fmt.Printf("♂ spotted by %s\n", me)
			return nil
		},
	}
}

func (a *app) safewordCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "safeword",
		Short: "Stop. Every contract ends at once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, cs, _, err := a.clients()
			if err != nil {
				return err
			}
			api := cs.CoreV1().ConfigMaps("gachinetes")
			for i := 0; i < 5; i++ {
				cm, err := api.Get(ctx, "trust-epoch", metav1.GetOptions{})
				if err != nil {
					return err
				}
				n, _ := strconv.ParseInt(cm.Data["epoch"], 10, 64)
				cm.Data["epoch"] = strconv.FormatInt(n+1, 10)
				cm.Data["said"] = time.Now().UTC().Format(time.RFC3339)
				if _, err = api.Update(ctx, cm, metav1.UpdateOptions{}); err == nil {
					fmt.Printf("♂ SAFEWORD. Trust epoch %d: every contract ends now.\n", n+1)
					return nil
				}
			}
			return fmt.Errorf("the trust epoch would not move")
		},
	}
}

func (a *app) dungeonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dungeon pod/NAME",
		Short: "Quarantine instead of kill: out of its controller and its Services, kept for a look",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			t, err := a.target(args, false)
			if err != nil {
				return err
			}
			_, cs, _, err := a.clients()
			if err != nil {
				return err
			}
			pods := cs.CoreV1().Pods(t.namespace)
			p, err := pods.Get(ctx, t.name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			// Its labels move to an annotation: Services and its controller
			// let it go, and the controller makes a replacement.
			old := []string{}
			for k, v := range p.Labels {
				old = append(old, k+"="+v)
			}
			p.Labels = map[string]string{"gym.gachinetes.dev/dungeon": "true"}
			if p.Annotations == nil {
				p.Annotations = map[string]string{}
			}
			p.Annotations["gym.gachinetes.dev/dungeon-labels"] = strings.Join(old, ",")
			p.OwnerReferences = nil
			if _, err := pods.Update(ctx, p, metav1.UpdateOptions{}); err != nil {
				return err
			}
			// And no traffic in or out, where the network enforces policies.
			np := dungeonPolicy(t.namespace)
			if _, err := cs.NetworkingV1().NetworkPolicies(t.namespace).Create(ctx, np, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
				return err
			}
			fmt.Printf("♂ %s/%s is in the dungeon: its controller makes a new one; delete it when you are done looking\n", t.namespace, t.name)
			return nil
		},
	}
}
