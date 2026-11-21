package api

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// covered are the packages whose every declared error, or code, the problem
// table names: the directory, the name problems.go imports it as, and the
// prefix of what it declares.
var covered = []struct{ dir, alias, prefix string }{
	{"../../../pkg/node/apiv1", "apiv1", "Code"},
	{"../../../pkg/metadata/entity", "entity", "Err"},
	{"../../../pkg/vfs/vfserr", "vfserr", "Err"},
	{"../../../pkg/metadata/errors", "merrs", "Err"},
	{"../../../pkg/metadata/kv", "kv", "Err"},
	{"../../../pkg/settings", "settings", "Err"},
	{"../../../pkg/secrets/secreterr", "secreterr", "Err"},
}

// answeredAs are the errors whose row answers another code than their own
// name, each with the code it answers; problems.go's decision: at the row
// says why.
var answeredAs = map[string]string{
	"vfserr.ErrLogonFailure": "unauthenticated",
	"settings.ErrBound":      "bound_setting",
	"entity.ErrLocalName":    "invalid",
}

// Every error and code the covered packages declare, read from their source,
// has a row in problems.go, and that row answers the error's or the code's
// own name as its code, or the one answeredAs names, written as a literal or
// as an apiv1 constant: a new error with no row, or a row answering another
// name, fails here.
func TestProblemTableNamesEveryDeclaredError(t *testing.T) {
	named, codes := tableRows(t)
	for _, c := range covered {
		for _, name := range declared(t, c.dir, c.prefix) {
			sel := c.alias + "." + name
			want := snake(strings.TrimPrefix(name, c.prefix))
			if code, ok := answeredAs[sel]; ok {
				want = code
			}
			if !named[sel] {
				t.Errorf("%s has no row in the problem table", sel)
			} else if code := codes[sel]; code != want {
				t.Errorf("%s answers code %q, want %q", sel, code, want)
			}
		}
	}
}

// Every value vfs.Errors lists, the typed errors of every kind among them,
// and every metadata store code, answers a named row; an error nothing names
// answers 500, reported unnamed so the caller counts it.
func TestProblemTableIsTotal(t *testing.T) {
	errs := vfs.Errors()
	for c := merrs.ErrorCode(1); !strings.HasPrefix(c.String(), "Unknown"); c++ {
		errs = append(errs, &merrs.StoreError{Code: c, Message: "m"})
	}
	if len(errs) < 40 {
		t.Fatalf("collected %d errors, want vfs's and the store's codes", len(errs))
	}
	for _, err := range errs {
		if p, ok := problemOf(fmt.Errorf("wrapped: %w", err)); !ok || p.Code == "" {
			t.Errorf("%v: answered %d %q, named %v; want a row", err, p.Status, p.Code, ok)
		}
	}
	if p, ok := problemOf(errors.New("no row names this")); ok || p.Status != http.StatusInternalServerError || p.Code != "" {
		t.Errorf("an unnamed error: answered %d %q, named %v; want 500 with no code, unnamed", p.Status, p.Code, ok)
	}
}

// A component's error reaches the body by its name, wrapped or typed: a
// share being removed answers 409 stale, a store code its name, and a typed
// vfs error the sentinel it is. A store error's cause answers before its
// code, and an ended request 503 unavailable, whatever else it carries.
func TestProblemCodeIsTheComponentsName(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("put grant: %w", entity.ErrStale), http.StatusConflict, "stale"},
		{entity.ErrGenerationMismatch, http.StatusPreconditionFailed, "generation_mismatch"},
		{merrs.NewInvalidArgumentError("a netgroup needs a name"), http.StatusBadRequest, "invalid_argument"},
		{&vfs.AccessError{Cause: vfs.AccessReadOnly}, http.StatusForbidden, "access"},
		{&vfs.Delay{Cause: vfs.DelayFrozen}, http.StatusConflict, "delay"},
		{&fieldError{field: "spec.name", detail: "unknown field"}, http.StatusBadRequest, "invalid"},
		{&refusal{code: "password_change_required"}, http.StatusForbidden, "password_change_required"},
		{entity.ErrMoving, http.StatusConflict, "moving"},
		{context.Canceled, http.StatusServiceUnavailable, "unavailable"},
		{fmt.Errorf("list: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "unavailable"},
		{&merrs.StoreError{Code: merrs.ErrIOError, Message: "read", Cause: context.DeadlineExceeded}, http.StatusServiceUnavailable, "unavailable"},
		{errors.Join(context.Canceled, &merrs.StoreError{Code: merrs.ErrNotFound}), http.StatusServiceUnavailable, "unavailable"},
		{&merrs.StoreError{Code: merrs.ErrIOError, Message: "read", Cause: errors.New("disk")}, http.StatusInternalServerError, "io_error"},
		{kv.ErrConflict, http.StatusConflict, "conflict"},
		{errors.Join(context.DeadlineExceeded, kv.ErrConflict), http.StatusConflict, "conflict"},
		{kv.ErrClosed, http.StatusServiceUnavailable, "closed"},
		{fmt.Errorf("sign in: %w", vfserr.ErrLogonFailure), http.StatusUnauthorized, "unauthenticated"},
		{fmt.Errorf("x: %w", vfserr.ErrUnknownPrincipal), http.StatusBadRequest, "unknown_principal"},
		{fmt.Errorf("delete setting: %w", errors.ErrUnsupported), http.StatusServiceUnavailable, "unavailable"},
	} {
		p, _ := problemOf(tc.err)
		if p.Status != tc.status || string(p.Code) != tc.code {
			t.Errorf("%v: answered %d %q, want %d %q", tc.err, p.Status, p.Code, tc.status, tc.code)
		}
	}
	if p, _ := problemOf(&fieldError{field: "spec.name", detail: "unknown field"}); p.Field != "spec.name" {
		t.Errorf("a refused field: problem's field %q, want spec.name", p.Field)
	}
}

// Only a refusal's or a refused field's text reaches the detail: a mapped
// component error's text, and an unnamed error's, which may carry anything,
// stay out of the body, an unnamed one answering "internal error".
func TestProblemDetail(t *testing.T) {
	const secret = "s3cr3t-7a1f"
	for _, tc := range []struct {
		err    error
		detail string
	}{
		{errors.New(secret), "internal error"},
		{fmt.Errorf("open %s: %w", secret, vfserr.ErrNotFound), ""},
		{merrs.NewInvalidArgumentError(secret), ""},
		{&refusal{code: apiv1.CodeForbidden, detail: "no"}, "no"},
		{fmt.Errorf("%s: %w", secret, &fieldError{field: "name", detail: "unknown field"}), "unknown field"},
	} {
		p, _ := problemOf(tc.err)
		if p.Detail != tc.detail || strings.Contains(fmt.Sprint(p), secret) {
			t.Errorf("%v: problem %+v, want detail %q and no %q", tc.err, p, tc.detail, secret)
		}
	}
}

// Every row answers a status in 400–599, each code one status and one
// retryable flag across both tables, and the flag is RFC 23's: a 429 or a
// 503 may succeed later unchanged, a refused value, a missing or refused
// caller, a failed precondition and a server fault may not; a 409 may be
// either, by the state that refuses it.
func TestProblemStatuses(t *testing.T) {
	byCode := map[apiv1.Code]answerOf{}
	check := func(name string, a answerOf) {
		if a.status < 400 || a.status > 599 {
			t.Errorf("%s: status %d, want 400–599", name, a.status)
		}
		switch a.status {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if !a.retryable {
				t.Errorf("%s: %d not retryable, want retryable", name, a.status)
			}
		case http.StatusConflict:
		default:
			if a.retryable {
				t.Errorf("%s: %d retryable, want not", name, a.status)
			}
		}
		if b, ok := byCode[a.code]; ok && (b.status != a.status || b.retryable != a.retryable) {
			t.Errorf("%s: code %q answers %d retryable %v, another row %d retryable %v", name, a.code, a.status, a.retryable, b.status, b.retryable)
		}
		byCode[a.code] = a
	}
	for code, a := range own {
		check(string(code), a)
	}
	for _, c := range components {
		if c.err == errors.ErrUnsupported {
			// The one unavailable answer that is final: no retry reaches a
			// body this build does not hold.
			if w := (answerOf{http.StatusServiceUnavailable, apiv1.CodeUnavailable, false}); c.answerOf != w {
				t.Errorf("%v: answers %+v, want %+v", c.err, c.answerOf, w)
			}
			continue
		}
		check(c.err.Error(), c.answerOf)
	}
}

// Every code answers the status and the retryable flag RFC 23 gives its
// condition, written here apart from the table: a 409 is retryable only
// where the state refusing it passes by itself, a lock, a move, a re-home, a
// running operation, a delay or a conflicting transaction.
func TestProblemAnswers(t *testing.T) {
	want := map[answerOf][]apiv1.Code{
		{status: 400}: {"invalid", "invalid_name", "name_too_long", "syntax", "invalid_file", "wrong_share", "not_dir", "is_dir",
			"past_end", "is_directory", "not_directory", "invalid_argument", "not_supported", "invalid_handle", "cross_share", "too_large",
			"unknown_principal", "unknown_key", "wrong_scope", "required", "secret_ref"},
		{status: 401}: {"unauthenticated"},
		{status: 403}: {"forbidden", "password_change_required", "access", "perm", "wrong_security", "access_denied", "auth_required",
			"permission_denied", "privilege_required"},
		{status: 404}: {"not_found"},
		{status: 409}: {"managed", "not_cancellable", "last_admin", "not_a_copy", "joined", "stale", "cross_project", "tags_exhausted", "exist",
			"not_empty", "read_only", "no_space", "quota", "range_not_locked", "delete_pending", "state", "cross", "incomplete",
			"already_exists", "quota_exceeded", "stale_handle", "lock_not_found", "newer_schema", "bound_setting", "format_version",
			"unproven", "wrong_kind", "kind_changed"},
		{status: 409, retryable: true}: {"operation_running", "delay", "moving", "rehoming", "locked", "share_violation", "blocked",
			"deadlock", "lock_limit_exceeded", "lock_conflict", "conflict"},
		{status: 412}:                  {"generation_mismatch"},
		{status: 422}:                  {"idempotency_mismatch"},
		{status: 428}:                  {"precondition_required"},
		{status: 429, retryable: true}: {"throttled"},
		{status: 500}:                  {"inconsistent", "bad_row", "lost", "corrupt", "io", "file_open", "io_error", "uncommitted_version", "wrong_role"},
		{status: 503, retryable: true}: {"starting", "unavailable", "grace", "grace_period", "connection_limit_reached", "closed"},
	}
	byCode := map[apiv1.Code]answerOf{}
	for a, codes := range want {
		for _, c := range codes {
			a.code = c
			byCode[c] = a
		}
	}
	seen := map[apiv1.Code]bool{}
	check := func(name string, a answerOf) {
		seen[a.code] = true
		if w, ok := byCode[a.code]; !ok {
			t.Errorf("%s: code %q is not in this test's list", name, a.code)
		} else if a != w {
			t.Errorf("%s: code %q answers %d retryable %v, want %d retryable %v", name, a.code, a.status, a.retryable, w.status, w.retryable)
		}
	}
	for code, a := range own {
		check(string(code), a)
	}
	for _, c := range components {
		if c.err == errors.ErrUnsupported {
			// The one unavailable answer that is final: no retry reaches a
			// body this build does not hold.
			if w := (answerOf{http.StatusServiceUnavailable, apiv1.CodeUnavailable, false}); c.answerOf != w {
				t.Errorf("%v: answers %+v, want %+v", c.err, c.answerOf, w)
			}
			continue
		}
		check(c.err.Error(), c.answerOf)
	}
	for c := range byCode {
		if !seen[c] {
			t.Errorf("code %q is in this test's list, but no row answers it", c)
		}
	}
}

// declared returns the exported names starting with prefix that the
// package's non-test files declare as a variable or a constant.
func declared(t *testing.T, dir, prefix string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR && g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					if n.IsExported() && strings.HasPrefix(n.Name, prefix) {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s declares nothing starting %s", dir, prefix)
	}
	slices.Sort(names)
	return names
}

// tableRows reads problems.go and returns each pkg.Name its rows and keys
// name, and the code each of those rows answers, a literal or an apiv1
// constant resolved to its value; a row whose code is neither fails the test.
func tableRows(t *testing.T) (named map[string]bool, codes map[string]string) {
	t.Helper()
	src, err := os.ReadFile("problems.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "problems.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := constants(t, covered[0].dir)
	named, codes = map[string]bool{}, map[string]string{}
	selector := func(e ast.Expr) string {
		if c, ok := e.(*ast.CallExpr); ok && len(c.Args) == 1 {
			e = c.Args[0]
		}
		if s, ok := e.(*ast.SelectorExpr); ok {
			if x, ok := s.X.(*ast.Ident); ok {
				return x.Name + "." + s.Sel.Name
			}
		}
		return ""
	}
	// row records the code of a row naming s, its answer a.
	row := func(s string, a ast.Expr) {
		lit, ok := a.(*ast.CompositeLit)
		if s == "" || !ok || len(lit.Elts) != 3 {
			return
		}
		named[s] = true
		switch e := lit.Elts[1].(type) {
		case *ast.BasicLit:
			codes[s], _ = strconv.Unquote(e.Value)
		case *ast.SelectorExpr:
			if v, ok := consts[selector(e)]; ok {
				codes[s] = v
				return
			}
			t.Errorf("%s: code %s is no apiv1 constant", s, selector(e))
		default:
			t.Errorf("%s: code is neither a literal nor an apiv1 constant", s)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if s := selector(n.Key); s != "" {
				named[s] = true
				row(s, n.Value)
			}
		case *ast.CompositeLit:
			if len(n.Elts) == 2 {
				row(selector(n.Elts[0]), n.Elts[1])
			}
		}
		return true
	})
	return named, codes
}

// constants returns each string constant the package in dir declares, by
// apiv1.Name, read from its source.
func constants(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, sp := range g.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								out["apiv1."+n.Name], _ = strconv.Unquote(lit.Value)
							}
						}
					}
				}
			}
		}
	}
	return out
}

// snake is a Go name in snake case, an acronym kept as one word: NotFound is
// not_found, IOError io_error.
func snake(name string) string {
	r := []rune(name)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && unicode.IsUpper(c) && (unicode.IsLower(r[i-1]) || i+1 < len(r) && unicode.IsLower(r[i+1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}
