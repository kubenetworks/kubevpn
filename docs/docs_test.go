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

// LinkCheckResult represents a single issue found during link checking
type LinkCheckResult struct {
	File    string // Relative path to the file
	Message string // Description of the issue
}

// checkCrossReferences scans a directory and returns all link check issues found
func checkCrossReferences(rootDir string, skipDirs []string) ([]LinkCheckResult, error) {
	var results []LinkCheckResult

	// More precise link pattern - avoids matching things like [a](b, c)
	// Matches [text](url) where url doesn't contain unescaped spaces
	linkPattern := regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+(?:\s+[^)\s]+)*)\)`)

	// Create skip set for quick lookup
	skipSet := make(map[string]bool)
	for _, d := range skipDirs {
		skipSet[d] = true
	}

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipSet[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}

		relPath, err := filepath.Rel(rootDir, path)
		if err != nil {
			relPath = path
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Strip code blocks (```...```) before looking for links
		content := stripCodeBlocks(string(data))

		sourceDir := filepath.Dir(path)
		for _, match := range linkPattern.FindAllStringSubmatch(content, -1) {
			ref := strings.TrimSpace(match[2])

			// Skip external links
			if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "mailto:") {
				continue
			}

			// Check for user directory paths (false positives like /etc/ should not be flagged)
			if looksLikeUserDir(ref) {
				results = append(results, LinkCheckResult{
					File:    relPath,
					Message: fmt.Sprintf("link contains user directory path: %s", ref),
				})
			}

			// Strip fragment and query
			cleanRef := ref
			if idx := strings.Index(cleanRef, "#"); idx != -1 {
				cleanRef = cleanRef[:idx]
			}
			if idx := strings.Index(cleanRef, "?"); idx != -1 {
				cleanRef = cleanRef[:idx]
			}
			cleanRef = strings.TrimSpace(cleanRef)
			if cleanRef == "" {
				continue // anchor-only link
			}

			// Resolve the target path
			var target string
			switch {
			case strings.HasPrefix(cleanRef, "/docs/"):
				// Absolute path from repo docs root - check this first!
				target = filepath.Join(repoRoot, cleanRef[1:]) // strip leading /
			case filepath.IsAbs(cleanRef):
				// Other absolute paths - skip checking (likely system paths)
				continue
			default:
				// Relative path - resolve relative to source file directory
				target = filepath.Join(sourceDir, cleanRef)
			}

			if _, err := os.Stat(target); os.IsNotExist(err) {
				results = append(results, LinkCheckResult{
					File:    relPath,
					Message: fmt.Sprintf("broken link to %s (resolved to %s)", ref, target),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}

// stripCodeBlocks removes code blocks (```...```) from markdown content
// to avoid matching links inside code.
func stripCodeBlocks(content string) string {
	// Pattern to match fenced code blocks
	codeBlockPattern := regexp.MustCompile("(?s)```.*?```")
	return codeBlockPattern.ReplaceAllString(content, "")
}

var (
	userDirPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^/Users/[^/]+`),           // macOS
		regexp.MustCompile(`^/home/[^/]+`),            // Linux
		regexp.MustCompile(`^[A-Za-z]:\\Users\\[^\\]+`), // Windows
	}
	// False positive patterns - these look like user dirs but are system paths
	safePathPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^/etc/`),
		regexp.MustCompile(`^/tmp/`),
		regexp.MustCompile(`^/var/`),
		regexp.MustCompile(`^/dev/`),
		regexp.MustCompile(`^/proc/`),
		regexp.MustCompile(`^/sys/`),
	}
)

func looksLikeUserDir(path string) bool {
	// First check if it's a known safe path
	for _, p := range safePathPatterns {
		if p.MatchString(path) {
			return false
		}
	}
	// Then check if it matches user dir patterns
	for _, p := range userDirPatterns {
		if p.MatchString(path) {
			return true
		}
	}
	return false
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

// TestDocCrossReferences checks that docs/ internal links point to files that exist.
// It scans:
//   - docs/ directory (excluding testdata)
//   - Root directory files: README.md, README_ZH.md, CONTRIBUTING.md, SECURITY.md
func TestDocCrossReferences(t *testing.T) {
	var results []LinkCheckResult

	// First check the docs directory
	docsDir := filepath.Join(repoRoot, "docs")
	docsResults, err := checkCrossReferences(docsDir, []string{"testdata"})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, docsResults...)

	// Then check the root directory files by checking them in their actual location
	// To do this, we'll create a special check for individual files
	rootFiles := []string{"README.md", "README_ZH.md", "CONTRIBUTING.md", "SECURITY.md"}
	linkPattern := regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+(?:\s+[^)\s]+)*)\)`)

	for _, f := range rootFiles {
		path := filepath.Join(repoRoot, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue // Skip if file doesn't exist
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		content := stripCodeBlocks(string(data))
		sourceDir := repoRoot // Links are relative to repo root for these files

		for _, match := range linkPattern.FindAllStringSubmatch(content, -1) {
			ref := strings.TrimSpace(match[2])

			// Skip external links
			if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "mailto:") {
				continue
			}

			// Check for user directory paths
			if looksLikeUserDir(ref) {
				results = append(results, LinkCheckResult{
					File:    f,
					Message: fmt.Sprintf("link contains user directory path: %s", ref),
				})
			}

			// Strip fragment and query
			cleanRef := ref
			if idx := strings.Index(cleanRef, "#"); idx != -1 {
				cleanRef = cleanRef[:idx]
			}
			if idx := strings.Index(cleanRef, "?"); idx != -1 {
				cleanRef = cleanRef[:idx]
			}
			cleanRef = strings.TrimSpace(cleanRef)
			if cleanRef == "" {
				continue // anchor-only link
			}

			// Resolve the target path
			var target string
			switch {
			case strings.HasPrefix(cleanRef, "/docs/"):
				target = filepath.Join(repoRoot, cleanRef[1:])
			case filepath.IsAbs(cleanRef):
				continue // Skip other absolute paths
			default:
				target = filepath.Join(sourceDir, cleanRef)
			}

			if _, err := os.Stat(target); os.IsNotExist(err) {
				results = append(results, LinkCheckResult{
					File:    f,
					Message: fmt.Sprintf("broken link to %s (resolved to %s)", ref, target),
				})
			}
		}
	}

	// Report all issues
	for _, r := range results {
		t.Errorf("%s: %s", r.File, r.Message)
	}
}

// TestDocCrossReferencesFixtures tests the cross-reference logic against our test fixtures
// to verify it catches broken links, user directories, etc.
func TestDocCrossReferencesFixtures(t *testing.T) {
	testDir := filepath.Join(repoRoot, "docs", "testdata", "links")
	results, err := checkCrossReferences(testDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify we found the expected number of issues
	// Expected issues from our fixture:
	// - ../README.md (broken)
	// - missing.md (broken)
	// - subdir/nowhere.md (broken)
	// - images/nonexistent.png (broken)
	// - /Users/test/doc.md (user dir + broken)
	// - /home/user/file.md (user dir + broken)
	// - C:\Users\test\doc.md (user dir + broken)
	expectedIssueCount := 7 // 4 broken links + 3 user dir warnings (some have both)

	if len(results) < expectedIssueCount {
		t.Errorf("expected at least %d issues, found %d", expectedIssueCount, len(results))
	}

	// Check that specific issues are present
	foundMissingMD := false
	foundUserDir := false
	foundAnchorLink := false // Should be skipped

	for _, r := range results {
		if strings.Contains(r.Message, "missing.md") {
			foundMissingMD = true
		}
		if strings.Contains(r.Message, "user directory") {
			foundUserDir = true
		}
		if strings.Contains(r.Message, "anchor") {
			foundAnchorLink = true
		}
	}

	if !foundMissingMD {
		t.Error("expected to find broken link to missing.md")
	}
	if !foundUserDir {
		t.Error("expected to find user directory path warning")
	}
	if foundAnchorLink {
		t.Error("did not expect to find issue with anchor-only link")
	}

	t.Logf("Found %d issues as expected", len(results))
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
	{"connectionRegistry", "registry.go"},
	{"cleanupConnection", "connection.go"},
	{"getSudoTunIPs", ""},
	{"resolveTunIP", ""},
	// handler cross-package resource teardown helper
	{"DeleteTrafficManagerCoreResources", "traffmgr.go"},
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
