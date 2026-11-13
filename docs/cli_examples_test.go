package docs_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	// Import the commands package to get the actual Cobra commands
	cmdspkg "github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
)

// CLICheckResult represents a single issue found during CLI example checking
type CLICheckResult struct {
	File    string // Relative path to the file
	Line    int    // Line number in the file
	Command string // The command that was checked
	Message string // Description of the issue
}

// cliExample represents a single CLI example extracted from a markdown file
type cliExample struct {
	line    int
	command string
}

// extractCLIExamples extracts all CLI examples from markdown content that are
// marked with ```shell cli-checked or ```sh cli-checked
func extractCLIExamples(content string) []cliExample {
	var examples []cliExample

	// Normalize CRLF to LF: a Windows checkout converts LF files to CRLF, and the
	// closing-fence comparison below must see bare "```" lines to close a block.
	content = strings.ReplaceAll(content, "\r\n", "\n")

	lines := strings.Split(content, "\n")
	inBlock := false
	var buffer bytes.Buffer
	var startLine int

	for i, line := range lines {
		lineNum := i + 1
		if !inBlock {
			if strings.HasPrefix(line, "```shell cli-checked") || strings.HasPrefix(line, "```sh cli-checked") {
				inBlock = true
				startLine = lineNum
				buffer.Reset()
			}
		} else {
			if line == "```" {
				inBlock = false
				cmd := strings.TrimSpace(buffer.String())
				if cmd != "" {
					examples = append(examples, cliExample{line: startLine, command: cmd})
				}
			} else {
				buffer.WriteString(line)
				buffer.WriteString("\n")
			}
		}
	}

	return examples
}

// buildCommandIndex builds a map of command name to Cobra command
func buildCommandIndex() map[string]*cobra.Command {
	index := make(map[string]*cobra.Command)
	index["sync"] = cmdspkg.CmdSync(nil)
	index["disconnect"] = cmdspkg.CmdDisconnect(nil)
	return index
}

// validateCLIExample validates a single CLI example and returns any issues found
func validateCLIExample(cmdLine string, cmdIndex map[string]*cobra.Command) []CLICheckResult {
	var results []CLICheckResult

	parts := splitCommandLine(cmdLine)
	if len(parts) == 0 {
		return results
	}

	if parts[0] != "kubevpn" {
		return results // Not a kubevpn command, skip
	}

	if len(parts) < 2 {
		results = append(results, CLICheckResult{
			Message: "invalid kubevpn command",
		})
		return results
	}

	subCmd := parts[1]
	cmd, ok := cmdIndex[subCmd]
	if !ok {
		return results // Not a command we're validating, skip
	}

	// Special case checks first
	switch subCmd {
	case "sync":
		// Check that --local-dir is not used (it's not a valid flag)
		for _, part := range parts[2:] {
			if strings.HasPrefix(part, "--local-dir") || strings.HasPrefix(part, "-local-dir") {
				results = append(results, CLICheckResult{
					Message: "sync command should use --sync, not --local-dir",
				})
			}
		}

	case "disconnect":
		// Check that either --all is used or an ID is provided
		hasAllFlag := false
		hasIDArg := false

		for _, part := range parts[2:] {
			if part == "--all" || part == "-all" {
				hasAllFlag = true
			} else if !strings.HasPrefix(part, "-") {
				hasIDArg = true
			}
		}

		if !hasAllFlag && !hasIDArg {
			results = append(results, CLICheckResult{
				Message: "disconnect command needs either --all or a connection ID",
			})
		}
	}

	// Validate flags against the Cobra command
	flagIssues := validateFlags(parts[2:], cmd)
	results = append(results, flagIssues...)

	return results
}

// validateFlags validates the flags against the Cobra command
func validateFlags(flagParts []string, cmd *cobra.Command) []CLICheckResult {
	var results []CLICheckResult

	// Create a flag set from the command
	fs := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	fs.SetInterspersed(true)

	// Add all flags from the command
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		fs.AddFlag(f)
	})

	// Also add help flag which is added by Cobra automatically
	fs.BoolP("help", "h", false, "help for "+cmd.Name())

	// Parse all flags, interspersed with positional args
	err := fs.Parse(flagParts)
	if err != nil {
		results = append(results, CLICheckResult{
			Message: fmt.Sprintf("invalid flags: %v", err),
		})
	}

	return results
}

// splitCommandLine splits a command line into parts, handling quotes properly
func splitCommandLine(line string) []string {
	var parts []string
	var buffer bytes.Buffer
	inQuote := false
	var quoteChar byte

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' && !inQuote:
			if buffer.Len() > 0 {
				parts = append(parts, buffer.String())
				buffer.Reset()
			}
		case c == '"' || c == '\'':
			if inQuote && c == quoteChar {
				inQuote = false
			} else if !inQuote {
				inQuote = true
				quoteChar = c
			} else {
				buffer.WriteByte(c)
			}
		case c == '\\' && i+1 < len(line):
			buffer.WriteByte(line[i+1])
			i++
		default:
			buffer.WriteByte(c)
		}
	}

	if buffer.Len() > 0 {
		parts = append(parts, buffer.String())
	}

	return parts
}

// checkCLIExamples checks all CLI examples in a directory and returns any issues found
func checkCLIExamples(rootDir string, skipDirs []string) ([]CLICheckResult, error) {
	var results []CLICheckResult

	cmdIndex := buildCommandIndex()

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

		examples := extractCLIExamples(string(data))
		for _, example := range examples {
			issues := validateCLIExample(example.command, cmdIndex)
			for _, issue := range issues {
				issue.File = relPath
				issue.Line = example.line
				issue.Command = example.command
				results = append(results, issue)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}

// TestCLIExamples validates that CLI examples marked with "shell cli-checked"
// use valid flags and follow the expected patterns.
func TestCLIExamples(t *testing.T) {
	docsDir := filepath.Join(repoRoot, "docs")
	results, err := checkCLIExamples(docsDir, []string{"testdata"})
	if err != nil {
		t.Fatal(err)
	}

	// Also check the root directory files
	rootFiles := []string{"README.md", "README_ZH.md", "CONTRIBUTING.md", "SECURITY.md"}
	for _, f := range rootFiles {
		path := filepath.Join(repoRoot, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue // Skip if file doesn't exist
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		cmdIndex := buildCommandIndex()
		examples := extractCLIExamples(string(data))
		for _, example := range examples {
			issues := validateCLIExample(example.command, cmdIndex)
			for _, issue := range issues {
				issue.File = f
				issue.Line = example.line
				issue.Command = example.command
				results = append(results, issue)
			}
		}
	}

	// Report all issues
	for _, r := range results {
		t.Errorf("%s:%d: %s (command: %q)", r.File, r.Line, r.Message, r.Command)
	}
}

// TestCLIExamplesFixtures tests that our validation catches invalid examples.
func TestCLIExamplesFixtures(t *testing.T) {
	testFile := filepath.Join(repoRoot, "docs", "testdata", "cli-examples.md")
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}

	cmdIndex := buildCommandIndex()
	examples := extractCLIExamples(string(data))

	var totalIssues int
	var foundLocalDir bool
	var foundBareDisconnect bool
	var foundBadFlag bool
	var foundAnotherBadFlag bool

	for _, example := range examples {
		issues := validateCLIExample(example.command, cmdIndex)
		totalIssues += len(issues)
		t.Logf("Command: %q, Issues: %v", example.command, issues)

		for _, issue := range issues {
			if strings.Contains(issue.Message, "--local-dir") {
				foundLocalDir = true
			}
			if strings.Contains(issue.Message, "needs either --all or a connection ID") {
				foundBareDisconnect = true
			}
			if strings.Contains(issue.Message, "invalid flags") && strings.Contains(example.command, "--bad-flag") {
				foundBadFlag = true
			}
			if strings.Contains(issue.Message, "invalid flags") && strings.Contains(example.command, "--another-bad-flag") {
				foundAnotherBadFlag = true
			}
		}
	}

	// We expect 5 issues from our fixture (--local-dir, bare disconnect, --bad-flag, --another-bad-flag, and one more?)
	expectedIssues := 5
	if totalIssues != expectedIssues {
		t.Errorf("expected %d issues, found %d", expectedIssues, totalIssues)
	}

	if !foundLocalDir {
		t.Error("expected to find issue with --local-dir")
	}
	if !foundBareDisconnect {
		t.Error("expected to find issue with bare disconnect")
	}
	if !foundBadFlag {
		t.Error("expected to find issue with --bad-flag")
	}
	if !foundAnotherBadFlag {
		t.Error("expected to find issue with --another-bad-flag")
	}

	t.Logf("Found %d issues as expected", totalIssues)
}
