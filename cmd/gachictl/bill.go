package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/bucks"
)

var (
	pricesGVR  = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "pricelists"}
	budgetsGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "budgets"}
)

func (a *app) billCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bill",
		Short: "Three hundred bucks: what each namespace's pods cost a month, against its budget",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dyn, cs, _, err := a.clients()
			if err != nil {
				return err
			}
			lists, err := dyn.Resource(pricesGVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			if len(lists.Items) == 0 {
				return fmt.Errorf("no PriceList: the gym charges nothing yet")
			}
			pl := lists.Items[0]
			for i := range lists.Items {
				if lists.Items[i].GetName() == "gym" {
					pl = lists.Items[i]
				}
			}
			prices, err := bucks.PricesFrom(&pl)
			if err != nil {
				return err
			}
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			byNs := map[string][]*corev1.Pod{}
			for i := range pods.Items {
				p := &pods.Items[i]
				byNs[p.Namespace] = append(byNs[p.Namespace], p)
			}
			budgets := map[string]float64{}
			if bl, err := dyn.Resource(budgetsGVR).Namespace("").List(ctx, metav1.ListOptions{}); err == nil {
				for i := range bl.Items {
					if b, err := bucks.BudgetFrom(&bl.Items[i]); err == nil {
						budgets[bl.Items[i].GetNamespace()] = b.Monthly
					}
				}
			}
			var names []string
			for ns := range byNs {
				names = append(names, ns)
			}
			sort.Strings(names)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAMESPACE\tPODS\tA MONTH\tBUDGET\t")
			var total float64
			for _, ns := range names {
				m := prices.Monthly(byNs[ns])
				total += m
				budget, mark := "—", ""
				if b, ok := budgets[ns]; ok {
					budget = fmt.Sprintf("%s%.2f", prices.Currency, b)
					if m > b {
						mark = "♂ over"
					}
				}
				fmt.Fprintf(w, "%s\t%d\t%s%.2f\t%s\t%s\n", ns, len(byNs[ns]), prices.Currency, m, budget, mark)
			}
			fmt.Fprintf(w, "TOTAL\t\t%s%.2f\t\t\n", prices.Currency, total)
			return w.Flush()
		},
	}
}
