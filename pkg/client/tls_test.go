package client_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/internal/config"
	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/codemug/shhttp/v2/pkg/client"
)

// writeCert creates a certificate signed by parent (self-signed when nil)
// and writes it and its key as PEM files.
func writeCert(t *testing.T, dir, name string, ca bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  ca,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	if parent == nil {
		parent, parentKey = tpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func TestClientCertificates(t *testing.T) {
	dir := t.TempDir()
	ca, caKey := writeCert(t, dir, "ca", true, nil, nil)
	writeCert(t, dir, "server", false, ca, caKey)
	writeCert(t, dir, "client", false, ca, caKey)
	other, otherKey := writeCert(t, dir, "otherca", true, nil, nil)
	writeCert(t, dir, "stranger", false, other, otherKey)
	p := func(n string) string { return filepath.Join(dir, n) }

	cfg := config.Config{TLSCert: p("server.pem"), TLSKey: p("server.key"), ClientCA: p("ca.pem")}
	tlsCfg, err := cfg.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Version{Version: "tls"})
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	ctx := t.Context()
	good, err := client.TLSFromFiles(p("ca.pem"), p("client.pem"), p("client.key"))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := client.New(srv.URL, "", client.WithTLSConfig(good)).Version(ctx); err != nil || v != "tls" {
		t.Fatalf("with a client certificate: %q, %v", v, err)
	}
	noCert, _ := client.TLSFromFiles(p("ca.pem"), "", "")
	if _, err := client.New(srv.URL, "", client.WithTLSConfig(noCert)).Version(ctx); err == nil {
		t.Fatal("connected without a client certificate")
	}
	wrongCA, _ := client.TLSFromFiles(p("ca.pem"), p("stranger.pem"), p("stranger.key"))
	if _, err := client.New(srv.URL, "", client.WithTLSConfig(wrongCA)).Version(ctx); err == nil {
		t.Fatal("connected with a certificate from another CA")
	}
	if _, err := client.TLSFromFiles("", p("client.pem"), ""); err == nil {
		t.Fatal("certificate without key accepted")
	}
}
