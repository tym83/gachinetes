// gachi-fantasy plays a change out on a copy of the cluster before it
// happens: deep dark fantasies. It seeds a store of its own with what the
// cluster's tape held at a point, starts an API server, the controllers
// and the scheduler on it, with kwok playing the nodes, applies the change
// and lets the copy run until it is quiet. The report says what would be
// made, changed and removed, where pods would land and which would go.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tym83/gachinetes/pkg/fantasy"
	"github.com/tym83/gachinetes/pkg/tape"
)

type flags struct {
	kubeconfig, at, work, bin, report string
	changes                           []string
	quiet, timeout                    time.Duration
	keep, merge                       bool
	base                              int
}

func main() {
	var f flags
	var changes string
	flag.StringVar(&f.kubeconfig, "kubeconfig", "", "the real cluster, whose tape seeds the fantasy; empty: in the cluster")
	flag.StringVar(&f.at, "at", "", "the point the fantasy branches from: a revision or an RFC 3339 time; empty: now")
	flag.StringVar(&changes, "f", "", "the change: manifests to apply, comma separated files")
	flag.StringVar(&f.work, "work", "/tmp/fantasy", "where the fantasy keeps its store, certificates and logs")
	flag.StringVar(&f.bin, "bin", "/usr/local/bin", "where kuberoot-store, kube-apiserver, kube-controller-manager, kube-scheduler, kubectl and kwok are")
	flag.StringVar(&f.report, "report", "", "write the report as JSON here too")
	flag.DurationVar(&f.quiet, "until-quiet", 20*time.Second, "the fantasy is over once nothing changes this long")
	flag.DurationVar(&f.timeout, "timeout", 10*time.Minute, "and in any case after this long")
	flag.BoolVar(&f.keep, "keep", false, "leave the fantasy running, to look around in it")
	flag.BoolVar(&f.merge, "merge", false, "then apply the change to the real cluster, unless it wrote the change's objects since the branch")
	flag.IntVar(&f.base, "port-base", 23000, "the fantasy's ports start here")
	empty := flag.Bool("empty", false, "start an empty cluster of its own and keep it: a second gym, for away matches")
	flag.Parse()
	if *empty {
		c := &fantasy.ControlPlane{Work: f.work, Bin: f.bin, Base: f.base}
		ctx := context.Background()
		if err := c.StartStore(ctx); err != nil {
			log.Fatal(err)
		}
		if err := c.StartControlPlane(ctx); err != nil {
			log.Fatal(err)
		}
		fmt.Println(c.Kubeconfig())
		select {}
	}
	if changes == "" {
		log.Fatal("-f: a change is needed")
	}
	f.changes = strings.Split(changes, ",")
	rep, err := run(context.Background(), f)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(rep.Text())
	if f.report != "" {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(f.report, raw, 0o600); err != nil {
			log.Fatal(err)
		}
	}
	if f.merge {
		if err := merge(context.Background(), f, rep.Branch); err != nil {
			log.Fatal(err)
		}
	}
}

// merge applies the change to the real cluster, as the fantasy did, when
// the cluster has not written the change's objects since the branch.
func merge(ctx context.Context, f flags, branch int64) error {
	objs, err := objectsIn(f.changes)
	if err != nil {
		return err
	}
	cfg, err := realConfig(f)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cfls, err := conflicts(ctx, cs, dyn, objs, branch)
	if err != nil {
		return err
	}
	if len(cfls) > 0 {
		return mergeError(cfls)
	}
	args := []string{"apply", "--server-side", "--force-conflicts", "--field-manager=fantasy"}
	if f.kubeconfig != "" {
		args = append([]string{"--kubeconfig", f.kubeconfig}, args...)
	}
	for _, file := range f.changes {
		out, err := exec.CommandContext(ctx, filepath.Join(f.bin, "kubectl"), append(args, "-f", file)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("merging %s: %v: %s", file, err, out)
		}
		fmt.Printf("♂ merged: %s", out)
	}
	return nil
}

func realConfig(f flags) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = f.kubeconfig
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
}

func run(ctx context.Context, f flags) (*fantasy.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout+5*time.Minute)
	defer cancel()
	// What the cluster held at the point.
	seed, err := fetchSeed(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	log.Printf("seed: %d keys at revision %d", len(seed.KVs), seed.Revision)

	c := &fantasy.ControlPlane{Work: f.work, Bin: f.bin, Base: f.base}
	defer func() {
		if !f.keep {
			c.Stop()
		}
	}()
	if err := c.StartStore(ctx); err != nil {
		return nil, err
	}
	kept, dropped, err := plant(ctx, c.StoreURL(), seed.KVs)
	if err != nil {
		return nil, fmt.Errorf("planting the seed: %w", err)
	}
	log.Printf("planted %d keys, left out %d", kept, dropped)
	if err := c.StartControlPlane(ctx); err != nil {
		return nil, err
	}
	store := &tape.Store{URL: c.TapeURL()}
	// The copy settles first: controllers and kwok catch up with what was
	// planted. What they change then is not the change's doing.
	before, err := fantasy.WaitQuiet(ctx, store, f.quiet, f.timeout)
	if err != nil {
		return nil, fmt.Errorf("settling: %w", err)
	}
	log.Printf("settled at revision %d; applying the change", before)
	for _, file := range f.changes {
		out, err := exec.CommandContext(ctx, filepath.Join(f.bin, "kubectl"), "--kubeconfig", c.Kubeconfig(),
			"apply", "--server-side", "--force-conflicts", "--field-manager=fantasy", "-f", file).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("applying %s: %v: %s", file, err, out)
		}
		log.Printf("%s", bytes.TrimSpace(out))
	}
	after, err := fantasy.WaitQuiet(ctx, store, f.quiet, f.timeout)
	if err != nil {
		return nil, fmt.Errorf("playing out: %w", err)
	}
	d, err := tape.DiffBetween(ctx, store, "/registry/", tape.Point{Rev: before}, tape.Point{Rev: after}, tape.Filter{})
	if err != nil {
		return nil, err
	}
	rep := fantasy.Summarize(d)
	rep.Branch, rep.Settled, rep.Over = seed.Revision, before, after
	rep.Changes = f.changes
	return rep, nil
}

// fetchSeed reads the raw values at the point through the real cluster's
// tape API, with the caller's rights: it takes the right to read Secrets.
func fetchSeed(ctx context.Context, f flags) (tape.Range, error) {
	var r tape.Range
	cfg, err := realConfig(f)
	if err != nil {
		return r, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return r, err
	}
	q := url.Values{"prefix": {"/registry/"}}
	if f.at != "" {
		q.Set("at", f.at)
	}
	req := cs.CoreV1().RESTClient().Get().AbsPath(tape.Prefix + "/raw")
	for k := range q {
		req = req.Param(k, q.Get(k))
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r)
}

// plant writes the seed into the fantasy's store, as it was stored, but
// for what a copy cannot or should not have: values the cluster encrypted
// with keys the copy does not hold, events, leases of the real control
// plane, and the API server's own bookkeeping, which the copy's API server
// keeps afresh.
func plant(ctx context.Context, endpoint string, kvs []tape.KV) (kept, dropped int, err error) {
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 10 * time.Second})
	if err != nil {
		return 0, 0, err
	}
	defer c.Close()
	for _, kv := range kvs {
		if !fantasy.Plantable(kv.Key, kv.Value) {
			dropped++
			continue
		}
		if _, err := c.Put(ctx, kv.Key, string(kv.Value)); err != nil {
			return kept, dropped, fmt.Errorf("%s: %w", kv.Key, err)
		}
		kept++
	}
	return kept, dropped, nil
}
