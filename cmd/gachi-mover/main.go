// gachi-mover moves running pods between nodes. On every node it runs as
// an agent that hands out the kubelet's checkpoints and takes them in as
// images the runtime restores; on a control plane it runs the Migration
// controller: checkpoint on one node, restore on another, the old pod
// gone. Get your ass back here.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"log"
	"math/big"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tym83/gachinetes/pkg/mover"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "the cluster: the kubelet's for an agent, an admin's for the controller")
	agent := flag.String("agent", "", "run the agent of this node, listening on this address (host:port)")
	node := flag.String("node", "", "the agent's node name")
	controller := flag.Bool("controller", false, "run the Migration controller")
	agentPort := flag.String("agent-port", "9600", "the port agents listen on")
	namespace := flag.String("namespace", "gachinetes", "where the controller's service account lives")
	flag.Parse()
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	if *agent != "" {
		a := &Agent{cs: cs, node: *node, dir: checkpointDir, registry: &mover.Registry{Dir: registryDir}}
		// The node's runtime pulls checkpoints from here, on the loopback
		// only.
		go func() {
			srv := &http.Server{Addr: "127.0.0.1:5001", Handler: a.registry.Handler(), ReadHeaderTimeout: 10 * time.Second}
			log.Fatal(srv.ListenAndServe())
		}()
		go func() {
			cert, err := selfSigned()
			if err != nil {
				log.Fatal(err)
			}
			srv := &http.Server{Addr: *agent, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second,
				TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
			log.Printf("♂ gachi-mover agent of %s on %s", *node, *agent)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	}
	if *controller {
		c, err := NewController(cfg, cs, *namespace, *agentPort)
		if err != nil {
			log.Fatal(err)
		}
		log.Print("♂ gachi-mover controller")
		go c.Run(ctx)
	}
	select {}
}

// registryDir keeps the checkpoint images a node takes in.
const registryDir = "/var/lib/gachinetes/checkpoints"

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "gachi-mover"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
