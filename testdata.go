package main

// Test markdown content constants
// These eliminate magic values scattered throughout test files

const (
	// Basic markdown
	testMarkdownSimple = "# Test"
	testMarkdownHeader = "# Hello World\n\nThis is a **test**."

	// GFM features
	testMarkdownTable = "| A | B |\n|---|---|\n| 1 | 2 |"
	testMarkdownCode  = "```go\nfunc main() {}\n```"
	testMarkdownStrikethrough = "~~deleted~~"
	testMarkdownTaskList      = "- [x] Done\n- [ ] Todo"
	testMarkdownAutolink      = "https://example.com"

	// Content variations
	testMarkdownFileContent = "# File Content\n\nThis is the content."
	testMarkdownModified    = "# Modified Content"
	testMarkdownAllowed     = "# Allowed"
	testMarkdownSafe        = "# Safe"
	testMarkdownNavTest     = "# Nav Test"

	// Complex markdown
	testMarkdownComplex = `# Complex Document

This has:
- Lists
- **Bold** and *italic*
- [Links](https://example.com)

` + "```go\nfunc test() {}\n```"

	// Security test paths
	testPathTraversal     = "../../../etc/passwd"
	testPathURLEncoded    = "/view/%2e%2e%2f%2e%2e%2fetc%2fpasswd"
	testPathValidTilde    = "~/Documents"
	testPathEtc          = "/etc/passwd"
	testPathTmp          = "/tmp"
	testPathNullByte     = "~/safe.md\x00/../../etc/passwd"
)
