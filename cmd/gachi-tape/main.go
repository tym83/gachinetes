// gachi-tape serves the cluster's tape decoded, as the aggregated API
// tape.gachinetes.dev. It runs on a control plane node, next to the store
// member whose tape it reads, and answers only the API server.
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
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/tym83/gachinetes/pkg/tape"
)

func main() {
	store := flag.String("store", "http://127.0.0.1:2382", "the store member's tape")
	listen := flag.String("listen", ":9282", "where the API is served, HTTPS")
	flag.Parse()
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	rh, err := tape.LoadRequestHeader(context.Background(), cs)
	if err != nil {
		log.Fatal(err)
	}
	api := tape.Handler(&tape.Store{URL: *store, Client: &http.Client{Timeout: time.Minute}})
	disc := tape.Discovery()
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, tape.Prefix+"/") {
			api.ServeHTTP(w, r)
			return
		}
		disc.ServeHTTP(w, r)
	})
	cert, err := selfSigned()
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:    *listen,
		Handler: tape.Guard(rh, cs, root),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
			// The API server's proxy certificate is checked per request,
			// so that probes without one reach /healthz.
			ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: rh.CA},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("♂ gachi-tape: reading %s, serving %s on %s", *store, tape.Prefix, *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// selfSigned is the serving certificate; the APIService skips verifying
// it (see deploy/), as metrics-server's default install does.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "gachi-tape.gachinetes.svc"},
		DNSNames:     []string{"gachi-tape.gachinetes.svc"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
