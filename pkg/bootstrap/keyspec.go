package bootstrap

import (
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/cubbit/dittofs/internal/configunits"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// kind is how a key's value is written and checked.
type kind int

const (
	kindString     kind = iota // any string
	kindEnum                   // one of spec.enum
	kindPath                   // an absolute, clean path
	kindOutput                 // stdout, stderr, or a path
	kindUUID                   // a UUID
	kindSocket                 // a path within the socket-address limit
	kindListenOff              // ip:port, or off
	kindListenList             // a list of ip:port
	kindIPList                 // a list of IP literals
	kindEnumList               // a list of spec.enum's values
	kindPathList               // a list of paths
	kindStringList             // a list of strings
	kindJournals               // a list of entries, each with a path
	kindBool                   // true or false
	kindCount                  // an integer, 0 or more
	kindSizeMiB                // a size, a whole number of MiB of at least 1MiB
	kindDays                   // a duration, a whole number of days
	kindDuration               // a duration
	kindLegacyPath             // blockstore.journal.path, read as before
)

// keySpec is one key of RFC 13 Appendix C: its name, how it is written, its
// default, and whether its value may hold a secret, so no refusal prints it.
type keySpec struct {
	key    string
	kind   kind
	enum   []string
	def    any
	secret bool
	// nullUnset: a YAML null or an empty variable reads as not set. Only
	// metrics.listen's row says so (RFC 13 Appendix C); every other key
	// refuses an empty value by its own rule.
	nullUnset bool
}

// roleKeys are one role's wrapping-key keys, under keys.<role>.
func roleKeys(role string) []keySpec {
	p := "keys." + role + "."

	return []keySpec{
		{key: p + "kind", kind: kindEnum, enum: []string{"local", "kmip"}, def: ""},
		{key: p + "local.file", kind: kindPath, def: "", secret: true},
		{key: p + "local.retired_files", kind: kindPathList, def: []string(nil), secret: true},
		{key: p + "kmip.endpoint", kind: kindString, def: ""},
		{key: p + "kmip.server_ca", kind: kindPath, def: "", secret: true},
		{key: p + "kmip.client_cert", kind: kindPath, def: "", secret: true},
		{key: p + "kmip.client_key", kind: kindPath, def: "", secret: true},
		{key: p + "kmip.key_uid", kind: kindString, def: ""},
		{key: p + "kmip.timeout", kind: kindDuration, def: time.Duration(0)},
		{key: p + "kmip.retired_key_uids", kind: kindStringList, def: []string(nil)},
	}
}

// keySpecs are RFC 13 Appendix C's keys, in its table's order, then the v1
// key the bootstrap still reads. A "did you mean" tie goes to the first.
var keySpecs = slices.Concat(
	[]keySpec{
		{key: "node.id", kind: kindUUID, def: entity.NodeID{}},
		{
			key: "node.roles", kind: kindEnumList, enum: []string{"protocol", "storage"},
			def: []string{"protocol", "storage"},
		},
		{
			key: "metadata.backend", kind: kindEnum, enum: []string{"badger", "foundationdb"},
			def: "badger",
		},
		{key: "metadata.badger.path", kind: kindPath, def: ""},
		{key: "metadata.foundationdb.cluster_file", kind: kindPath, def: ""},
		{key: "metadata.foundationdb.tls.cert_file", kind: kindPath, def: "", secret: true},
		{key: "metadata.foundationdb.tls.key_file", kind: kindPath, def: "", secret: true},
		{key: "metadata.foundationdb.tls.ca_file", kind: kindPath, def: "", secret: true},
		{key: "journals", kind: kindJournals, def: []Journal(nil)},
		{key: "adapters.listen", kind: kindIPList, def: []string(nil)},
		{key: "api.listen", kind: kindListenList, def: []string{"127.0.0.1:8080"}},
		{key: "api.tls.cert_file", kind: kindPath, def: "", secret: true},
		{key: "api.tls.key_file", kind: kindPath, def: "", secret: true},
		{key: "api.socket", kind: kindSocket, def: ""},
		{key: "metrics.listen", kind: kindListenOff, def: "127.0.0.1:9090", nullUnset: true},
		{
			key: "logging.level", kind: kindEnum, enum: []string{"debug", "info", "warn", "error"},
			def: "info",
		},
		{key: "logging.format", kind: kindEnum, enum: []string{"text", "json"}, def: "text"},
		{key: "logging.output", kind: kindOutput, def: nil},
		{key: "logging.rotation.max_size", kind: kindSizeMiB, def: 100},
		{key: "logging.rotation.max_backups", kind: kindCount, def: 5},
		{key: "logging.rotation.max_age", kind: kindDays, def: 30},
		{key: "logging.rotation.compress", kind: kindBool, def: false},
	},
	roleKeys("protocol"),
	roleKeys("storage"),
	[]keySpec{
		{key: "blockstore.journal.path", kind: kindLegacyPath, def: ""},
	},
)

// specByKey indexes keySpecs; blocks holds every prefix of a declared key,
// the mappings the file nests keys under.
var specByKey, blocks = func() (map[string]*keySpec, map[string]bool) {
	byKey := make(map[string]*keySpec, len(keySpecs))
	prefixes := map[string]bool{}

	for i := range keySpecs {
		key := keySpecs[i].key
		byKey[key] = &keySpecs[i]

		for j := range len(key) {
			if key[j] == '.' {
				prefixes[key[:j]] = true
			}
		}
	}

	return byKey, prefixes
}()

// envName is the key's variable: DITTOFS_, then the key in upper case with
// its dots as underscores.
func (s *keySpec) envName() string {
	return "DITTOFS_" + strings.ToUpper(strings.ReplaceAll(s.key, ".", "_"))
}

// defaultValue is the key's value when no source sets it. logging.output's
// follows the state directory.
func (s *keySpec) defaultValue() any {
	if s.key == "logging.output" {
		return getDefaultLogPath()
	}

	if l, ok := s.def.([]string); ok {
		return slices.Clone(l)
	}

	return s.def
}

// isNull reports whether n is a YAML null.
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// typeName names n's YAML type, as a refusal names the type it got.
func typeName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}

	switch n.ShortTag() {
	case "!!str":
		return "a string"
	case "!!int", "!!float":
		return "a number"
	case "!!bool":
		return "a boolean"
	case "!!null":
		return "null"
	}

	// Any other tag is the file's own text, so it is not repeated.
	return "a value of another type"
}

// isString reports whether n is a string scalar; a null counts as the empty
// string, which each key's own rule then refuses.
func isString(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && (n.ShortTag() == "!!str" || n.ShortTag() == "!!null")
}

// form is what a type refusal says the key must be.
func (s *keySpec) form() string {
	switch s.kind {
	case kindSizeMiB:
		return `a size such as "100MiB"`
	case kindDays:
		return `a duration such as "720h"`
	case kindDuration:
		return `a duration such as "5s"`
	case kindBool:
		return "true or false"
	case kindCount:
		return "an integer"
	case kindUUID:
		return "a UUID"
	case kindEnumList, kindListenList, kindIPList, kindPathList, kindStringList, kindJournals:
		return "a list"
	}

	return "a string"
}

// fromNode reads and checks the file's value of the key. A type refusal
// names the type, never the value: a decoder's own error quotes it.
func (s *keySpec) fromNode(n *yaml.Node, path string, rep *report) (any, bool) {
	c := check{where: path, key: s.key, show: !s.secret, rep: rep}

	switch s.kind {
	case kindBool:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
			return c.typeRefusal(s, n)
		}

		// The scalar's text, as fromEnv reads it: yaml.v3 still resolves
		// TRUE and True as booleans (RFC 13 Appendix B, "true or false,
		// and nothing else").
		if n.Value != "true" && n.Value != "false" {
			return nil, c.refuse(c.got("must be true or false", n.Value))
		}

		return n.Value == "true", true
	case kindCount:
		// The scalar's text, as fromEnv reads it: yaml.v3 resolves 010 as
		// 8 and accepts 0x10 and 1_0, while the variable reads 010 as 10
		// and refuses the other two.
		i, err := strconv.Atoi(n.Value)
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" || err != nil {
			return c.typeRefusal(s, n)
		}

		return i, c.count(i)
	case kindJournals:
		return s.journalsFromNode(n, c)
	case kindEnumList, kindListenList, kindIPList, kindPathList, kindStringList:
		if n.Kind != yaml.SequenceNode {
			return c.typeRefusal(s, n)
		}

		list := make([]string, 0, len(n.Content))

		for _, e := range n.Content {
			e = deref(e)
			if !isString(e) {
				c.rep.add(
					c.where,
					c.key,
					"must be a list of strings, got a list holding "+typeName(e),
				)

				return nil, false
			}

			list = append(list, e.Value)
		}

		return s.check(list, c)
	}

	if !isString(n) {
		return c.typeRefusal(s, n)
	}

	return s.check(n.Value, c)
}

// journalsFromNode reads the file's journals: a list of entries, each a
// mapping whose path names one directory.
func (s *keySpec) journalsFromNode(n *yaml.Node, c check) (any, bool) {
	if n.Kind != yaml.SequenceNode {
		return c.typeRefusal(s, n)
	}

	paths := make([]string, 0, len(n.Content))
	ok := true

	for _, e := range n.Content {
		e = deref(e)
		if e.Kind != yaml.MappingNode {
			c.rep.add(c.where, c.key, "each entry must be a mapping with a path, got "+typeName(e))

			ok = false

			continue
		}

		path, seen := "", map[string]bool{}

		for i := 0; i+1 < len(e.Content); i += 2 {
			name, v := e.Content[i].Value, deref(e.Content[i+1])
			if seen[name] {
				c.rep.add(c.where, "journals."+name, "listed twice")

				ok = false

				continue
			}

			seen[name] = true

			if name != "path" {
				refuseUndeclared(v, "journals."+name, c.where, c.rep)

				ok = false

				continue
			}

			if !isString(v) {
				c.rep.add(c.where, c.key, "each entry's path must be a string, got "+typeName(v))

				ok = false

				continue
			}

			path = v.Value
		}

		paths = append(paths, path)
	}

	if !ok {
		return nil, false
	}

	return s.check(paths, c)
}

// fromEnv reads and checks the variable's value of the key. No refusal of a
// variable prints its value, which may be a secret (RFC 13 §6).
func (s *keySpec) fromEnv(raw string, rep *report) (any, bool) {
	c := check{where: whereEnvironment, key: s.envName(), show: false, rep: rep}

	switch s.kind {
	case kindBool:
		switch raw {
		case "true":
			return true, true
		case "false":
			return false, true
		}

		rep.add(c.where, c.key, "must be true or false")

		return nil, false
	case kindCount:
		i, err := strconv.Atoi(raw)
		if err != nil {
			rep.add(c.where, c.key, "must be an integer")

			return nil, false
		}

		return i, c.count(i)
	case kindEnumList, kindListenList, kindIPList, kindPathList, kindStringList, kindJournals:
		return s.check(strings.Split(raw, ","), c)
	}

	return s.check(raw, c)
}

// check is where a refusal goes, and whether it may print the value: a
// value from the file of a key that holds no secret.
type check struct {
	where, key string
	show       bool
	rep        *report
}

func (c check) refuse(rule string) bool {
	c.rep.add(c.where, c.key, rule)

	return false
}

func (c check) typeRefusal(s *keySpec, n *yaml.Node) (any, bool) {
	return nil, c.refuse("must be " + s.form() + ", got " + typeName(n))
}

// got appends the refused value when the refusal may print it.
func (c check) got(rule, value string) string {
	if c.printable(value) {
		return rule + ", got " + value
	}

	return rule
}

// printable reports whether a refusal may print value: a value from the
// file of a key that holds no secret, not empty, and on one line of
// printable characters, so it cannot break the report's one line per
// refusal.
func (c check) printable(value string) bool {
	if !c.show || value == "" {
		return false
	}

	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}

	return true
}

func (c check) count(i int) bool {
	if i < 0 {
		return c.refuse(c.got("must be 0 or more", strconv.Itoa(i)))
	}

	return true
}

// check applies the key's own rule to a value read as a string or a list
// of strings, and returns the value in its field's type.
func (s *keySpec) check(v any, c check) (any, bool) {
	switch s.kind {
	case kindString, kindLegacyPath:
		return v, true
	case kindEnum:
		return v, c.enum(s, v.(string))
	case kindPath:
		return v, c.path(v.(string))
	case kindOutput:
		if out := v.(string); out == "stdout" || out == "stderr" {
			return v, true
		}

		return v, c.path(v.(string))
	case kindSocket:
		p := v.(string)
		if !c.path(p) {
			return v, false
		}

		if len(p) > socketLimit {
			return v, c.refuse(c.got(
				"longer than "+strconv.Itoa(socketLimit)+" bytes, the socket address limit", p))
		}

		return v, true
	case kindUUID:
		id, err := uuid.Parse(v.(string))
		if err != nil {
			return nil, c.refuse("must be a UUID")
		}

		return entity.NodeID(id), true
	case kindListenOff:
		if v.(string) == "off" {
			return v, true
		}

		return v, c.listen(v.(string))
	case kindDuration:
		d, err := configunits.ParseDuration(v.(string))
		if err != nil {
			return nil, c.formRefusal(s)
		}

		return d, true
	case kindDays:
		d, err := configunits.ParseDuration(v.(string))
		if err != nil {
			return nil, c.formRefusal(s)
		}

		if d%(24*time.Hour) != 0 {
			return nil, c.refuse(c.got("must be a whole number of days", v.(string)))
		}

		return int(d / (24 * time.Hour)), true
	case kindSizeMiB:
		n, err := configunits.ParseFileSize(v.(string))
		if err != nil {
			return nil, c.formRefusal(s)
		}

		if n < mib || n%mib != 0 {
			return nil, c.refuse(c.got("must be a whole number of MiB, at least 1MiB", v.(string)))
		}

		return int(n / mib), true
	}

	return s.checkList(v.([]string), c)
}

// formRefusal refuses a string that is not in the key's form, naming the
// type: the value from the file, as a decoder's would be, is not printed.
func (c check) formRefusal(s *keySpec) bool {
	if c.where == whereEnvironment {
		return c.refuse("must be " + s.form())
	}

	return c.refuse("must be " + s.form() + ", got a string")
}

// checkList applies a list key's rule to each entry.
func (s *keySpec) checkList(list []string, c check) (any, bool) {
	ok := true

	for _, e := range list {
		switch s.kind {
		case kindEnumList:
			ok = c.enum(s, e) && ok
		case kindListenList:
			ok = c.listen(e) && ok
		case kindIPList:
			if net.ParseIP(e) == nil {
				ok = c.refuse(c.got2(e, " is not an IP literal", "not an IP literal")) && ok
			}
		case kindPathList:
			ok = c.path(e) && ok
		case kindJournals:
			if e == "" {
				ok = c.refuse("an empty entry") && ok

				continue
			}

			ok = c.path(e) && ok
		}
	}

	if !ok {
		return nil, false
	}

	if s.kind == kindJournals {
		journals := make([]Journal, len(list))
		for i, p := range list {
			journals[i] = Journal{Path: p}
		}

		return journals, true
	}

	return list, true
}

// got2 is the rule with the value before it when it may be printed, else
// the rule alone.
func (c check) got2(value, withValue, alone string) string {
	if c.printable(value) {
		return value + withValue
	}

	return alone
}

func (c check) enum(s *keySpec, v string) bool {
	if slices.Contains(s.enum, v) {
		return true
	}

	var rule string
	if len(s.enum) == 2 {
		rule = "must be " + s.enum[0] + " or " + s.enum[1]
	} else {
		rule = "must be one of " + strings.Join(s.enum, ", ")
	}

	return c.refuse(c.got(rule, v))
}

// path refuses a path that is not absolute and clean: a path is never
// rewritten, since a journal's path is its identity in the store (RFC 13
// Appendix C).
func (c check) path(p string) bool {
	if !filepath.IsAbs(p) {
		return c.refuse(c.got2(p, " is not an absolute path", "not an absolute path"))
	}

	if clean := filepath.Clean(p); clean != p {
		return c.refuse(c.got2(p, " is not a clean absolute path; write "+clean,
			"not a clean absolute path"))
	}

	return true
}

// listen refuses a listen address that is not ip:port with an IP literal
// host and a port from 0 to 65535: an empty host is refused rather than
// read as every interface (RFC 13 Appendix C).
func (c check) listen(a string) bool {
	host, port, err := net.SplitHostPort(a)

	switch {
	case err != nil:
		return c.refuse(c.got2(a, " is not an ip:port address", "not an ip:port address"))
	case host == "":
		return c.refuse(c.got2(a,
			" has an empty host; write 0.0.0.0:"+port+" or [::]:"+port,
			"an address has an empty host"))
	case net.ParseIP(host) == nil:
		return c.refuse(
			c.got2(a, ": the host is not an IP literal", "the host is not an IP literal"),
		)
	}

	// decision: port 0 is accepted and binds a port the system picks, which
	// the start logs; the tests of dfs start bind their listeners so. Appendix
	// C writes the address as ip:port and bounds the port no further.
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return c.refuse(c.got2(a, ": the port must be between 0 and 65535",
			"the port must be between 0 and 65535"))
	}

	return true
}

// socketLimit is the longest socket path the operating system binds: its
// socket address's path field, less the terminating zero.
var socketLimit = len(syscall.RawSockaddrUnix{}.Path) - 1

// mib is one MiB in bytes.
const mib = 1 << 20

// settingKeys are the dotted keys of RFC 13 Appendix B's settings, the rows
// whose class is live, restart, next write or bound. "<protocol>" stands for
// any one adapter's name. A key under one of these is that setting's field.
var settingKeys = []string{
	"adapters.nfs.tls",
	"adapters.nfs.versions",
	"adapters.<protocol>.disabled_operations",
	"adapters.<protocol>.drain_timeout",
	"adapters.<protocol>.enabled",
	"adapters.<protocol>.handshake_timeout",
	"adapters.<protocol>.idle_timeout",
	"adapters.<protocol>.port",
	"adapters.<protocol>.read_timeout",
	"adapters.<protocol>.write_timeout",
	"adapters.smb.dialects",
	"adapters.smb.pending_timeout",
	"adapters.smb.recall_timeout",
	"api.oidc_role_map",
	"api.password_sign_in",
	"api.session_lifetime",
	"api.token_max_lifetime",
	"audit.retention",
	"backup.kind",
	"backups.copy_rate",
	"backups.max_copy_time",
	"backups.spill_max",
	"backup.verify_every",
	"events.max_bytes",
	"events.retention",
	"eviction.repack_threshold",
	"gc.audit.forward_period",
	"gc.audit.period",
	"gc.hold_threshold",
	"gc.interval",
	"gc.space_amp_target",
	"gc.trash_retention",
	"group_commit.max_files",
	"identity.directory",
	"identity.directory_stale_grace",
	"identity.id_range",
	"identity.keytab",
	"identity.netbios_name",
	"identity.ntlm",
	"identity.oidc",
	"journal.headroom_records",
	"journal.idle_seal_bytes",
	"journal.spare_target",
	"metrics.top_shares",
	"migration.freeze_timeout",
	"migration.hold_reply",
	"migration.reconnect_wait",
	"offload.max_age",
	"offload.retry_max",
	"offload.retry_max_pressure",
	"offload.retry_min",
	"readahead.max_blocks",
	"readahead.max_bytes",
	"rehome.rate",
	"remote.credential",
	"remote.endpoint",
	"removal.rescan",
	"shard.dwell",
	"shard.follow_writer",
	"shard.replace_delay",
	"shard.unreachable_delay",
	"shard.unreachable_handover_bound",
	"sidecars.portmap.port",
	"snapshots.cut_deadline",
	"snapshots.directory",
	"snapshots.gate_max",
	"snapshots.hold_bound",
	"snapshots.hold_journal_fraction",
	"snapshots.lock_max",
	"snapshots.reserve",
	"snapshots.reserve_fraction",
	"tracing.endpoint",
	"tracing.sample_ratio",
}

// isSetting reports whether key is a setting of Appendix B, or a key under
// one: a host's file never sets a setting (RFC 13 §2.2).
func isSetting(key string) bool {
	parts := strings.Split(key, ".")

	for _, setting := range settingKeys {
		want := strings.Split(setting, ".")
		if len(parts) < len(want) {
			continue
		}

		match := true

		for i, w := range want {
			if w != "<protocol>" && w != parts[i] {
				match = false

				break
			}
		}

		if match {
			return true
		}
	}

	return false
}
