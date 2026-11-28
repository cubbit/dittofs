package keyprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gemalto/kmip-go/kmip14"

	"github.com/cubbit/dittofs/internal/kmip"
	"github.com/cubbit/dittofs/internal/logger"
)

// kmipProvider fetches a master symmetric key from a KMIP-speaking HSM
// at startup, caches the bytes in memory for the daemon's lifetime, and
// performs per-block wrap / unwrap locally using AES-256-GCM.
//
// Trade-off vs full HSM-resident envelope ops (KMIP Encrypt / Decrypt)
// the master-key bytes live in process memory while the daemon runs, so
// a compromise of the daemon's address space recovers the key. The HSM
// is still the canonical custodian. A follow-up can move Wrap inside the
// HSM via KMIP Encrypt / Decrypt without changing this package's public
// surface (the KeyProvider interface stays the same).
//
// Pointing KeyUID at a different HSM key does NOT rotate the master key:
// only the newly fetched key is held, so every block wrapped under the
// previous key becomes permanently unreadable. See the package doc.
type kmipProvider struct {
	aesGCMKEK
}

func newKMIPProvider(ctx context.Context, cfg Config) (*kmipProvider, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%w: kmip endpoint required", ErrInvalidConfig)
	}
	if cfg.KeyUID == "" {
		return nil, fmt.Errorf("%w: kmip key_uid required", ErrInvalidConfig)
	}
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, fmt.Errorf("%w: kmip client_cert and client_key required", ErrInvalidConfig)
	}
	tlsCfg, err := buildKMIPTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	// A TimeoutMS of zero is the transport's default.
	client := kmip.NewClient(cfg.Endpoint, tlsCfg, time.Duration(cfg.TimeoutMS)*time.Millisecond)
	// The state check runs before the Get so that key material the HSM has
	// disowned is never pulled into this process at all.
	state, err := client.State(ctx, cfg.KeyUID)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: %w", err)
	}
	if err := currentKeyStateErr(cfg.KeyUID, state); err != nil {
		return nil, err
	}
	key, err := fetchKMIPKey(ctx, client, cfg.KeyUID)
	if err != nil {
		return nil, err
	}
	// A KMIP key is identified by the uid the operator configured, so a
	// retired uid is its own master key id with nothing to look up.
	retired, err := loadRetiredKeys(cfg.KeyUID, cfg.RetiredKeyUIDs, kmipRetiredKeyLoader(ctx, client))
	if err != nil {
		zeroKey(key)
		return nil, err
	}
	return &kmipProvider{aesGCMKEK: aesGCMKEK{
		masterKey:   key,
		masterKeyID: cfg.KeyUID,
		retired:     retired,
	}}, nil
}

// currentKeyStateErr refuses a current key in any state but Active: an
// operator who moves a key out of Active expects new writes under it to
// stop.
func currentKeyStateErr(uid string, state kmip14.State) error {
	if state == kmip14.StateActive {
		return nil
	}
	return fmt.Errorf("%w: kmip key %q is in state %s at the HSM, not Active; "+
		"the encrypted remote will not come up until the configured key_uid names an Active key",
		ErrKeyStateUnusable, uid, state)
}

// retiredKeyStateErr refuses a retired key only when its material is gone.
// A retired key is decrypt-only, so states that disqualify the current key
// do not disqualify it: blocks already written under it still have to be
// readable.
func retiredKeyStateErr(uid string, state kmip14.State) error {
	if kmip.Classify(state, nil) != kmip.Destroyed {
		return nil
	}
	return fmt.Errorf("%w: kmip key %q is in state %s at the HSM, so its material is gone",
		ErrKeyStateUnusable, uid, state)
}

// kmipRetiredKeyLoader resolves one retired uid into its key material. A
// key whose material the HSM has destroyed says so in those terms rather
// than surfacing as a generic fetch failure. A compromised one is loaded
// with a loud warning, because it is the input to a re-wrap decision.
func kmipRetiredKeyLoader(ctx context.Context, client *kmip.Client) retiredKeyLoader {
	return func(uid string) (string, []byte, error) {
		state, err := client.State(ctx, uid)
		if err != nil {
			return uid, nil, fmt.Errorf("keyprovider: %w", err)
		}
		if err := retiredKeyStateErr(uid, state); err != nil {
			return uid, nil, err
		}
		if state == kmip14.StateCompromised {
			logger.Warn("retired master key is marked Compromised at the HSM; blocks already written are still being read under it",
				"key_uid", uid)
		}
		k, err := fetchKMIPKey(ctx, client, uid)
		return uid, k, err
	}
}

// fetchKMIPKey retrieves one symmetric key from the HSM; a key that is not
// the AES-256 length the wrap layer requires is a configuration error.
func fetchKMIPKey(ctx context.Context, client *kmip.Client, uid string) ([]byte, error) {
	key, err := client.GetSymmetricKey(ctx, uid)
	if err != nil {
		return nil, kmipKeyErr(err)
	}
	return key, nil
}

// kmipKeyErr maps a failed Get: a key of the wrong length is a
// configuration error, anything else a fetch failure.
func kmipKeyErr(err error) error {
	if errors.Is(err, kmip.ErrKeyLength) {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return fmt.Errorf("keyprovider: %w", err)
}

func buildKMIPTLSConfig(cfg Config) (*tls.Config, error) {
	certPEM, err := os.ReadFile(cfg.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: load kmip client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: read kmip client key: %w", err)
	}
	var caPEM []byte
	if cfg.ServerCA != "" {
		if caPEM, err = os.ReadFile(cfg.ServerCA); err != nil {
			return nil, fmt.Errorf("keyprovider: read kmip server CA: %w", err)
		}
	}
	tlsCfg, err := kmip.TLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, fmt.Errorf("keyprovider: %w", err)
	}
	return tlsCfg, nil
}
