package keyprovider

import (
	"context"
	"errors"
	"testing"
)

// NewProviderWithPassphrase takes the passphrase from its caller: the key
// file loads with the passphrase given, and an empty passphrase is
// ErrPassphraseMissing.
func TestNewProviderWithPassphrase(t *testing.T) {
	t.Parallel()

	path := writeKeyFile(t, testPassphrase)

	p, err := NewProviderWithPassphrase(
		context.Background(),
		Config{Kind: KindLocal, File: path},
		testPassphrase,
	)
	if err != nil {
		t.Fatalf("NewProviderWithPassphrase: %v", err)
	}

	t.Cleanup(func() {
		_ = p.Close()
	})

	if p.CurrentMasterKeyID() == "" {
		t.Error("CurrentMasterKeyID: got the empty id, want the key file's id")
	}
}

func TestNewProviderWithPassphraseEmpty(t *testing.T) {
	t.Parallel()

	path := writeKeyFile(t, testPassphrase)

	if _, err := NewProviderWithPassphrase(
		context.Background(),
		Config{Kind: KindLocal, File: path},
		"",
	); !errors.Is(err, ErrPassphraseMissing) {
		t.Fatalf("empty passphrase: want ErrPassphraseMissing, got %v", err)
	}
}

// NewProvider reads the passphrase the way it always did, and the caller's
// passphrase always wins over the variable: with the variable set, an
// empty passphrase argument still refuses to load the key file. Not
// parallel: it sets the host's passphrase variable.
func TestNewProviderPassphraseSource(t *testing.T) {
	path := writeKeyFile(t, testPassphrase)

	t.Setenv(localPassphraseEnv, testPassphrase)

	p, err := NewProvider(context.Background(), Config{Kind: KindLocal, File: path})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() {
		_ = p.Close()
	})

	if _, err := NewProviderWithPassphrase(
		context.Background(), Config{Kind: KindLocal, File: path}, "",
	); !errors.Is(err, ErrPassphraseMissing) {
		t.Fatalf("empty passphrase with the variable set: want ErrPassphraseMissing, got %v", err)
	}
}
