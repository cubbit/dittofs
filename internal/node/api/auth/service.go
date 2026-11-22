package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/internal/node/api/apisettings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// accountCheck is identity's account check, as sign-in and the per-call check
// call it: the principal a name resolves to, its password's check, and its
// current record.
type accountCheck interface {
	Lookup(ctx context.Context, name string) (entity.Principal, error)
	VerifyPassword(ctx context.Context, p entity.Principal, password []byte) error
	Account(ctx context.Context, p entity.Principal) (entity.User, error)
}

// store is the control plane as sign-in reaches it: the counts, the
// sessions and the settings read untracked, and the write.
type store interface {
	signInStore
	Session(ctx context.Context, id [16]byte) (entity.Session, bool, error)
	SessionByRefresh(ctx context.Context, hash [32]byte) (entity.Session, bool, error)
	Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error]
}

// Deps is what a Service is built from.
type Deps struct {
	Store    store
	Accounts accountCheck
	Tokens   *Tokens
	// Sealer seals a password's hashes before the write that stores them;
	// nil, every password write answers ErrUnavailable.
	Sealer Sealer
	// Settings is the api section as the node holds it now, read at each
	// sign-in, so a live api.session_lifetime reaches the next session.
	Settings func() apisettings.Settings
	Clock    Clock
	Metrics  prometheus.Registerer
}

// Service signs callers in, rotates their sessions' refresh tokens and
// answers who each call is.
type Service struct {
	d        Deps
	signIns  *prometheus.CounterVec
	failures *prometheus.CounterVec
}

// NewService builds the service over d and registers its two series.
func NewService(d Deps) (*Service, error) {
	s := &Service{
		d: d,
		signIns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dittofs_api_sign_ins_total",
			Help: "Sign-ins, by method and result: ok or the problem's code."}, []string{"method", "result"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dittofs_api_auth_failures_total",
			Help: "API calls refused authentication, by reason."}, []string{"reason"}),
	}
	for _, c := range []prometheus.Collector{s.signIns, s.failures} {
		if err := d.Metrics.Register(c); err != nil {
			return nil, fmt.Errorf("auth: register the sign-in series: %w", err)
		}
	}
	return s, nil
}

// refreshLen is a refresh token's length in random bytes before encoding.
const refreshLen = 32

// Audited is what a sign-in or a refresh recorded in the audit, which the
// API counts its audit records by: nothing, or one record applied or
// refused.
type Audited string

const (
	AuditedNothing Audited = ""
	AuditedApplied Audited = "applied"
	AuditedRefused Audited = "refused"
)

// resultOf is a sign-in's result as its series labels it: ok, or the
// problem's code its error answers.
func resultOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrThrottled):
		return string(apiv1.CodeThrottled)
	case errors.Is(err, vfserr.ErrLogonFailure):
		return string(apiv1.CodeUnauthenticated)
	}
	return string(apiv1.CodeUnavailable)
}

// newRefresh is a fresh refresh token and the hash a session keeps of it.
func newRefresh() (string, [32]byte) {
	b := make([]byte, refreshLen)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b), sha256.Sum256(b)
}

// refreshHash is the hash of the refresh token raw, false when raw is no
// token newRefresh could have made.
func refreshHash(raw string) ([32]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != refreshLen {
		return [32]byte{}, false
	}
	return sha256.Sum256(b), true
}

// before reports whether t is before notBefore, in whole seconds; a zero
// notBefore is before every time.
func before(t, notBefore time.Time) bool { return t.Unix() < notBefore.Unix() }

// issuedAt is a credential's issue or creation time: the write's store time
// in whole seconds, raised to the account's not-before when that is later.
func issuedAt(now, notBefore time.Time) time.Time {
	now = now.Truncate(time.Second)
	if before(now, notBefore) {
		return notBefore.Truncate(time.Second)
	}
	return now
}

// tokens is the answer of a sign-in or a refresh: an access token signed
// with k, read before the write, and issued at iat, its expiry, and refresh.
// It reads nothing, so once the write has committed nothing can fail it.
func tokens(k KeyVersion, p entity.Principal, session [16]byte, iat time.Time, refresh string, mustChange bool) (apiv1.Tokens, error) {
	access, err := sign(k, p, session, iat)
	if err != nil {
		return apiv1.Tokens{}, unavailable(err)
	}
	return apiv1.Tokens{Access: access, Refresh: refresh, ExpiresAt: iat.Add(accessLife), MustChange: mustChange}, nil
}

// unavailable is err, a store or signing-key read that failed, as a store
// that cannot answer, so the API answers 503 and never a refusal; a refusal,
// a throttle or an error already saying so is left as it is.
func unavailable(err error) error {
	if err == nil || errors.Is(err, vfserr.ErrLogonFailure) || errors.Is(err, ErrThrottled) || errors.Is(err, vfserr.ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
}

// Refused counts a refused authentication by its reason and returns the
// refusal, ErrLogonFailure.
func (s *Service) Refused(reason string) error {
	s.failures.WithLabelValues(reason).Inc()
	return vfserr.ErrLogonFailure
}
