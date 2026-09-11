package docsgate

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the reader the link gate is built on. It is deliberately small:
// it knows how to find the HEADINGS and the LINKS in a markdown file and
// nothing else. Rendering is mdBook's job.
//
// Two things it must get right, because getting either wrong makes the gate
// lie rather than fail:
//
//   - Fenced code blocks and inline code spans are NOT prose. This book is full
//     of shell transcripts and YAML, and `# comment` inside a fence is not a
//     heading. A scan that counted them would invent anchors that do not exist,
//     which would make a genuinely broken link pass.
//   - The anchor derivation must be mdBook's, character for character. See the
//     package doc for why an approximation is the dangerous answer here.

// ---------------------------------------------------------------------------
// mdBook's anchor derivation
// ---------------------------------------------------------------------------

// htmlTag matches an HTML tag the way mdBook's own id derivation does, so a
// heading carrying inline markup contributes its TEXT to the anchor and not its
// tag names. Non-greedy and dot-matches-newline, like the Rust original.
var htmlTag = regexp.MustCompile(`(?s)<.*?>`)

// htmlEntities are the entity spellings mdBook strips outright before
// normalizing. They are the reason "Build, test & lint gates" and its rendered
// form "Build, test &amp; lint gates" derive the SAME anchor.
var htmlEntities = []string{"&lt;", "&gt;", "&amp;", "&#39;", "&quot;"}

// normalizeID is mdBook's normalize_id: keep alphanumerics, '_' and '-',
// ASCII-lowercase what is kept, turn each whitespace character into a hyphen,
// and DROP everything else.
//
// Dropping is what preserves a flag's leading dashes — '-' is a kept character,
// so "`--principal`" contributes "--principal" and the space in front of it
// contributes one more hyphen. That is the three-hyphen anchor this whole gate
// exists for. Nothing here collapses runs of hyphens, and nothing may be added
// that does.
func normalizeID(content string) string {
	var b strings.Builder
	b.Grow(len(content))
	for _, r := range content {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
			// Rust's to_ascii_lowercase touches A-Z only; a non-ASCII letter
			// is kept in whatever case it arrived in.
			if r < utf8.RuneSelf {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(r)
			}
		case unicode.IsSpace(r):
			b.WriteRune('-')
		}
	}
	return b.String()
}

// idFromContent is mdBook's id_from_content: strip HTML tags, strip the entity
// spellings, trim, drop a leading run of '#' left over from an ATX marker, trim
// again, then normalize.
func idFromContent(content string) string {
	s := htmlTag.ReplaceAllString(content, "")
	for _, e := range htmlEntities {
		s = strings.ReplaceAll(s, e, "")
	}
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "#")
	s = strings.TrimSpace(s)
	return normalizeID(s)
}

// anchorSet derives every anchor a page emits, in mdBook's order and with
// mdBook's de-duplication: the first occurrence of an id keeps it, the second
// gets "-1", the third "-2". The counter is per page, which is why this takes a
// whole file and not a heading.
func anchorSet(src string) map[string]bool {
	counts := make(map[string]int)
	out := make(map[string]bool)
	for _, h := range headings(src) {
		id := idFromContent(h.text)
		if id == "" {
			continue
		}
		n := counts[id]
		counts[id] = n + 1
		if n > 0 {
			id = id + "-" + strconv.Itoa(n)
		}
		out[id] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// The reader
// ---------------------------------------------------------------------------

// heading is one ATX heading: its inline text and the 1-based line it sits on.
type heading struct {
	text string
	line int
}

// link is one link destination found in a file, with the line it sits on so a
// failure can be acted on without grepping.
type link struct {
	dest string
	line int
}

var (
	atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	fenceOpen  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	// inlineLink matches [text](dest) and ![alt](dest). The destination stops at
	// the first whitespace so a link title — [text](dest "title") — is not
	// swallowed into the path. (?s) because this book wraps its prose at 80
	// columns and a link's TEXT routinely straddles a line break:
	//
	//	... the same [value
	//	model](#the-metadata-value-model), the same depth ...
	//
	// A line-at-a-time scan misses every one of those. There were 17 of them in
	// this book when the gate was written, and silently not checking a link is
	// the failure mode the gate exists to prevent.
	inlineLink = regexp.MustCompile(`(?s)!?\[[^\]]*\]\(\s*([^)\s]*)`)
	// refDefinition matches a link reference definition at the start of a line:
	// [label]: dest. A reference USAGE resolves to one of these, so checking the
	// definitions checks every use of them.
	refDefinition = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:[ \t]*(\S+)`)
	// htmlHref matches an href= attribute in raw HTML embedded in the markdown.
	htmlHref = regexp.MustCompile(`(?is)<a\s[^>]*href\s*=\s*"([^"]*)"`)
)

// scan walks a markdown source once and returns its ATX headings and its link
// destinations, with fenced code blocks and inline code spans removed from
// consideration.
//
// One pass, both outputs, because the fence state is the expensive thing to get
// right and neither caller may be allowed to disagree with the other about it.
//
// Links are matched over a PARAGRAPH rather than a line, because a link's text
// wraps but never crosses a blank line (CommonMark forbids it). That is the
// widest window that is still safe: an unmatched '[' can lead the regex to the
// end of its paragraph and no further.
//
// Setext headings ("Title" underlined with === or ---) are not recognised, and
// this book uses none — every one of its 488 anchors was reproduced from ATX
// headings alone. If one ever appears, the gate reports links INTO it as broken
// rather than reporting a broken link as fine, which is the direction a gate is
// allowed to be wrong in.
func scan(src string) ([]heading, []link) {
	var (
		heads []heading
		links []link
		fence string
		para  []string
		start int
	)

	flush := func() {
		if len(para) == 0 {
			return
		}
		text := strings.Join(para, "\n")
		para = para[:0]
		lineOf := func(off int) int {
			return start + strings.Count(text[:off], "\n")
		}
		for _, m := range inlineLink.FindAllStringSubmatchIndex(text, -1) {
			links = append(links, link{dest: text[m[2]:m[3]], line: lineOf(m[0])})
		}
		for _, m := range refDefinition.FindAllStringSubmatchIndex(text, -1) {
			links = append(links, link{dest: text[m[2]:m[3]], line: lineOf(m[0])})
		}
		for _, m := range htmlHref.FindAllStringSubmatchIndex(text, -1) {
			links = append(links, link{dest: text[m[2]:m[3]], line: lineOf(m[0])})
		}
	}

	for i, raw := range strings.Split(src, "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")

		if fence != "" {
			// Inside a fence: only a matching closing fence is meaningful.
			if m := fenceOpen.FindStringSubmatch(line); m != nil &&
				m[1][0] == fence[0] && len(m[1]) >= len(fence) &&
				strings.TrimSpace(line) == strings.TrimSpace(m[0]) {
				fence = ""
			}
			continue
		}
		if m := fenceOpen.FindStringSubmatch(line); m != nil {
			flush()
			fence = m[1]
			continue
		}

		if m := atxHeading.FindStringSubmatch(line); m != nil {
			flush()
			text := strings.TrimSpace(m[2])
			// Strip an optional closing sequence of '#'.
			if t := strings.TrimRight(text, "#"); t != text && (t == "" || strings.HasSuffix(t, " ")) {
				text = strings.TrimSpace(t)
			}
			heads = append(heads, heading{text: text, line: lineNo})
			continue
		}

		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if len(para) == 0 {
			start = lineNo
		}
		para = append(para, stripCodeSpans(line))
	}
	flush()
	return heads, links
}

func headings(src string) []heading {
	h, _ := scan(src)
	return h
}

func linksIn(src string) []link {
	_, l := scan(src)
	return l
}

// stripCodeSpans blanks out `inline code` so a link SHOWN as an example — the
// generated CLI reference is full of them — is not chased as a real link. The
// text is replaced with spaces rather than deleted so column positions, and
// therefore the rest of the line's parsing, are unchanged.
func stripCodeSpans(line string) string {
	b := []byte(line)
	i := 0
	for i < len(b) {
		if b[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		open := i
		j := i + n
		for j < len(b) {
			if b[j] == '`' {
				k := 0
				for j+k < len(b) && b[j+k] == '`' {
					k++
				}
				if k == n {
					for x := open; x < j+k; x++ {
						b[x] = ' '
					}
					i = j + k
					break
				}
				j += k
				continue
			}
			j++
		}
		if j >= len(b) {
			// Unterminated run of backticks: not a code span, move past it.
			i = open + n
		}
	}
	return string(b)
}
