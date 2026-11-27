package identity

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// LogonSink receives one LogonEvent per sign-in attempt (RFC 18 §3.1), with
// the method shape of RFC 25's logon stream. It must not block the sign-in.
type LogonSink interface {
	Logon(ctx context.Context, e *LogonEvent)
}

// LogonEvent is one sign-in attempt, as RFC 25 §4.2's logon fields name it.
// It never carries the credential: no response, ticket, hash or key.
type LogonEvent struct {
	Time      time.Time
	Outcome   string // ok, or why the attempt failed: throttled when refused unchecked
	Mechanism string // ntlm or kerberos
	Protocol  string // the file protocol the attempt arrived over
	Name      string // the user name as presented
	Principal entity.PrincipalID
	Client    netip.Addr
}

// LogLines is a LogonSink writing each event as one Info line on l, without
// the name as presented, which RFC 25 §5.4 keeps out of every log line.
//
// ponytail: logon events are log lines, so the presented name of a failed
// attempt is recorded nowhere. Replace it with RFC 25's logon stream, whose
// records carry the name and are read only with the right to read the audit,
// when that sink lands.
func LogLines(l *slog.Logger) LogonSink { return logLines{l} }

type logLines struct{ l *slog.Logger }

func (s logLines) Logon(ctx context.Context, e *LogonEvent) {
	attrs := []slog.Attr{
		slog.Time("at", e.Time),
		slog.String("outcome", e.Outcome),
		slog.String("mechanism", e.Mechanism),
		slog.String("protocol", e.Protocol),
		slog.String("client", e.Client.String()),
	}
	if e.Principal != (entity.PrincipalID{}) {
		attrs = append(attrs, slog.String("principal", e.Principal.String()))
	}
	s.l.LogAttrs(ctx, slog.LevelInfo, "identity: logon", attrs...)
}
