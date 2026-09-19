// Package readmedoc reads the examples README.md documents, so a contract test
// can hold what a package produces against the block that is the contract.
// Response returns an endpoint's documented JSON, and Fence a fenced block held
// against a file on disk, such as the compose file the quick start embeds. It
// is test support: nothing the daemon runs calls it.
package readmedoc

import (
	"fmt"
	"os"
	"strings"
)

// Response returns the response example under the first "###" heading
// containing heading, as JSON with its line comments stripped.
//
// The example is located by that heading and by the "// response" line
// introducing it, so a neighbouring endpoint's block cannot leak keys in, and
// neither can the request example some sections put in the same fence.
func Response(readmePath, heading string) ([]byte, error) {
	lines, err := readLines(readmePath)
	if err != nil {
		return nil, err
	}
	start, end, err := section(lines, heading, 3)
	if err != nil {
		return nil, fmt.Errorf("readmedoc: %s %w", readmePath, err)
	}
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		stop, err := fenceEnd(lines, i)
		if err != nil {
			return nil, fmt.Errorf("readmedoc: %s %w", readmePath, err)
		}
		for j := i + 1; j < stop; j++ {
			if strings.TrimSpace(lines[j]) != "// response" {
				continue
			}
			body := stripLineComments(strings.Join(lines[j+1:stop], "\n"))
			// A "// response" line with nothing after it is a documentation bug,
			// not a response. Saying so beats handing the caller an empty slice to
			// report as a JSON parse failure.
			if strings.TrimSpace(body) == "" {
				return nil, fmt.Errorf("readmedoc: the %s response block is empty", heading)
			}
			return []byte(body), nil
		}
		i = stop
	}
	return nil, fmt.Errorf("readmedoc: the %s section has no // response block", heading)
}

// Fence returns the body of the first fenced block whose info string starts
// with info, so "```yaml" and "```yaml title=x" both match, under the first
// heading, at any level, containing heading. Each body line comes back with its
// newline, which is how the source spells it, so the result holds byte for byte
// against a file that ends in one. An empty block is an error, the way an empty
// response example is.
func Fence(readmePath, heading, info string) ([]byte, error) {
	lines, err := readLines(readmePath)
	if err != nil {
		return nil, err
	}
	start, end, err := section(lines, heading, 1)
	if err != nil {
		return nil, fmt.Errorf("readmedoc: %s %w", readmePath, err)
	}
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		stop, err := fenceEnd(lines, i)
		if err != nil {
			return nil, fmt.Errorf("readmedoc: %s %w", readmePath, err)
		}
		if fenceInfo(lines[i]) == info {
			if stop == i+1 {
				return nil, fmt.Errorf("readmedoc: the %s section's ```%s block is empty", heading, info)
			}
			return []byte(strings.Join(lines[i+1:stop], "\n") + "\n"), nil
		}
		i = stop
	}
	return nil, fmt.Errorf("readmedoc: the %s section has no ```%s block", heading, info)
}

func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), "\n"), nil
}

// section returns the line range [start, end) under the first heading of level
// minLevel or deeper that contains heading. The range runs to the next heading
// of the same or a shallower level, so a subheading stays inside its section.
// Only ATX headings count, "#" to "######" followed by a space, and a fenced
// block is skipped whole on both scans, so a "#" comment in an embedded YAML or
// shell block can neither open a section nor close one.
func section(lines []string, heading string, minLevel int) (start, end int, err error) {
	level := 0
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "```") {
			if i, err = fenceEnd(lines, i); err != nil {
				return 0, 0, err
			}
			continue
		}
		if l := headingLevel(lines[i]); l >= minLevel && strings.Contains(lines[i], heading) {
			level = l
			break
		}
	}
	if i == len(lines) {
		return 0, 0, fmt.Errorf("has no %s heading", heading)
	}
	start = i + 1
	for end = start; end < len(lines); end++ {
		if strings.HasPrefix(lines[end], "```") {
			if end, err = fenceEnd(lines, end); err != nil {
				return 0, 0, err
			}
			continue
		}
		if l := headingLevel(lines[end]); l > 0 && l <= level {
			break
		}
	}
	return start, end, nil
}

// headingLevel returns the level of an ATX heading line, or 0 for any other
// line. "#4416" and "#!/bin/sh" are not headings; "#" alone is an empty one.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0
	}
	if n == len(line) || line[n] == ' ' || line[n] == '\t' {
		return n
	}
	return 0
}

// fenceEnd returns the index of the line that closes the fence opened at open.
func fenceEnd(lines []string, open int) (int, error) {
	for i := open + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "```") {
			return i, nil
		}
	}
	return 0, fmt.Errorf("has an unterminated fenced block opened at line %d", open+1)
}

// fenceInfo returns the first word of a fence's info string, which is the
// language, so a trailing space or an attribute after it does not hide a block.
func fenceInfo(open string) string {
	fields := strings.Fields(strings.TrimPrefix(open, "```"))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// stripLineComments removes // comments from a JSON-with-comments document. It
// tracks string literals, so a "//" inside a URL survives.
func stripLineComments(src string) string {
	var b strings.Builder
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inString && c == '\\' && i+1 < len(src):
			b.WriteByte(c)
			i++
			c = src[i]
		case c == '"':
			inString = !inString
		case !inString && c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i-- // leave the newline for the loop to copy
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
