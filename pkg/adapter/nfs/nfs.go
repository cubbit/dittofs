// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the NFS adapter, configured: it validates s and loads its TLS
// files, refusing a setting it cannot apply, and opens nothing.
func New(d adapter.Deps, s config.NFS) (adapter.Adapter, error) {
	if err := config.Validate(s); err != nil {
		return nil, err
	}
	if err := loadTLS(s); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("nfs: the NFS adapter is not built: %w", errors.ErrUnsupported)
}

// loadTLS loads the certificate and its key, and the client CA where set,
// naming the field of the first that does not load. TLS off loads nothing.
func loadTLS(s config.NFS) error {
	t := s.TLS
	if t.CertFile == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile); err != nil {
		field := "TLS.KeyFile"
		if pem, rerr := os.ReadFile(t.CertFile); rerr != nil || !x509.NewCertPool().AppendCertsFromPEM(pem) {
			field = "TLS.CertFile"
		}
		return fmt.Errorf("nfs: %s: %w", field, err)
	}
	if t.ClientCA == "" {
		return nil
	}
	pem, err := os.ReadFile(t.ClientCA)
	if err != nil {
		return fmt.Errorf("nfs: TLS.ClientCA: %w", err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return fmt.Errorf("nfs: TLS.ClientCA: %s holds no PEM certificate", t.ClientCA)
	}
	return nil
}
