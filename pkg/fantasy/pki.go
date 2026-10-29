package fantasy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// pair is a certificate and its key.
type pair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newPair(cn string, orgs []string, signer *pair, ca bool, usage []x509.ExtKeyUsage, ips []net.IP, dns []string) (*pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 7),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	if ca {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	parent, parentKey := tmpl, key
	if signer != nil {
		parent, parentKey = signer.cert, signer.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &pair{cert: cert, key: key, der: der}, nil
}

func (p *pair) pem() (certPEM, keyPEM []byte, err error) {
	k, err := x509.MarshalECPrivateKey(p.key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: k}), nil
}

func (p *pair) write(dir, name string) error {
	c, k, err := p.pem()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), c, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".key"), k, 0o600)
}

// makePKI makes what the copy's control plane needs, for the API server
// on 127.0.0.1:apiPort: a cluster authority, the API server's certificate,
// a front proxy authority and client, the service account key, and
// kubeconfigs for an admin, the controller manager and the scheduler.
func makePKI(pki, kube string, apiPort int) error {
	for _, d := range []string{pki, kube} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	server := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	client := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	ca, err := newPair("fantasy-ca", nil, nil, true, nil, nil, nil)
	if err != nil {
		return err
	}
	api, err := newPair("kube-apiserver", nil, ca, false, server,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.96.0.1")},
		[]string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"})
	if err != nil {
		return err
	}
	fpca, err := newPair("front-proxy-ca", nil, nil, true, nil, nil, nil)
	if err != nil {
		return err
	}
	fpc, err := newPair("front-proxy-client", nil, fpca, false, client, nil, nil)
	if err != nil {
		return err
	}
	for name, p := range map[string]*pair{"ca": ca, "apiserver": api, "front-proxy-ca": fpca, "front-proxy-client": fpc} {
		if err := p.write(pki, name); err != nil {
			return err
		}
	}
	// Service account tokens: RSA, as the API server's defaults expect.
	sa, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(&sa.PublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pki, "sa.key"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(sa)}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pki, "sa.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o600); err != nil {
		return err
	}
	caPEM, _, err := ca.pem()
	if err != nil {
		return err
	}
	for file, who := range map[string][2]string{
		"super-admin.conf":        {"fantasy-admin", "system:masters"},
		"controller-manager.conf": {"system:kube-controller-manager", ""},
		"scheduler.conf":          {"system:kube-scheduler", ""},
	} {
		var orgs []string
		if who[1] != "" {
			orgs = []string{who[1]}
		}
		p, err := newPair(who[0], orgs, ca, false, client, nil, nil)
		if err != nil {
			return err
		}
		c, k, err := p.pem()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(kube, file), kubeconfig(apiPort, caPEM, c, k), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func kubeconfig(port int, ca, cert, key []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fantasy
  cluster:
    server: https://127.0.0.1:%d
    certificate-authority-data: %s
users:
- name: fantasy
  user:
    client-certificate-data: %s
    client-key-data: %s
contexts:
- name: fantasy
  context: {cluster: fantasy, user: fantasy}
current-context: fantasy
`, port, b64(ca), b64(cert), b64(key)))
}
