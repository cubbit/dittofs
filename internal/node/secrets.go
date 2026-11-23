package node

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// versionList is vs as a JSON array body, its numbers comma separated, so
// the prune's audit record names every version it drops.
func versionList(vs []uint64) string {
	var b strings.Builder

	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, "%d", v)
	}

	return b.String()
}

// keptFromRotation reports whether the store keeps kind's old versions once
// every node reports the new one (RFC 13 §7): a keytab's and a machine
// password's, kept for RFC 18's retention after the change at the KDC, and
// the signing key's, kept until the last access token it signed has expired.
func keptFromRotation(kind entity.SecretKind) bool {
	switch kind {
	case entity.SecretKeytab, entity.SecretMachinePassword, entity.SecretKey:
		return true
	}

	return false
}

// heldSecret is one reference the secrets pass opened and now holds: how the
// pass reads it again, the version the last pass read, the version its
// section then ran or the last one kept when it was held, and the lowest
// version that last pass still listed.
type heldSecret struct {
	kind   entity.SecretKind
	owner  entity.SecretOwner
	read   uint64
	held   uint64
	listed uint64
}

// secretsRead is one secrets pass: the walk's snapshot with every section it
// refused kept at what that section last ran, each reference the snapshot
// held, the version the pass read per reference, the lowest one it still
// listed, the sections whose reference's version moved since the last
// report, and the sections it refused with their error.
//
// decision: the versions are read before the apply of the moved sections, and
// a seal that lands meanwhile can only raise what the component then
// resolves, so the version the report carries is never above the one its
// holder holds (RFC 13 §7). Withdraw it if a holder ever resolves before the
// node reads.
type secretsRead struct {
	snap    *settings.Snapshot
	uses    []settings.SecretUse
	read    map[entity.SecretRef]uint64
	listed  map[entity.SecretRef]uint64
	moved   map[string]entity.SettingScope
	refused map[string]error
}

// secretsPass reads, after each settings walk and before any section applies,
// the version of every reference the walk's snapshot holds, under the role
// the kind is sealed under and the owner the section's write check sealed
// (RFC 13 §7; RFC 16 §2.3): a version moved since the last report runs its
// section again, and a reference that does not open holds or keeps its
// section whole at what it last ran, its rows refused, identity's sections
// with the rest (RFC 13 §6).
func (n *Node) secretsPass(ctx context.Context, wk settingsRead) secretsRead {
	r := &n.rec
	out := secretsRead{
		snap:    wk.snap,
		read:    map[entity.SecretRef]uint64{},
		listed:  map[entity.SecretRef]uint64{},
		moved:   map[string]entity.SettingScope{},
		refused: map[string]error{},
	}

	r.mu.Lock()

	held := make(map[entity.SecretRef]uint64, len(r.held))
	for ref, h := range r.held {
		held[ref] = h.held
	}
	r.mu.Unlock()

	out.uses = wk.snap.Secrets()

	for _, u := range out.uses {
		owner := entity.SecretOwner{Scope: u.Scope, Key: u.Key}

		vs, err := n.secrets.Versions(ctx, roleOfKind(u.Kind), u.Kind, owner, u.Ref)
		if err != nil {
			// ponytail: a read that fails, a store error among them,
			// refuses the section until the settings generation next moves,
			// as the keytab's read does (its ceiling in identity's keytab),
			// since the provider's error does not tell a store error from a
			// version that does not open. Retry it on reconcile's backoff
			// once the two can be told apart.
			out.refused[u.Section] = err

			continue
		}

		var lowest uint64
		if len(vs) > 0 {
			lowest = vs[len(vs)-1].Number
		}

		out.read[u.Ref] = vs[0].Number

		out.listed[u.Ref] = lowest
		if _, was := held[u.Ref]; was && held[u.Ref] != vs[0].Number {
			if _, seen := out.moved[u.Section]; !seen {
				out.moved[u.Section] = u.Scope
			}
		}
	}

	if len(out.refused) > 0 {
		prev := r.snapshot.Load()

		// The double Keep holds the section whole, as routeSettings does for
		// the sections it withholds: one Keep alone would leave a per-instance
		// section's other instances, which Read refused nothing of, running
		// their new values while the broken reference's is held back.
		for _, prefix := range slices.Sorted(maps.Keys(out.refused)) {
			out.snap = out.snap.Keep(prefix, nil).Keep(prefix, prev)
		}
	}

	return out
}

// heldSecretsGrow is r.held shrunk to the references the published snapshot
// names, then grown to what each section ran this pass: a section the pass or
// the walk refused, an apply that failed, one with no way to its component or
// identity's refusal keeps the version it last ran, and a refused reference
// keeps its entry whole. The caller holds r.mu.
func (n *Node) heldSecretsGrow(
	pass secretsRead,
	withheld map[string]bool,
	published *settings.Snapshot,
) {
	r := &n.rec

	// What the node reports holds what its published value names, and that
	// alone: a row switched to another reference leaves the old one out of
	// the status, and a later rotation of it moves neither the report nor
	// the count of secrets behind.
	names := make(map[entity.SecretRef]bool)
	for _, u := range published.Secrets() {
		names[u.Ref] = true
	}

	for ref := range r.held {
		if !names[ref] {
			delete(r.held, ref)
		}
	}

	for _, u := range pass.uses {
		if !names[u.Ref] {
			continue
		}

		read, ok := pass.read[u.Ref]
		if !ok {
			continue
		}

		h := heldSecret{
			kind:   u.Kind,
			owner:  entity.SecretOwner{Scope: u.Scope, Key: u.Key},
			read:   read,
			held:   r.held[u.Ref].held,
			listed: pass.listed[u.Ref],
		}

		if !withheld[u.Section] {
			h.held = read
		}

		r.held[u.Ref] = h
	}
}

// sectionOf is the section a refused setting's record names, the longest
// prefix the table gives its key; "" when the record is no setting.
func (n *Node) sectionOf(rec entity.ResourceRef) string {
	if rec.Kind != entity.ResourceSetting || len(rec.ID) == 0 {
		return ""
	}

	key := rec.ID[1:]
	if entity.ScopeKind(rec.ID[0]) != entity.ScopeInstallation {
		if len(key) < len(entity.NodeID{}) {
			return ""
		}

		key = key[len(entity.NodeID{}):]
	}

	return n.rec.table.PrefixOf(string(key))
}

// reportedSecrets is each reference the node reports in its status with the
// version it reports: those the last secrets pass read, at the version their
// section then ran or the last one kept when it was held, and identity's
// own, the lower when both report one.
func (n *Node) reportedSecrets() map[entity.SecretRef]uint64 {
	r := &n.rec
	r.mu.Lock()

	out := make(map[entity.SecretRef]uint64, len(r.held))
	for ref, h := range r.held {
		if h.held != 0 {
			out[ref] = h.held
		}
	}
	r.mu.Unlock()

	for _, sv := range n.identitySecrets() {
		if was, ok := out[sv.Secret]; ok && was <= sv.Version {
			continue
		}

		out[sv.Secret] = sv.Version
	}

	return out
}

// statusSecrets is the reported references and their versions, one entry per
// reference, sorted so the status carries them the same whatever turned
// first.
//
// decision: a version the report carries is the one the last pass read while
// its section ran, or lower (RFC 13 §7): the pass reads before the apply, so a
// seal that lands meanwhile can only raise what the component then resolves,
// never the reported version above the held one. Identity's own entries join
// in at the lower of the two, so an entry identity resolved keeps its reported
// version at what the component holds even when the pass read fresher.
func (n *Node) statusSecrets() []entity.SecretVersion {
	held := n.reportedSecrets()

	refs := make([]entity.SecretRef, 0, len(held))
	for ref := range held {
		refs = append(refs, ref)
	}

	slices.SortFunc(refs, func(a, b entity.SecretRef) int { return bytes.Compare(a[:], b[:]) })

	out := make([]entity.SecretVersion, 0, len(refs))
	for _, ref := range refs {
		out = append(out, entity.SecretVersion{Secret: ref, Version: held[ref]})
	}

	return out
}

// secretsBehind is how many references the node reports at a version below
// the store's current one, the current versions read at the scrape inside
// one store bound (RFC 13 §8): the count moves as soon as a seal lands,
// before the next pass applies it, and not one reference is named.
func (n *Node) secretsBehind() int {
	held := n.reportedSecrets()

	var behind int

	if len(held) == 0 {
		return 0
	}

	_, err := n.bounded(context.Background(), func(ctx context.Context) error {
		for ref, v := range held {
			s, err := n.meta.ControlPlane().Secret(ctx, ref, 0)
			if err != nil {
				// A reference that does not read has no current version to
				// be behind.
				continue
			}

			if s.Version > v {
				behind++
			}
		}

		return nil
	})
	if err != nil {
		return 0
	}

	return behind
}

// pruneSecrets drops the old versions of each reference the status just
// committed holds, once the pass listed one below the version it reports and
// no kind the store keeps across a rotation (RFC 13 §7): the prune commits
// inside an audited Write, its record naming the reference and the versions
// dropped (the control plane's report that the old credential may be revoked
// at its service), and one log line names the reference, never a value.
//
// ponytail: the audit record names every version below the reported one the
// last pass still listed, so a second node that holds an older version gets
// a record claiming drops the prune did not make: Provider.Prune itself
// floors at the oldest registered report, so the record overstates only the
// revocation notice, never a deletion. Read the prune's actual cut once a
// registered node besides this one exists.
func (n *Node) pruneSecrets(ctx context.Context, held []entity.SecretVersion) {
	r := &n.rec
	nodeID := n.cfg.Bootstrap.Node.ID

	for _, sv := range held {
		r.mu.Lock()
		h, ok := r.held[sv.Secret]
		r.mu.Unlock()

		if !ok || keptFromRotation(h.kind) || h.listed == 0 || h.listed >= sv.Version {
			continue
		}

		// Every version below the reported one falls together, however
		// many rotations landed before this pass: the report is the record
		// that each named version's credential may be revoked at its
		// service.
		dropped := make([]uint64, 0, sv.Version-h.listed)
		for v := h.listed; v < sv.Version; v++ {
			dropped = append(dropped, v)
		}

		ref := sv.Secret
		audit := &entity.AuditRecord{
			Node:   nodeID,
			Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: ref[:]},
			Change: []byte(
				fmt.Sprintf(`{"secret":"%x","dropped":[%s]}`, ref[:], versionList(dropped)),
			),
			Outcome: []byte("applied"),
		}

		_, err := n.bounded(ctx, func(ctx context.Context) error {
			return n.meta.ControlPlane().
				Write(ctx, entity.WriteMeta{Audit: audit}, func(tx entity.ControlPlaneTx) error {
					return n.secrets.Prune(ctx, tx, h.kind, h.owner, ref)
				})
		})
		if err != nil {
			n.cfg.Logger.Warn("the secrets pass did not prune below the reported version",
				"secret", fmt.Sprintf("%x", ref[:]), "error", err)

			continue
		}

		r.mu.Lock()
		if h, ok := r.held[ref]; ok {
			h.listed = sv.Version
			r.held[ref] = h
		}
		r.mu.Unlock()

		n.cfg.Logger.Info("pruned a rotated secret's old versions below the reported one",
			"secret", fmt.Sprintf("%x", ref[:]), "below", sv.Version)
	}
}
