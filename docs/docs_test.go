package docs_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var repoRoot string

func init() {
	// Walk up from this test file to find go.mod (repo root)
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			repoRoot = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("cannot find repo root (go.mod)")
		}
		dir = parent
	}
}

// TestDocReferencesInCode checks that every docs/ path referenced in Go source
// and CLAUDE.md actually points to an existing file.
func TestDocReferencesInCode(t *testing.T) {
	docRefPattern := regexp.MustCompile(`docs/[\w.-]+\.md`)

	// Scan Go files
	err := filepath.Walk(filepath.Join(repoRoot, "pkg"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "pb.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range docRefPattern.FindAllString(string(data), -1) {
			target := filepath.Join(repoRoot, match)
			if _, err := os.Stat(target); os.IsNotExist(err) {
				rel, _ := filepath.Rel(repoRoot, path)
				t.Errorf("%s references %s but file does not exist", rel, match)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Scan CLAUDE.md
	claudeMD, err := os.ReadFile(filepath.Join(repoRoot, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range docRefPattern.FindAllString(string(claudeMD), -1) {
		target := filepath.Join(repoRoot, match)
		if _, err := os.Stat(target); os.IsNotExist(err) {
			t.Errorf("CLAUDE.md references %s but file does not exist", match)
		}
	}
}

// TestDocNumberingSequential checks that numbered docs (01-xxx.md, 02-xxx.md, ...)
// have sequential numbering with no gaps.
func TestDocNumberingSequential(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatal(err)
	}

	numPattern := regexp.MustCompile(`^(\d+)-`)
	var numbers []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := numPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		numbers = append(numbers, n)
	}

	sort.Ints(numbers)
	for i, n := range numbers {
		expected := i + 1
		if n != expected {
			t.Errorf("numbering gap: expected %02d but found %02d (sequence: %v)", expected, n, numbers)
			break
		}
	}
}

// TestDocHeadingsAreEnglish checks that the first heading (# Title) in each
// numbered doc contains no CJK characters.
func TestDocHeadingsAreEnglish(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatal(err)
	}

	headingPattern := regexp.MustCompile(`^#\s+(.+)`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// Only check numbered docs
		if len(e.Name()) < 3 || e.Name()[2] != '-' {
			continue
		}
		data, err := os.ReadFile(filepath.Join(docsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			m := headingPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if containsCJK(m[1]) {
				t.Errorf("%s: heading contains CJK characters: %q", e.Name(), m[1])
			}
			break
		}
	}
}

// TestDocNoDuplicateNumbers checks that no two docs share the same number prefix.
func TestDocNoDuplicateNumbers(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatal(err)
	}

	numPattern := regexp.MustCompile(`^(\d+)-`)
	seen := make(map[string]string) // number → filename
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := numPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if prev, ok := seen[m[1]]; ok {
			t.Errorf("duplicate number prefix %s: %s and %s", m[1], prev, e.Name())
		}
		seen[m[1]] = e.Name()
	}
}

// TestDocCrossReferences checks that docs/ internal links (e.g., [text](05-owner-id.md))
// point to files that exist within the docs directory.
func TestDocCrossReferences(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatal(err)
	}

	linkPattern := regexp.MustCompile(`\[([^\]]*)\]\(([^)]+\.md)\)`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(docsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range linkPattern.FindAllStringSubmatch(string(data), -1) {
			ref := match[2]
			if strings.HasPrefix(ref, "http") {
				continue
			}
			target := filepath.Join(docsDir, ref)
			if _, err := os.Stat(target); os.IsNotExist(err) {
				t.Errorf("%s: broken link to %s", e.Name(), ref)
			}
		}
	}
}

// TestDocFilesNotEmpty checks that no doc file is empty.
func TestDocFilesNotEmpty(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, _ := e.Info()
		if info.Size() == 0 {
			t.Errorf("%s is empty", e.Name())
		}
	}
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) {
			return true
		}
	}
	return false
}

// claudeSymbols is a curated allowlist of Go identifiers that CLAUDE.md documents
// as existing API (shared helpers, key types/methods, design-pattern hooks). If any
// is removed or renamed in code without updating CLAUDE.md, this test fails —
// catching the kind of doc drift that left a stale "AddRolloutFunc" note in the file.
//
// When you legitimately rename/remove one of these, update CLAUDE.md AND this list
// in the same change. The `file` field is a hint shown in the failure message only;
// it does not gate the check (symbols may move between files).
var claudeSymbols = []struct {
	id   string // Go identifier to find as a whole word in pkg/ + cmd/ source
	file string // file where CLAUDE.md claims it lives (hint only)
}{
	// handler rollback registry
	{"AddRollbackFunc", "rollback.go"},
	{"getRollbackFuncs", "rollback.go"},
	{"executeRollbackFuncs", ""},
	// daemon/action connection helpers
	{"findConnection", "connection.go"},
	{"removeConnection", "connection.go"},
	{"resetCurrentConnection", ""},
	{"cleanupConnection", "connection.go"},
	{"getSudoTunIPs", ""},
	{"resolveTunIP", ""},
	// daemon/action stream/writer/lifecycle helpers
	{"newStreamWriter", "writer.go"},
	{"initStreamLogger", "writer.go"},
	{"resolveKubeconfigBytes", "writer.go"},
	{"NewSessionLifecycle", "lifecycle.go"},
	// util shared helpers
	{"InitFactoryByBytes", ""},
	{"InitFactoryByPath", ""},
	{"InitKubeClient", ""},
	{"IsNewer", ""},
	{"GetConnectionID", ""},
	// ssh
	{"SshJumpBytes", "jump.go"},
	{"SshJump", "jump.go"},
	// inject strategy
	{"NewInjector", "injector.go"},
	{"gatherContainerPorts", ""},
	{"addEnvoyConfig", "envoy.go"},
	// controlplane / envoy config types
	{"IsFargateMode", ""},
	{"ParsePortMap", ""},
	{"CurrentSchemaVersion", ""},
}

// TestClaudeMDSymbolsExist asserts that every identifier CLAUDE.md documents as
// existing API is actually present in the Go source under pkg/ and cmd/. It guards
// against documentation drifting from code (e.g. documenting a helper that was
// removed, as once happened with AddRolloutFunc).
func TestClaudeMDSymbolsExist(t *testing.T) {
	type src struct {
		path    string
		content string
	}
	var sources []src
	for _, root := range []string{"pkg", "cmd"} {
		filepath.Walk(filepath.Join(repoRoot, root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sources = append(sources, src{path: path, content: string(data)})
			return nil
		})
	}

	for _, sym := range claudeSymbols {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(sym.id) + `\b`)
		found := false
		for _, s := range sources {
			if re.MatchString(s.content) {
				found = true
				break
			}
		}
		if !found {
			hint := ""
			if sym.file != "" {
				hint = fmt.Sprintf(" (CLAUDE.md says it lives in %s)", sym.file)
			}
			t.Errorf("CLAUDE.md documents symbol %q but it is not found in any pkg/ or cmd/ .go source%s — update CLAUDE.md and docs_test.go's claudeSymbols list", sym.id, hint)
		}
	}
}

func TestMain(m *testing.M) {
	if repoRoot == "" {
		fmt.Fprintln(os.Stderr, "cannot find repo root")
		os.Exit(1)
	}
	os.Exit(m.Run())
}
