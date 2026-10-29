// gachi-referee watches the gym: it calls bouts when field managers keep
// taking a field from each other (Bout objects and events on the object),
// and holds belts, the admission webhook that refuses or warns of writes
// of a held field by anyone else (BeltPolicy objects).
package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tym83/gachinetes/pkg/belts"
	"github.com/tym83/gachinetes/pkg/wrestling"
)

var (
	boutsGVR    = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "bouts"}
	policiesGVR = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "beltpolicies"}
	budgetsGVR  = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "budgets"}
	pricesGVR   = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "pricelists"}
	shakesGVR   = schema.GroupVersionResource{Group: belts.Group, Version: belts.Version, Resource: "proteinshakes"}
)

// unwatched are resources the referee leaves alone: written by design by
// many hands or too often to mean anything (events, leases, endpoints),
// Secrets, which it has no business holding in memory, and its own bouts.
var unwatched = map[string]bool{
	"events": true, "events.events.k8s.io": true, "leases.coordination.k8s.io": true,
	"endpoints": true, "endpointslices.discovery.k8s.io": true, "secrets": true,
	"bouts." + belts.Group: true,
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "the cluster, when not run in it")
	listen := flag.String("listen", ":9443", "where the belts webhook is served, HTTPS")
	webhookURL := flag.String("webhook-url", "", "how the API server reaches the webhook; empty: the gachi-referee Service in --namespace")
	namespace := flag.String("namespace", "gachinetes", "the namespace of the referee's Service")
	window := flag.Duration("window", 5*time.Minute, "how long the referee remembers a round")
	switches := flag.Int("switches", 4, "how many changes of hands within the window make a bout")
	skip := flag.String("skip", "status.conditions,metadata.annotations.deployment.kubernetes.io/revision", "paths not fought over, comma separated")
	flag.Parse()

	cfg, err := rest.InClusterConfig()
	if *kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	cs := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ref := &wrestling.Referee{Window: *window, Switches: *switches, Skip: strings.Split(*skip, ",")}

	cert, caPEM, err := selfSigned(*namespace)
	if err != nil {
		log.Fatal(err)
	}
	g := &guard{cs: cs, dyn: dyn, namespace: *namespace}
	h := &holder{cs: cs, dyn: dyn, caBundle: caPEM, url: *webhookURL, namespace: *namespace, guard: g}
	go serveWebhook(*listen, cert, h)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	watchAll(cs.Discovery(), factory, ref)
	reload := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { h.reload(ctx) },
		UpdateFunc: func(any, any) { h.reload(ctx) },
		DeleteFunc: func(any) { h.reload(ctx) },
	}
	for _, gvr := range []schema.GroupVersionResource{policiesGVR, budgetsGVR, pricesGVR} {
		_, _ = factory.ForResource(gvr).Informer().AddEventHandler(reload)
	}
	h.policyStore = factory.ForResource(policiesGVR).Informer().GetStore()
	h.budgetStore = factory.ForResource(budgetsGVR).Informer().GetStore()
	h.priceStore = factory.ForResource(pricesGVR).Informer().GetStore()
	h.pods = factory.ForResource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Informer().GetIndexer()
	k := &kitchen{cs: cs, namespace: *namespace, shakes: factory.ForResource(shakesGVR).Informer().GetStore()}
	_, _ = factory.ForResource(shakesGVR).Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { k.reconcile(ctx) },
		UpdateFunc: func(any, any) { k.reconcile(ctx) },
		DeleteFunc: func(any) { k.reconcile(ctx) },
	})
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	h.reload(ctx)
	k.reconcile(ctx)
	log.Printf("♂ gachi-referee: watching, window %s, %d switches make a bout", *window, *switches)

	go func() {
		for range time.Tick(3 * time.Second) {
			g.reconcile(ctx)
		}
	}()
	announcer := &announcer{cs: cs, dyn: dyn}
	for range time.Tick(10 * time.Second) {
		announcer.sync(ctx, ref.Bouts(time.Now()))
	}
}

// watchAll watches every resource that can be listed, watched and
// updated, and hands every update to the referee.
func watchAll(d discovery.DiscoveryInterface, f dynamicinformer.DynamicSharedInformerFactory, ref *wrestling.Referee) {
	lists, err := d.ServerPreferredResources()
	if err != nil && len(lists) == 0 {
		log.Fatal(err)
	}
	n := 0
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			name := r.Name
			if gv.Group != "" {
				name += "." + gv.Group
			}
			if strings.Contains(r.Name, "/") || unwatched[name] || !has(r.Verbs, "list", "watch", "update") {
				continue
			}
			inf := f.ForResource(gv.WithResource(r.Name)).Informer()
			_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
				UpdateFunc: func(o, n any) {
					old, ok1 := o.(*unstructured.Unstructured)
					new, ok2 := n.(*unstructured.Unstructured)
					if ok1 && ok2 && old.GetResourceVersion() != new.GetResourceVersion() {
						ref.Observe(old, new, time.Now())
					}
				},
			})
			n++
		}
	}
	log.Printf("watching %d resources", n)
}

func has(verbs []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, v := range verbs {
			found = found || v == w
		}
		if !found {
			return false
		}
	}
	return true
}
