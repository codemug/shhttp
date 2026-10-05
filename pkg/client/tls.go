package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// WithTLSConfig makes the client use this TLS configuration, for example
// one from TLSFromFiles.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *Client) {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = cfg
		c.http = &http.Client{Transport: t}
	}
}

// TLSFromFiles builds a client TLS configuration. caFile (optional) is a
// PEM file of CAs to trust instead of the system's; certFile and keyFile
// (optional, together) are a client certificate for servers that require
// one.
func TLSFromFiles(caFile, certFile, keyFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contains no PEM certificates", caFile)
		}
		cfg.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("a client certificate needs both a certificate and a key file")
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
