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
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/tym83/gachinetes/pkg/tape"
)

func main() {
	store := flag.String("store", "http://127.0.0.1:2382", "the store member's tape")
	listen := flag.String("listen", ":9282", "where the API is served, HTTPS")
	kubeconfig := flag.String("kubeconfig", "", "the cluster to ask, when not run in it")
	register := flag.String("register", "", "run as a node service: keep an endpoint of the gachi-tape Service in --namespace at this address")
	namespace := flag.String("namespace", "gachinetes", "the namespace of the gachi-tape Service")
	encryption := flag.String("encryption-config", "", "the API server's encryption configuration, to open the Secrets it encrypted")
	flag.Parse()
	cfg, err := rest.InClusterConfig()
	if *kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		log.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if *register != "" {
		go keepEndpoint(context.Background(), cs, *namespace, *register, *listen)
	}
	rh, err := tape.LoadRequestHeader(context.Background(), cs)
	if err != nil {
		log.Fatal(err)
	}
	st := &tape.Store{URL: *store, Client: &http.Client{Timeout: time.Minute}}
	if *encryption != "" {
		if st.Decrypter, err = tape.LoadDecrypter(*encryption); err != nil {
			log.Fatal(err)
		}
	}
	api := tape.Handler(st)
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

// keepEndpoint keeps an EndpointSlice of the gachi-tape Service pointing at
// this node's address, for a tape run as a node service rather than a pod.
func keepEndpoint(ctx context.Context, cs kubernetes.Interface, namespace, addr, listen string) {
	_, portStr, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(portStr)
	name := "gachi-tape-" + strings.ReplaceAll(addr, ".", "-")
	want := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: "gachi-tape", discoveryv1.LabelManagedBy: "gachi-tape"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
		Ports:       []discoveryv1.EndpointPort{{Port: ptr.To(int32(port)), Protocol: ptr.To(corev1.ProtocolTCP)}},
	}
	for {
		api := cs.DiscoveryV1().EndpointSlices(namespace)
		cur, err := api.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = api.Create(ctx, want, metav1.CreateOptions{FieldManager: "gachi-tape"})
		case err == nil:
			want.ResourceVersion = cur.ResourceVersion
			_, err = api.Update(ctx, want, metav1.UpdateOptions{FieldManager: "gachi-tape"})
		}
		if err != nil {
			log.Printf("endpoint: %v", err)
		}
		time.Sleep(time.Minute)
	}
}
