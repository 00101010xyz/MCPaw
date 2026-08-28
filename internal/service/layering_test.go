package service

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoSourceSpecificImports guards the architectural rule that keeps this
// package safe to change for one indexable connector without having to
// check every other: internal/service may depend on the generic source
// registry (internal/index/source), but never on one source's own
// subpackage (internal/index/source/zotero and, later, its siblings), each
// of which carries knowledge of exactly one connector's tool names and
// response shapes. A file here importing one of those directly would mean a
// Zotero-specific change could break this package's build for every other
// connector too — exactly the coupling the Source seam exists to prevent.
// See internal/archtest for the equivalent whole-repo check on the storage
// adapter boundary.
func TestNoSourceSpecificImports(t *testing.T) {
	const (
		registryPath = "github.com/00101010xyz/mcpaw/internal/index/source"
		forbidPrefix = registryPath + "/"
	)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == registryPath {
				continue
			}
			if strings.HasPrefix(path, forbidPrefix) {
				t.Errorf("%s imports %s directly — internal/service must depend only on the "+
					"generic %s registry, never on one source's own subpackage",
					e.Name(), path, registryPath)
			}
		}
	}
}
