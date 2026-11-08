package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/tym83/gachinetes/pkg/fantasy"
)

// preview plays the change out as a preview: the report and a kubeconfig of
// the branch go to the preview's ConfigMap and Secret, and the branch stays
// up for its time to live, or until the preview is discarded.
func preview(f flags) error {
	cfg, err := realConfig(f)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	c := f.controlPlane()
	defer c.Stop()
	rep, err := run(ctx, f, c)
	if err == nil {
		var admin []byte
		if admin, err = os.ReadFile(c.Kubeconfig()); err == nil {
			err = fantasy.PublishKubeconfig(ctx, cs, f.previewNS, f.preview, admin)
		}
	}
	if perr := fantasy.PublishReport(ctx, cs, f.previewNS, f.preview, rep, err); perr != nil {
		return fmt.Errorf("publishing the report: %w (the fantasy: %v)", perr, err)
	}
	if err != nil {
		return err
	}
	fmt.Print(rep.Text())
	log.Printf("♂ preview %s up for %s", f.preview, f.ttl)
	select {
	case <-ctx.Done():
		log.Printf("preview %s discarded", f.preview)
	case <-time.After(f.ttl):
		log.Printf("preview %s ran out", f.preview)
	}
	return nil
}
