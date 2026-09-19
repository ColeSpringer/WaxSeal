package readmedoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDoc(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFence pins the scanner's reading of Markdown: the block is picked by its
// section and by the first word of its info string, a fence of another kind on
// the way is skipped whole, a "#" comment inside a fence is neither the heading
// nor the end of the section, and a deeper subheading does not end it either.
// The json block exists only in the section after, so finding it would mean
// the section boundary was not honoured.
func TestFence(t *testing.T) {
	doc := writeDoc(t, strings.Join([]string{
		"# Title",
		"```yaml",
		"# Quick start named in a comment before the real heading",
		"decoy: true",
		"```",
		"## Quick start",
		"prose",
		"```sh",
		"# a shell comment, not a heading",
		"echo hi",
		"```",
		"### A subheading inside the section",
		"```yaml title=compose.yaml",
		"# a yaml comment, not a heading",
		"services:",
		"  x: {}",
		"```",
		"## Next",
		"```yaml",
		"wrong: block",
		"```",
		"```json",
		"{}",
		"```",
		"",
	}, "\n"))
	got, err := Fence(doc, "Quick start", "yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := "# a yaml comment, not a heading\nservices:\n  x: {}\n"
	if string(got) != want {
		t.Errorf("Fence = %q, want %q", got, want)
	}
	if _, err := Fence(doc, "Quick start", "json"); err == nil || !strings.Contains(err.Error(), "no ```json block") {
		t.Errorf("the json block belongs to the next section, got %v", err)
	}
	if _, err := Fence(doc, "Missing", "yaml"); err == nil || !strings.Contains(err.Error(), "no Missing heading") {
		t.Errorf("a missing heading should say so, got %v", err)
	}
}

func TestFenceEmptyBlock(t *testing.T) {
	doc := writeDoc(t, "## Quick start\n```yaml\n```\n")
	if _, err := Fence(doc, "Quick start", "yaml"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty block should be reported, got %v", err)
	}
}

func TestFenceUnterminated(t *testing.T) {
	doc := writeDoc(t, "## Quick start\n```yaml\nopen: forever\n")
	if _, err := Fence(doc, "Quick start", "yaml"); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("an unterminated fence should be reported, got %v", err)
	}
}

// TestResponseSectionEndsAtPeerHeading: a "###" section ends at the next "###"
// or "##", so the response block of the endpoint after cannot be read as this
// one's, and a "####" inside it does not end it.
func TestResponseSectionEndsAtPeerHeading(t *testing.T) {
	doc := writeDoc(t, strings.Join([]string{
		"### `POST /a`",
		"#### notes",
		"```jsonc",
		"// request",
		`{"a": 1}`,
		"```",
		"### `POST /b`",
		"```jsonc",
		"// response",
		`{"b": 2}`,
		"```",
		"",
	}, "\n"))
	if _, err := Response(doc, "/a"); err == nil || !strings.Contains(err.Error(), "no // response block") {
		t.Errorf("/a has no response block of its own, got %v", err)
	}
	got, err := Response(doc, "/b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != `{"b": 2}` {
		t.Errorf("Response(/b) = %q", got)
	}
}

func TestHeadingLevel(t *testing.T) {
	for line, want := range map[string]int{
		"# a":            1,
		"### `POST /x`":  3,
		"###### six":     6,
		"####### seven":  0,
		"#4416":          0,
		"#!/bin/sh":      0,
		"#":              1,
		"##":             2,
		"prose # inline": 0,
		"":               0,
	} {
		if got := headingLevel(line); got != want {
			t.Errorf("headingLevel(%q) = %d, want %d", line, got, want)
		}
	}
}
