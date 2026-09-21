package testkit

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var sqliteImplementationImports = map[string]struct{}{
	"database/sql":                    {},
	"github.com/arcmantle/go-sqlite3": {},
}

func CheckStorageAdapterBoundary(root string) error {
	violations := make([]string, 0)
	fileSet := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}

		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("make boundary path relative: %w", err)
		}
		relativePath = filepath.ToSlash(relativePath)
		if strings.HasPrefix(relativePath, "storage/sqlite/") {
			return nil
		}

		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relativePath, err)
		}
		for _, importSpec := range file.Imports {
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				return fmt.Errorf("parse import in %s: %w", relativePath, err)
			}
			if _, prohibited := sqliteImplementationImports[importPath]; prohibited {
				violations = append(violations, fmt.Sprintf("%s imports %s", relativePath, importPath))
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check storage adapter boundary: %w", err)
	}
	if len(violations) == 0 {
		return nil
	}
	sort.Strings(violations)
	return fmt.Errorf("SQLite implementation details outside storage/sqlite: %s", strings.Join(violations, "; "))
}
