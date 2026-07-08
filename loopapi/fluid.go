package loopapi

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ExtractText reconstructs readable text from a Loop page's Fluid ODSP snapshot
// (content-type application/ms-fluid).
//
// Loop stores page content as a Fluid "merge tree" (SharedString). The snapshot
// embeds a series of JSON chunks, each carrying a "segmentTexts" array whose
// elements are plain strings, {"text": "..."} runs, or {"marker": ...} nodes
// that denote block boundaries (paragraphs, list items, headings). We walk the
// snapshot in order, concatenate the text runs, and insert newlines on markers.
func ExtractText(snapshot []byte) string {
	s := string(snapshot)
	var out strings.Builder
	i := 0
	for {
		j := strings.Index(s[i:], `"segmentTexts"`)
		if j < 0 {
			break
		}
		abs := i + j
		start := strings.LastIndexByte(s[:abs], '{')
		if start < 0 {
			i = abs + len(`"segmentTexts"`)
			continue
		}
		end := matchBrace(s, start)
		if end < 0 {
			i = abs + len(`"segmentTexts"`)
			continue
		}
		blob := s[start:end]
		i = end

		var chunk struct {
			SegmentTexts []json.RawMessage `json:"segmentTexts"`
		}
		if err := json.Unmarshal([]byte(blob), &chunk); err != nil {
			continue
		}
		for _, raw := range chunk.SegmentTexts {
			appendSegment(&out, raw)
		}
	}
	return normalize(out.String())
}

func appendSegment(out *strings.Builder, raw json.RawMessage) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 {
		return
	}
	// Plain string run.
	if trimmed[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			out.WriteString(str)
		}
		return
	}
	// Object: either a styled text run or a block marker.
	var obj struct {
		Text   *string          `json:"text"`
		Marker *json.RawMessage `json:"marker"`
		Props  struct {
			NodeType string `json:"nodeType"`
		} `json:"props"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	switch {
	case obj.Text != nil:
		out.WriteString(*obj.Text)
	case obj.Marker != nil:
		// Block boundary. ListItem / Paragraph / Heading all break the line.
		out.WriteByte('\n')
	}
}

// matchBrace returns the index just past the '}' that closes the '{' at start,
// respecting JSON string/escape rules.
func matchBrace(s string, start int) int {
	depth := 0
	inStr := false
	esc := false
	for k := start; k < len(s); k++ {
		c := s[k]
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return -1
}

var multiNewline = regexp.MustCompile(`\n{3,}`)

func normalize(s string) string {
	s = multiNewline.ReplaceAllString(s, "\n\n")
	// Trim trailing spaces on each line.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
