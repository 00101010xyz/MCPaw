// Package archtest holds whole-repository layering guards — invariants from
// docs/ARCHITECTURE.md §3.1 that no single package's own tests are
// positioned to check, because the risk is one package reaching into
// another it shouldn't, not a defect within one package's own code.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot locates the directory containing go.mod, walking upward from
// the test's working directory — a package directory under the module, per
// the standard `go test` convention — so this works regardless of where
// `go test ./...` is invoked from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test's working directory")
		}
		dir = parent
	}
}

const sqlitestoreImportPath = "github.com/00101010xyz/mcpaw/internal/store/sqlitestore"

// TestSqliteAdapterOnlyImportedFromCompositionRootOrTests guards
// docs/ARCHITECTURE.md §3.1's layering table: the SQLite adapter is
// "concrete I/O, swappable" and cmd/mcpaw is "the only place that
// constructs concrete types". A production (non-test) file anywhere else
// importing sqlitestore directly would mean swapping storage backends is a
// grep-and-pray exercise instead of a one-file change at the composition
// root — and would mean some application-layer package secretly depends on
// a concrete adapter instead of the store.Repositories interfaces it is
// supposed to be tested against with a fake.
//
// _test.go files are exempt: internal/service, internal/webui and
// internal/httpapi all deliberately test against a real temp-file SQLite
// store rather than mocking every repository port (see their test harness
// comments), which is a considered choice, not the coupling this guards
// against.
func TestSqliteAdapterOnlyImportedFromCompositionRootOrTests(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/store/sqlitestore/") || strings.HasPrefix(rel, "cmd/mcpaw/") {
			return nil // the adapter's own package, and the composition root, are exempt.
		}

		f, ferr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if ferr != nil {
			return ferr
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == sqlitestoreImportPath {
				t.Errorf("%s imports internal/store/sqlitestore directly — only cmd/mcpaw (the "+
					"composition root) and _test.go files may construct the concrete SQLite adapter; "+
					"everything else must depend on the store.Repositories interfaces", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module: %v", err)
	}
}
