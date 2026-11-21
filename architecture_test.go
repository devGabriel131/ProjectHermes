// Architecture tests for the bounded contexts in docs/context-map.md. They need the go command but no
// database, so they run with the unit tests.
//
// TestContextRootsImportNoAdapters reads `go list -deps` for each reshaped context root, so it sees every
// package a root links, direct or transitive, and nothing about what the root does with it. SQL drivers
// other than pgx are not on the forbidden list. Transitive means a root may not import internal/logging
// today: logging imports net/http and GORM.
//
// TestContextsTouchOnlyOwnedTables type-checks each package's production files and applies three matchers:
//   - SQL: in every constant string expression (a literal, a constant, or a concatenation of them; a
//     constant is charged where it is declared and again where it is used), an upper-case FROM, JOIN,
//     UPDATE, INTO or USING followed by a lower-case name, with an optional alias, and each further name in
//     a comma list, is a table. After FROM or JOIN a name followed by "(" is a function, and a name declared
//     as "name AS (" in the same string is a CTE; both are skipped. IS DISTINCT FROM and EXTRACT(...) are
//     removed first.
//   - GORM calls: an argument whose type is a model from models(), or a pointer, slice or array of one, uses
//     that model's table when the call is made on a *gorm.DB or takes one as a parameter; so does a literal
//     Table("name").
//   - GORM associations: a model's type declaration uses the table of every model it has a relationship
//     with, because Preload, Joins and association auto-save read and write through that field.
//
// Not seen, so not enforced:
//   - SQL built at run time (fmt.Sprintf, += on a variable), lower-case keywords, quoted names, LOCK TABLE,
//     TRUNCATE, COPY, FROM ONLY, clause.From, Table(variable), and a model passed as any. Schema-qualified
//     names fail closed: public.students reads as the unknown table "public".
//   - A table inside a statement whose CTE has the same name.
//   - SQL outside Go. No package embeds .sql today; sqlc's generated Go holds each query as a constant,
//     which is scanned, so queries.sql must keep upper-case keywords.
//   - A second reach into the same table from an allowlisted declaration: entries are per declaration.
//
// Known false positives fail loudly as a table with no owner: prose with an upper-case keyword before a
// lower-case word ("UPDATE contract %d failed"), and a lower-case word after a keyword ("DO UPDATE set").
//
// SQL is charged to the package whose source holds it, so a caller reaching another context's table through
// that context's own helper or scope (links.Usable, links.Enroll) is not a reach; the seams replace those
// helpers with service methods. Test files are not checked: tests seed other contexts' tables on purpose.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// reshapedRoots are context root packages already in the Go shape of docs/context-map.md. Each reshape step
// adds its root here in the same PR.
var reshapedRoots = []string{}

// rootForbidden reports why a root may not depend on pkg, or "" when it may. sqlc output is any
// internal/*/*db package of this module. database/sql/driver stays allowed for Valuer and Scanner types.
func rootForbidden(module, pkg string) string {
	switch {
	case pkg == "net/http", pkg == "html/template":
		return pkg + " belongs in <context>web"
	case strings.HasPrefix(pkg, "github.com/a-h/templ"):
		return "templ belongs in <context>web"
	case pkg == "database/sql", strings.HasPrefix(pkg, "github.com/jackc/pgx"), strings.HasPrefix(pkg, "gorm.io/"):
		return "database access belongs in <context>db"
	case strings.HasPrefix(pkg, module+"/") && sqlcOutput.MatchString(strings.TrimPrefix(pkg, module+"/")):
		return "sqlc output belongs in <context>db"
	}
	return ""
}

var sqlcOutput = regexp.MustCompile(`^internal/[a-z]+/[a-z]+db$`)

// contextOf maps today's packages to the context that owns them. Packages missing here own no table.
var contextOf = map[string]string{
	"internal/auth":                "identity",
	"internal/students":            "students",
	"internal/links":               "students",
	"internal/customer":            "students",
	"internal/settings":            "students",
	"internal/billing":             "billing",
	"internal/billing/billingtest": "billing",
	"internal/payments":            "billing",
}

// tableOwners is the table-ownership map of docs/context-map.md.
var tableOwners = map[string]string{
	"users": "identity", "sessions": "identity", "student_logins": "identity",
	"students": "students", "enrollments": "students", "links": "students", "settings": "students",
	"billing_contracts": "billing", "billing_obligations": "billing", "billing_checkout_attempts": "billing",
	"billing_payments": "billing", "billing_suspensions": "billing", "billing_webhook_events": "billing",
	"attempts": "learning", "responses": "learning", "word_cards": "learning", "word_card_days": "learning",
	"student_employers": "documents", "ist_attempts": "documents",
}

// reach is one declaration's use of a table: a function or method name, or the first name of a
// package-level var, const or type.
type reach struct{ file, decl, table string }

// crossContextAllowlist holds today's Billing → Students reaches. Phase F moves each behind a Students seam
// (Standing, Enrollment lock & enroll) and deletes its line; F9 leaves the list empty. Never add to it.
var crossContextAllowlist = map[reach]bool{
	{"internal/billing/models.go", "Contract", "enrollments"}:              true, // → FK only, association field removed
	{"internal/billing/billing.go", "contractFor", "students"}:             true, // → Profile
	{"internal/billing/cancel.go", "CancelEnrollment", "enrollments"}:      true, // → CancelEnrollment
	{"internal/billing/checkout.go", "beginCheckout", "enrollments"}:       true, // → LockEnrollment
	{"internal/billing/contact.go", "UpdateContact", "enrollments"}:        true, // → LockEnrollment, UpdateContact
	{"internal/billing/summary.go", "ForStudent", "students"}:              true, // → Profile
	{"internal/billing/summary.go", "linkToken", "enrollments"}:            true, // → LinkToken
	{"internal/billing/summary.go", "linkToken", "links"}:                  true, // → LinkToken
	{"internal/billing/suspension.go", "evaluateDeadlines", "students"}:    true, // → SetStanding
	{"internal/billing/suspension.go", "evaluateReactivation", "students"}: true, // → SetStanding
	{"internal/billing/withdraw.go", "withdraw", "students"}:               true, // → SetStanding
	{"internal/billing/worker.go", "leased", "enrollments"}:                true, // → LockEnrollment
	{"internal/billing/worker.go", "leasedStudent", "students"}:            true, // → a Students row lock, third in lock order
	{"internal/billing/worker.go", "syncCustomerContact", "students"}:      true, // → Profile
}

func TestContextRootsImportNoAdapters(t *testing.T) {
	if len(reshapedRoots) == 0 {
		t.Skip("no context root is reshaped yet; each reshape adds its root to reshapedRoots, which must then " +
			"depend on no net/http, html/template, templ, database/sql, pgx, GORM or <context>db package")
	}
	module := goModule(t)
	for _, root := range reshapedRoots {
		for _, p := range goList(t, "-deps", "./"+root) {
			if why := rootForbidden(module, p.ImportPath); why != "" {
				t.Errorf("%s depends on %s: %s", root, p.ImportPath, why)
			}
		}
	}
}

func TestContextsTouchOnlyOwnedTables(t *testing.T) {
	module := goModule(t)
	schemas := modelSchemas(t)
	exports := map[string]string{}
	var own []goPackage
	for _, p := range goList(t, "-deps", "-export", "./...") {
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Path == module {
			own = append(own, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})

	seen := map[reach]bool{}
	for _, p := range own {
		rel := strings.TrimPrefix(strings.TrimPrefix(p.ImportPath, module), "/")
		ctx := contextOf[rel]
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(rel, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range files {
			file := filepath.ToSlash(fset.File(f.Pos()).Name())
			forEachDecl(f, func(decl string, n ast.Node) {
				for _, use := range tableUses(n, p.ImportPath, info, schemas) {
					r := reach{file, decl, use.table}
					owner, known := tableOwners[use.table]
					switch {
					case !known:
						t.Errorf("%s (%s): table %q has no owner; add it to the table-ownership map in docs/context-map.md and tableOwners",
							fset.Position(use.pos), decl, use.table)
					case owner == ctx:
					case crossContextAllowlist[r]:
						seen[r] = true
					case ctx == "":
						t.Errorf("%s (%s): package %s belongs to no context and owns no table, but touches %s, owned by %s",
							fset.Position(use.pos), decl, rel, use.table, owner)
					default:
						t.Errorf("%s (%s): %s touches table %s, owned by %s; go through a %s seam (docs/context-map.md)",
							fset.Position(use.pos), decl, ctx, use.table, owner, owner)
					}
				}
			})
		}
	}
	for r := range crossContextAllowlist {
		if !seen[r] {
			t.Errorf("crossContextAllowlist entry %v no longer matches; delete it", r)
		}
	}
}

type tableUse struct {
	pos   token.Pos
	table string
}

const tableRef = `[a-z_]\w*(?:\s+(?:AS\s+)?[a-z_]\w*)?` // a name and its optional alias

var (
	sqlTables   = regexp.MustCompile(`\b(FROM|JOIN|UPDATE|INTO|USING)\s+(` + tableRef + `(?:\s*,\s*` + tableRef + `)*)(\s*\()?`)
	cteName     = regexp.MustCompile(`\b([a-z_]\w*)\s+AS\s*\(`)
	notTableSQL = regexp.MustCompile(`\bIS\s+(?:NOT\s+)?DISTINCT\s+FROM\b|\bEXTRACT\s*\([^)]*\)`)
)

// tableUses applies the three matchers to one declaration of package pkg.
func tableUses(n ast.Node, pkg string, info *types.Info, schemas map[string]model) []tableUse {
	var uses []tableUse
	if ts, ok := n.(*ast.TypeSpec); ok {
		for _, table := range schemas[pkg+"."+ts.Name.Name].related {
			uses = append(uses, tableUse{ts.Pos(), table})
		}
	}
	ast.Inspect(n, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok {
			if tv := info.Types[e]; tv.Value != nil && tv.Value.Kind() == constant.String {
				for _, table := range sqlTableNames(constant.StringVal(tv.Value)) {
					uses = append(uses, tableUse{e.Pos(), table})
				}
				return false
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || !isGORMCall(call, info) {
			return true
		}
		for _, arg := range call.Args {
			if m, ok := schemas[modelKey(info.TypeOf(arg))]; ok {
				uses = append(uses, tableUse{arg.Pos(), m.table})
			}
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Table" && len(call.Args) > 0 {
			if tv := info.Types[call.Args[0]]; tv.Value != nil && tv.Value.Kind() == constant.String {
				if name := strings.Fields(constant.StringVal(tv.Value)); len(name) > 0 {
					uses = append(uses, tableUse{call.Args[0].Pos(), name[0]})
				}
			}
		}
		return true
	})
	return uses
}

func sqlTableNames(sql string) []string {
	sql = notTableSQL.ReplaceAllString(sql, " ")
	ctes := map[string]bool{}
	for _, m := range cteName.FindAllStringSubmatch(sql, -1) {
		ctes[m[1]] = true
	}
	var tables []string
	for _, m := range sqlTables.FindAllStringSubmatch(sql, -1) {
		items := strings.Split(m[2], ",")
		// A set-returning function follows FROM or JOIN; after INTO the "(" opens a column list.
		if m[3] != "" && (m[1] == "FROM" || m[1] == "JOIN") {
			items = items[:len(items)-1]
		}
		for _, item := range items {
			if name := strings.Fields(item)[0]; !ctes[name] {
				tables = append(tables, name)
			}
		}
	}
	return tables
}

func TestSQLTableNames(t *testing.T) {
	for sql, want := range map[string]string{
		"INSERT INTO students (name) VALUES (?)":                               "students",
		"INSERT INTO students(name) VALUES (?)":                                "students",
		"UPDATE enrollments e SET pin = ? FROM links l, async WHERE e.x":       "enrollments links async",
		"SELECT 1 FROM billing_contracts AS c, students s":                     "billing_contracts students",
		"DELETE FROM billing_payments p USING students s":                      "billing_payments students",
		"SELECT * FROM unnest(?) JOIN links USING (id)":                        "links",
		"WITH slot AS (UPDATE links SET n = 1) SELECT id FROM slot":            "links",
		"SELECT a IS NOT DISTINCT FROM b, EXTRACT(YEAR FROM created_at)":       "",
		"SELECT 1 FROM enrollments WHERE id = ? FOR UPDATE SKIP LOCKED":        "enrollments",
		"JOIN LATERAL (SELECT state FROM billing_checkout_attempts) a ON true": "billing_checkout_attempts",
	} {
		if got := strings.Join(sqlTableNames(sql), " "); got != want {
			t.Errorf("sqlTableNames(%q) = %q, want %q", sql, got, want)
		}
	}
}

func isGORMCall(call *ast.CallExpr, info *types.Info) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isGORMDB(info.TypeOf(sel.X)) {
		return true
	}
	sig, ok := info.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return false
	}
	for i := range sig.Params().Len() {
		if isGORMDB(sig.Params().At(i).Type()) {
			return true
		}
	}
	return false
}

func isGORMDB(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "gorm.io/gorm" && named.Obj().Name() == "DB"
}

// modelKey names the type behind pointers, slices and arrays as "import/path.Type".
func modelKey(t types.Type) string {
	for {
		switch u := t.(type) {
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Named:
			if u.Obj().Pkg() == nil {
				return ""
			}
			return u.Obj().Pkg().Path() + "." + u.Obj().Name()
		default:
			return ""
		}
	}
}

type model struct {
	table   string
	related []string // tables of the models its fields associate with
}

// modelSchemas maps each AutoMigrate model to its table and related tables, as GORM names them.
func modelSchemas(t *testing.T) map[string]model {
	schemas := map[string]model{}
	cache := &sync.Map{}
	for _, m := range models() {
		s, err := schema.Parse(m, cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		var related []string
		for _, rel := range s.Relationships.Relations {
			related = append(related, rel.FieldSchema.Table)
		}
		typ := reflect.TypeOf(m).Elem()
		schemas[typ.PkgPath()+"."+typ.Name()] = model{s.Table, related}
	}
	return schemas
}

// forEachDecl calls fn with each top-level declaration and the name a reach records for it.
func forEachDecl(f *ast.File, fn func(decl string, n ast.Node)) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			fn(d.Name.Name, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					fn(s.Names[0].Name, s)
				case *ast.TypeSpec:
					fn(s.Name.Name, s)
				}
			}
		}
	}
}

type goPackage struct {
	ImportPath, Export string
	GoFiles            []string
	Module             *struct{ Path string }
}

func goList(t *testing.T, args ...string) []goPackage {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list", "-json=ImportPath,Export,GoFiles,Module"}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list %v: %v\n%s", args, err, exit.Stderr)
		}
		t.Fatalf("go list %v: %v", args, err)
	}
	var pkgs []goPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var p goPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
}

func goModule(t *testing.T) string {
	return goList(t, ".")[0].Module.Path
}
