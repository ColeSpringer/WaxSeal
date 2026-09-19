// Package readmedoc reads the response examples README.md documents, so a
// contract test can hold what a package produces against the block that is the
// contract. It is test support: nothing the daemon runs calls it.
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
	raw, err := os.ReadFile(readmePath)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "###") && strings.Contains(lines[i], heading) {
			break
		}
	}
	if i == len(lines) {
		return nil, fmt.Errorf("readmedoc: %s has no %s heading", readmePath, heading)
	}
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "##") && !strings.Contains(lines[i], heading) {
			break // the next section started, so this one had no response block
		}
		if !strings.HasPrefix(lines[i], "```") {
			continue
		}
		end := i + 1
		for ; end < len(lines) && !strings.HasPrefix(lines[end], "```"); end++ {
		}
		if end == len(lines) {
			return nil, fmt.Errorf("readmedoc: unterminated fenced block after %s", heading)
		}
		for j := i + 1; j < end; j++ {
			if strings.TrimSpace(lines[j]) != "// response" {
				continue
			}
			body := stripLineComments(strings.Join(lines[j+1:end], "\n"))
			// A "// response" line with nothing after it is a documentation bug,
			// not a response. Saying so beats handing the caller an empty slice to
			// report as a JSON parse failure.
			if strings.TrimSpace(body) == "" {
				return nil, fmt.Errorf("readmedoc: the %s response block is empty", heading)
			}
			return []byte(body), nil
		}
		i = end
	}
	return nil, fmt.Errorf("readmedoc: the %s section has no // response block", heading)
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
