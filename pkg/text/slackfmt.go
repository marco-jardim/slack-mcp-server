package text

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Placeholder delimiters used while a message is being sanitised. They are Unicode
// private-use code points, so they cannot collide with Markdown, HTML or Slack syntax.
// Any occurrence in the input is removed before processing starts.
const (
	phOpen  = "\uE000"
	phClose = "\uE001"
)

// ruleLine is what a Markdown horizontal rule becomes: Slack mrkdwn has no rule element.
const ruleLine = "────────"

var (
	// Protected spans.
	fencedCodeRe = regexp.MustCompile("(?s)```.*?(?:```|$)")
	fenceLangRe  = regexp.MustCompile("^```([A-Za-z0-9_+#.-]+)[ \t]*\n")
	inlineCodeRe = regexp.MustCompile("`[^`\n]+`")
	htmlPreRe    = regexp.MustCompile(`(?is)<pre(?:\s[^<>]*)?>(.*?)</pre\s*>`)
	htmlCodeRe   = regexp.MustCompile(`(?is)<code(?:\s[^<>]*)?>(.*?)</code\s*>`)
	quoteMarkRe  = regexp.MustCompile(`(?m)^>{1,3}[ \t]?`)
	emojiRe      = regexp.MustCompile(`:[A-Za-z0-9_+\-]+:`)

	// slackTokenRe matches Slack-native angle-bracket tokens: user, user-group and channel
	// mentions, special mentions, dates, and links (with or without a label).
	slackTokenRe = regexp.MustCompile(
		`<@[A-Z0-9]+(?:\|[^<>]*)?>` +
			`|<#[A-Z0-9]+(?:\|[^<>]*)?>` +
			`|<!(?:here|channel|everyone)(?:\|[^<>]*)?>` +
			`|<!subteam\^[^<>]+>` +
			`|<!date\^[^<>]+>` +
			`|<(?:https?://|ftp://|slack://|mailto:|tel:)[^<>\s|]+(?:\|[^<>]*)?>`)

	placeholderRe = regexp.MustCompile("\uE000([0-9]+)\uE001")
	entityRe      = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);`)

	// Markdown syntax. The signal expressions below decide whether the input is Markdown.
	mdBoldSignalRe   = regexp.MustCompile(`\*\*[^\s*](?:[^\n]*?[^\s*])?\*\*`)
	mdUnderSignalRe  = regexp.MustCompile(`(?:^|[^\w])__[^\s_](?:[^\n]*?[^\s_])?__(?:[^\w]|$)`)
	mdStrikeSignalRe = regexp.MustCompile(`~~[^\s~](?:[^\n]*?[^\s~])?~~`)
	headingSignalRe  = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+\S`)
	listSignalRe     = regexp.MustCompile(`(?m)^[ \t]*[-+][ \t]+\S`)
	mdLinkRe         = regexp.MustCompile(`(!?)\[([^\[\]\n]*)\]\(([^()\s]*(?:\([^()\s]*\)[^()\s]*)*)(?:[ \t]+"[^"\n]*")?\)`)

	headingRe        = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+([^\n]*?)[ \t]*$`)
	trailingHashesRe = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	bulletRe         = regexp.MustCompile(`(?m)^([ \t]*)[-+*][ \t]+`)
	hrRe             = regexp.MustCompile(`(?m)^[ \t]{0,3}(?:-[ \t]*){3,}$|^[ \t]{0,3}(?:\*[ \t]*){3,}$|^[ \t]{0,3}(?:_[ \t]*){3,}$`)
	linkSchemeRe     = regexp.MustCompile(`(?i)^(?:https?://|ftp://|slack://|mailto:|tel:)`)
	blankLinesRe     = regexp.MustCompile(`\n{3,}`)

	// HTML. htmlTagRe lists the elements that are recognised as HTML (and stripped once
	// converted). Unknown tags such as <String> are deliberately left alone and escaped.
	htmlTagRe = regexp.MustCompile(`(?i)</?(?:b|strong|i|em|s|del|strike|u|code|pre|a|br|p|ul|ol|li|h[1-6]|div|span|blockquote|hr|table|thead|tbody|tr|td|th|img|font|center|sup|sub|small|big|mark)(?:\s[^<>]*)?/?>`)

	htmlLinkRe         = regexp.MustCompile(`(?is)<a\s[^<>]*?href\s*=\s*["']([^"']*)["'][^<>]*>(.*?)</a\s*>`)
	htmlImgRe          = regexp.MustCompile(`(?is)<img\s[^<>]*>`)
	htmlSrcRe          = regexp.MustCompile(`(?i)\bsrc\s*=\s*["']([^"']*)["']`)
	htmlAltRe          = regexp.MustCompile(`(?i)\balt\s*=\s*["']([^"']*)["']`)
	htmlBreakRe        = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlRuleRe         = regexp.MustCompile(`(?i)<hr\s*/?>`)
	htmlParaCloseRe    = regexp.MustCompile(`(?i)</p\s*>`)
	htmlHeadingOpenRe  = regexp.MustCompile(`(?i)<h([1-6])(?:\s[^<>]*)?>`)
	htmlHeadingCloseRe = regexp.MustCompile(`(?i)</h[1-6]\s*>`)
	htmlListItemRe     = regexp.MustCompile(`(?i)<li(?:\s[^<>]*)?>`)
	htmlListBlockRe    = regexp.MustCompile(`(?i)</?(?:ul|ol)(?:\s[^<>]*)?>|</(?:div|tr)\s*>`)
	htmlBoldRe         = regexp.MustCompile(`(?i)</?(?:b|strong)(?:\s[^<>]*)?>`)
	htmlItalicRe       = regexp.MustCompile(`(?i)</?(?:i|em)(?:\s[^<>]*)?>`)
	htmlStrikeRe       = regexp.MustCompile(`(?i)</?(?:s|del|strike)(?:\s[^<>]*)?>`)
)

// fenceLanguages are the info strings that are dropped from a fenced code block's opening
// line: Slack has no syntax highlighting and would print the tag as the first line of code.
var fenceLanguages = map[string]struct{}{
	"go": {}, "golang": {}, "python": {}, "py": {}, "js": {}, "javascript": {}, "ts": {},
	"typescript": {}, "tsx": {}, "jsx": {}, "bash": {}, "sh": {}, "shell": {}, "zsh": {},
	"json": {}, "yaml": {}, "yml": {}, "toml": {}, "ini": {}, "sql": {}, "html": {},
	"xml": {}, "css": {}, "java": {}, "kotlin": {}, "c": {}, "cpp": {}, "csharp": {},
	"rust": {}, "ruby": {}, "php": {}, "swift": {}, "diff": {}, "text": {}, "txt": {},
	"md": {}, "markdown": {}, "powershell": {}, "ps1": {}, "dockerfile": {}, "makefile": {},
	"console": {}, "plaintext": {}, "hcl": {}, "proto": {},
}

// stash keeps the text that is protected from every conversion while a message is being
// sanitised, and hands out placeholders that stand in for it.
type stash struct {
	items []string
}

func (s *stash) put(content string) string {
	s.items = append(s.items, content)
	return phOpen + strconv.Itoa(len(s.items)-1) + phClose
}

// restore substitutes every placeholder with its stored content. Stored content may itself
// contain placeholders (a link label that holds inline code), hence the bounded loop.
func (s *stash) restore(text string) string {
	for pass := 0; pass < 8 && strings.Contains(text, phOpen); pass++ {
		text = placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
			sub := placeholderRe.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			idx, err := strconv.Atoi(sub[1])
			if err != nil || idx < 0 || idx >= len(s.items) {
				// Unreachable for placeholders created by put; keep the text visible
				// rather than silently dropping it.
				return m
			}
			return s.items[idx]
		})
	}
	return text
}

// SanitizeSlackMrkdwn converts text written in Markdown, HTML or Slack mrkdwn into valid
// Slack mrkdwn, detecting which one it is. It is safe to apply to text that is already
// mrkdwn: such input comes back byte-identical, and the function is idempotent
// (SanitizeSlackMrkdwn(SanitizeSlackMrkdwn(x)) == SanitizeSlackMrkdwn(x)).
//
// Processing happens in this order:
//
//  1. Protected spans are replaced by placeholders and restored unchanged at the end: fenced
//     code blocks (a leading language tag such as ```go is dropped), inline code, HTML
//     <pre>/<code> (converted to the equivalent Markdown code span), a leading blockquote
//     marker (">" or ">>>" at the start of a line), Slack tokens (<@U…>, <@W…>, <#C…|name>,
//     <!here>, <!channel>, <!everyone>, <!subteam^…>, <!date^…>, <https://…|label>,
//     <mailto:…|label>) and :emoji_names:.
//  2. Detection. The remaining text is Markdown when it contains any of: "**bold**",
//     "__bold__", "~~strike~~", "[text](url)" or "![alt](url)", an ATX heading line ("# "
//     to "###### "), a list line starting with "- " or "+ ", or a known HTML tag. The only
//     thing detection changes is how an ambiguous single-asterisk span is read, because
//     "*x*" is italic in Markdown and bold in Slack: in Markdown input it becomes "_x_";
//     otherwise it is taken to be Slack bold and left alone. Everything below is
//     unambiguous and runs regardless.
//  3. Always-safe conversions: **x** and __x__ -> *x*; ***x*** -> *_x_*; ~~x~~ -> ~x~;
//     [t](url) and ![alt](url) -> <url|t> (links whose scheme Slack cannot render become
//     "t (url)"); headings -> *heading*; "- ", "+ " and "* " list markers -> "• " (indentation
//     kept); horizontal rules -> "────────"; HTML b/strong/i/em/s/del/strike/code/pre/a/br/p/
//     ul/ol/li/h1-h6/hr/img converted, other known HTML tags stripped (unknown tags such as
//     <String> are kept and escaped), and HTML entities decoded.
//  4. Escaping: "&", "<" and ">" become "&amp;", "&lt;" and "&gt;" everywhere except inside
//     the protected Slack tokens and a blockquote marker. Entities are decoded before they
//     are escaped again, which is what keeps existing "&amp;", "&lt;" and "&gt;" from being
//     escaped twice and makes the whole function idempotent.
//
// Not converted: GFM tables (left as they are), Markdown backslash escapes, reference-style
// links, and single-backtick-only spans that span several lines.
func SanitizeSlackMrkdwn(s string) string {
	if s == "" {
		return s
	}

	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.NewReplacer(phOpen, "", phClose, "").Replace(s)

	st := &stash{}
	sawHTMLCode := false

	s = fencedCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		return st.put(normaliseFence(m))
	})
	s = htmlPreRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlPreRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		sawHTMLCode = true
		inner := strings.Trim(htmlTagRe.ReplaceAllString(sub[1], ""), "\n")
		return st.put("```\n" + normaliseEntities(inner) + "\n```")
	})
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		return st.put(normaliseEntities(m))
	})
	s = htmlCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlCodeRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		sawHTMLCode = true
		inner := htmlTagRe.ReplaceAllString(sub[1], "")
		if strings.Contains(inner, "\n") {
			return st.put("```\n" + normaliseEntities(strings.Trim(inner, "\n")) + "\n```")
		}
		return st.put("`" + normaliseEntities(inner) + "`")
	})
	s = quoteMarkRe.ReplaceAllStringFunc(s, st.put)
	s = slackTokenRe.ReplaceAllStringFunc(s, st.put)
	s = emojiRe.ReplaceAllStringFunc(s, st.put)

	markdown := sawHTMLCode || hasMarkdownSignals(s)
	if markdown {
		s = convertSingleAsterisk(s)
	}

	s = convertHTML(s, st)
	s = convertMarkdownLinks(s, st)

	s = hrRe.ReplaceAllString(s, ruleLine)
	s = convertHeadings(s)
	s = bulletRe.ReplaceAllString(s, "${1}• ")

	s = convertPairs(s, "***", "*_", "_*", false)
	s = convertPairs(s, "**", "*", "*", false)
	s = convertPairs(s, "___", "*_", "_*", true)
	s = convertPairs(s, "__", "*", "*", true)
	s = convertPairs(s, "~~", "~", "~", false)

	s = normaliseEntities(s)
	return st.restore(s)
}

// hasMarkdownSignals reports whether s (with protected spans already removed) contains
// syntax that only Markdown or HTML uses. See SanitizeSlackMrkdwn for the list.
func hasMarkdownSignals(s string) bool {
	return mdBoldSignalRe.MatchString(s) ||
		mdUnderSignalRe.MatchString(s) ||
		mdStrikeSignalRe.MatchString(s) ||
		mdLinkRe.MatchString(s) ||
		headingSignalRe.MatchString(s) ||
		listSignalRe.MatchString(s) ||
		htmlTagRe.MatchString(s)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// normaliseFence drops a known language tag from the opening fence and escapes the body.
func normaliseFence(block string) string {
	if sub := fenceLangRe.FindStringSubmatch(block); sub != nil {
		if _, known := fenceLanguages[strings.ToLower(sub[1])]; known {
			block = "```\n" + block[len(sub[0]):]
		}
	}
	return normaliseEntities(block)
}

// normaliseEntities decodes HTML entities and then escapes "&", "<" and ">" for Slack,
// leaving Slack tokens untouched. Decoding first means an existing "&amp;" is not escaped
// into "&amp;amp;", and a second run produces the same output as the first.
func normaliseEntities(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range slackTokenRe.FindAllStringIndex(s, -1) {
		b.WriteString(escapeSegment(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(escapeSegment(s[last:]))
	return b.String()
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeSegment(s string) string {
	return slackEscaper.Replace(decodeEntities(s))
}

// decodeEntities decodes only well-formed, semicolon-terminated entities, so that a URL
// such as ?a=1&copy=2 is not rewritten. A non-breaking space becomes a plain space.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return entityRe.ReplaceAllStringFunc(s, func(m string) string {
		decoded := html.UnescapeString(m)
		if decoded == "\u00a0" {
			return " "
		}
		return decoded
	})
}

// convertSingleAsterisk turns Markdown italics (*x*) into Slack italics (_x_). It is only
// called for input detected as Markdown. A star run is left alone when it is part of "**",
// a list marker ("* " followed by a space), or inside a word (2*3*4).
func convertSingleAsterisk(s string) string {
	if !strings.Contains(s, "*") {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '*' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := findItalicEnd(s, i)
		if end < 0 {
			b.WriteByte('*')
			i++
			continue
		}
		b.WriteByte('_')
		b.WriteString(s[i+1 : end])
		b.WriteByte('_')
		i = end + 1
	}
	return b.String()
}

// findItalicEnd returns the index of the star that closes the italic span opened at s[i],
// or -1 when s[i] does not open one.
func findItalicEnd(s string, i int) int {
	if i+1 >= len(s) {
		return -1
	}
	if i > 0 {
		prev, _ := utf8.DecodeLastRuneInString(s[:i])
		if prev == '*' || isWordRune(prev) {
			return -1
		}
	}
	first, _ := utf8.DecodeRuneInString(s[i+1:])
	if first == '*' || unicode.IsSpace(first) {
		return -1
	}
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\n':
			return -1
		case '*':
			if j+1 < len(s) && s[j+1] == '*' {
				// A bold marker inside the span: step over both stars.
				j++
				continue
			}
			prev, _ := utf8.DecodeLastRuneInString(s[:j])
			if unicode.IsSpace(prev) {
				continue
			}
			if j+1 < len(s) {
				next, _ := utf8.DecodeRuneInString(s[j+1:])
				if isWordRune(next) {
					continue
				}
			}
			return j
		}
	}
	return -1
}

// convertPairs replaces delim...delim spans on a single line with open...closeStr. The
// content must be non-empty and neither start nor end with whitespace or the delimiter's
// own character. With wordBoundary the span may not touch a word character from outside
// (so snake__case__name is left alone, as in CommonMark).
func convertPairs(s, delim, open, closeStr string, wordBoundary bool) string {
	if !strings.Contains(s, delim) {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		if !strings.HasPrefix(s[i:], delim) {
			b.WriteByte(s[i])
			i++
			continue
		}
		contentStart := i + len(delim)
		closeAt := -1
		if canOpenPair(s, i, contentStart, delim, wordBoundary) {
			lineEnd := len(s)
			if nl := strings.IndexByte(s[contentStart:], '\n'); nl >= 0 {
				lineEnd = contentStart + nl
			}
			for from := contentStart; from < lineEnd; {
				idx := strings.Index(s[from:lineEnd], delim)
				if idx < 0 {
					break
				}
				pos := from + idx
				if canClosePair(s, contentStart, pos, delim, wordBoundary) {
					closeAt = pos
					break
				}
				from = pos + 1
			}
		}
		if closeAt < 0 {
			b.WriteString(delim)
			i = contentStart
			continue
		}
		b.WriteString(open)
		b.WriteString(s[contentStart:closeAt])
		b.WriteString(closeStr)
		i = closeAt + len(delim)
	}
	return b.String()
}

func canOpenPair(s string, start, contentStart int, delim string, wordBoundary bool) bool {
	if contentStart >= len(s) {
		return false
	}
	first, _ := utf8.DecodeRuneInString(s[contentStart:])
	if unicode.IsSpace(first) || first == rune(delim[0]) {
		return false
	}
	if wordBoundary && start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(s[:start])
		if isWordRune(prev) {
			return false
		}
	}
	return true
}

func canClosePair(s string, contentStart, pos int, delim string, wordBoundary bool) bool {
	if pos <= contentStart {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(s[:pos])
	if unicode.IsSpace(last) || last == rune(delim[0]) {
		return false
	}
	after := pos + len(delim)
	if wordBoundary && after < len(s) {
		next, _ := utf8.DecodeRuneInString(s[after:])
		if isWordRune(next) {
			return false
		}
	}
	return true
}

// convertHeadings turns ATX headings into a bold line. Emphasis markers inside the heading
// are dropped, because a bold span cannot be nested in another one.
func convertHeadings(s string) string {
	return headingRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := headingRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		content := trailingHashesRe.ReplaceAllString(sub[1], "")
		content = convertPairs(content, "**", "", "", false)
		content = convertPairs(content, "__", "", "", true)
		content = strings.TrimSpace(content)
		if content == "" {
			return ""
		}
		return "*" + content + "*"
	})
}

// convertMarkdownLinks rewrites [text](url) and ![alt](url) as Slack links.
func convertMarkdownLinks(s string, st *stash) string {
	if !strings.Contains(s, "](") {
		return s
	}
	return mdLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdLinkRe.FindStringSubmatch(m)
		if sub == nil || sub[3] == "" {
			return m
		}
		label := convertPairs(sub[2], "**", "", "", false)
		label = convertPairs(label, "__", "", "", true)
		label = convertPairs(label, "~~", "", "", false)
		return buildLink(st, sub[3], strings.TrimSpace(label))
	})
}

// buildLink returns the Slack link <url|label>, stored as a protected token. When the URL
// is not one Slack can render (relative path, anchor, unknown scheme) it returns plain text
// instead: "label (url)", or just the label for an in-page anchor.
func buildLink(st *stash, rawURL, label string) string {
	target := strings.TrimSpace(decodeEntities(rawURL))
	if !linkSchemeRe.MatchString(target) || strings.ContainsAny(target, "<>| \t\r\n") {
		switch {
		case label == "":
			return target
		case target == "" || strings.HasPrefix(target, "#"):
			return label
		default:
			return label + " (" + target + ")"
		}
	}

	label = strings.Join(strings.Fields(label), " ")
	if label == target {
		label = ""
	}
	token := "<" + strings.ReplaceAll(target, "&", "&amp;")
	if label != "" {
		token += "|" + normaliseEntities(label)
	}
	return st.put(token + ">")
}

// convertHTML converts the common HTML elements to Slack mrkdwn and strips the other
// recognised ones. It does nothing when the text holds no known HTML tag.
func convertHTML(s string, st *stash) string {
	if !htmlTagRe.MatchString(s) {
		return s
	}

	s = htmlLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlLinkRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		label := strings.TrimSpace(htmlTagRe.ReplaceAllString(sub[2], ""))
		return buildLink(st, sub[1], label)
	})
	s = htmlImgRe.ReplaceAllStringFunc(s, func(m string) string {
		src := htmlSrcRe.FindStringSubmatch(m)
		if src == nil {
			return ""
		}
		alt := ""
		if a := htmlAltRe.FindStringSubmatch(m); a != nil {
			alt = strings.TrimSpace(a[1])
		}
		return buildLink(st, src[1], alt)
	})

	s = htmlBreakRe.ReplaceAllString(s, "\n")
	s = htmlRuleRe.ReplaceAllString(s, "\n---\n")
	s = htmlParaCloseRe.ReplaceAllString(s, "\n\n")
	s = htmlHeadingOpenRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlHeadingOpenRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		return "\n" + strings.Repeat("#", int(sub[1][0]-'0')) + " "
	})
	s = htmlHeadingCloseRe.ReplaceAllString(s, "\n")
	s = htmlListItemRe.ReplaceAllString(s, "\n• ")
	s = strings.NewReplacer("</li>", "", "</LI>", "").Replace(s)
	s = htmlListBlockRe.ReplaceAllString(s, "\n")
	s = htmlBoldRe.ReplaceAllString(s, "*")
	s = htmlItalicRe.ReplaceAllString(s, "_")
	s = htmlStrikeRe.ReplaceAllString(s, "~")
	s = htmlTagRe.ReplaceAllString(s, "")

	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.Trim(s, "\n")
}

// SanitizeBlocksJSON applies SanitizeSlackMrkdwn to every Block Kit text object whose type
// is "mrkdwn" (a section's text and fields, context elements, and any nested text object)
// and returns the re-encoded JSON. plain_text objects, rich_text blocks and every other
// field are left untouched. It returns an error when raw is not a single valid JSON value,
// so malformed input is reported exactly as before.
func SanitizeBlocksJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected data after the top-level JSON value: %v", tok)
	}

	sanitizeBlockValue(v)

	return json.Marshal(v)
}

func sanitizeBlockValue(v any) {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			sanitizeBlockValue(item)
		}
	case map[string]any:
		kind, _ := t["type"].(string)
		switch kind {
		case "rich_text":
			return
		case "mrkdwn":
			if txt, ok := t["text"].(string); ok {
				t["text"] = SanitizeSlackMrkdwn(txt)
			}
			return
		}
		for _, child := range t {
			sanitizeBlockValue(child)
		}
	}
}
