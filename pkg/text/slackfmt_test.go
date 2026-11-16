package text

import (
	"encoding/json"
	"strings"
	"testing"
)

type sanitizeCase struct {
	name string
	in   string
	want string
}

func sanitizeCases() []sanitizeCase {
	return []sanitizeCase{
		{name: "empty", in: "", want: ""},
		{name: "plain text unchanged", in: "just some words, nothing special", want: "just some words, nothing special"},
		{
			name: "slack native stays identical",
			in:   "*What it is*\n• *Governance:* preflight\n<https://linear.app/x|AGE-1286> and `code`",
			want: "*What it is*\n• *Governance:* preflight\n<https://linear.app/x|AGE-1286> and `code`",
		},
		{
			name: "slack native with quote, fence, emoji, mention and strike stays identical",
			in:   "Deploy :rocket: done by <@U0123ABC>\n> quote\n```\ncode here\n```\n_italic_ and ~strike~ and <https://example.com>",
			want: "Deploy :rocket: done by <@U0123ABC>\n> quote\n```\ncode here\n```\n_italic_ and ~strike~ and <https://example.com>",
		},
		{
			name: "README style markdown",
			in: "# Title\n\nSome **bold** and *italic* text with a [link](https://example.com).\n\n" +
				"## Features\n- item one\n- item `code`\n* star item\n\n```go\nfmt.Println(\"**x**\")\n```\n\n1. first",
			want: "*Title*\n\nSome *bold* and _italic_ text with a <https://example.com|link>.\n\n" +
				"*Features*\n• item one\n• item `code`\n• star item\n\n```\nfmt.Println(\"**x**\")\n```\n\n1. first",
		},
		{
			name: "mentions, channels, special mentions and emoji are preserved",
			in: "Hi <@U123ABC> and <@W456DEF|bob>, see <#C0456DEF|general> / <#C0456DEF> :tada: :+1: **done** " +
				"<!here> <!channel> <!everyone> <!subteam^S123ABC|@team> <!date^1392734382^{date_num}|fallback> <mailto:a@b.com|mail>",
			want: "Hi <@U123ABC> and <@W456DEF|bob>, see <#C0456DEF|general> / <#C0456DEF> :tada: :+1: *done* " +
				"<!here> <!channel> <!everyone> <!subteam^S123ABC|@team> <!date^1392734382^{date_num}|fallback> <mailto:a@b.com|mail>",
		},
		{
			name: "code content is not modified, only escaped",
			in:   "Use `**x**` and **y**\n```\n__z__ <b>\n```",
			want: "Use `**x**` and *y*\n```\n__z__ &lt;b&gt;\n```",
		},
		{
			name: "inline code is escaped but not converted",
			in:   "`a < b` and `**x**`",
			want: "`a &lt; b` and `**x**`",
		},
		{
			name: "known fence language is dropped",
			in:   "```python\nprint('**x**')\n```",
			want: "```\nprint('**x**')\n```",
		},
		{
			name: "unknown fence first line is kept",
			in:   "```foo\nbar\n```",
			want: "```foo\nbar\n```",
		},
		{
			name: "unterminated fence protects the rest of the text",
			in:   "```\nopen <b>",
			want: "```\nopen &lt;b&gt;",
		},
		{
			name: "html inline formatting, lists, links and entities",
			in: "<p>Hello <b>world</b> and <i>there</i></p><ul><li>one</li><li>two</li></ul>" +
				"<a href=\"https://x.com/a?b=1&amp;c=2\">link</a>&nbsp;&amp; done<br>next",
			want: "Hello *world* and _there_\n\n• one\n• two\n<https://x.com/a?b=1&amp;c=2|link> &amp; done\nnext",
		},
		{name: "html code and strong", in: "<code>x</code> and <strong>y</strong>", want: "`x` and *y*"},
		{name: "html pre", in: "<pre>a &lt; b</pre>", want: "```\na &lt; b\n```"},
		{name: "html heading and paragraph", in: "<h2>Title</h2><p>Body</p>", want: "*Title*\nBody"},
		{name: "html mixed with markdown", in: "<b>Bold</b> and **also**", want: "*Bold* and *also*"},
		{name: "unknown tag is kept and escaped", in: "<Foo>bar", want: "&lt;Foo&gt;bar"},
		{name: "generics are escaped", in: "Map<String, Integer> m", want: "Map&lt;String, Integer&gt; m"},
		{name: "escape ampersand and angle bracket", in: "a < b & c", want: "a &lt; b &amp; c"},
		{name: "existing entities are not double escaped", in: "a &lt; b &amp; c &gt; d", want: "a &lt; b &amp; c &gt; d"},
		{name: "ampersand and less-than mixed", in: "AT&T <3 you", want: "AT&amp;T &lt;3 you"},
		{name: "nbsp entity becomes a space", in: "a&nbsp;b &amp; c", want: "a b &amp; c"},
		{name: "blockquote marker is kept", in: "> note\ntext > other", want: "> note\ntext &gt; other"},
		{
			name: "heading with bold, closing hashes and a rule",
			in:   "# Title **bold**\n---\ntext",
			want: "*Title bold*\n────────\ntext",
		},
		{name: "closing hashes of a heading", in: "## Hello **World** ##", want: "*Hello World*"},
		{name: "horizontal rule", in: "a\n\n---\n\nb", want: "a\n\n────────\n\nb"},
		{name: "bold italic", in: "***x*** and **_y_**", want: "*_x_* and *_y_*"},
		{name: "strikethrough", in: "~~gone~~ stays", want: "~gone~ stays"},
		{name: "dunder is bold in markdown", in: "snake_case_name and __init__", want: "snake_case_name and *init*"},
		{
			name: "image, relative link and anchor",
			in:   "![logo](https://x.com/l.png) [Docs](./docs/readme.md) [Top](#top)",
			want: "<https://x.com/l.png|logo> Docs (./docs/readme.md) Top",
		},
		{name: "emphasis inside a link label is dropped", in: "[**bold** link](https://x.com)", want: "<https://x.com|bold link>"},
		{name: "link inside bold", in: "**[a](https://x.com)**", want: "*<https://x.com|a>*"},
		{
			name: "list markers keep their indentation",
			in:   "- one\n  - nested\n+ plus\n* star",
			want: "• one\n  • nested\n• plus\n• star",
		},
		{name: "star list without any markdown signal", in: "* one\n* two", want: "• one\n• two"},
		{name: "single asterisk is italic in markdown", in: "**a** and *b*", want: "*a* and _b_"},
		{name: "single asterisk is bold without markdown signals", in: "*a* and *b*", want: "*a* and *b*"},
		{name: "list item is a markdown signal", in: "- *x* item", want: "• _x_ item"},
		{name: "intraword stars are not emphasis", in: "2*3*4 and **bold**", want: "2*3*4 and *bold*"},
		{
			name: "tables are left alone",
			in:   "| a | b |\n|---|---|\n| 1 | 2 |",
			want: "| a | b |\n|---|---|\n| 1 | 2 |",
		},
		{name: "CRLF is normalised", in: "a\r\nb", want: "a\nb"},
		{name: "placeholder characters in the input are removed", in: "a\uE000b\uE001c", want: "abc"},
	}
}

func TestSanitizeSlackMrkdwn(t *testing.T) {
	for _, tc := range sanitizeCases() {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeSlackMrkdwn(tc.in); got != tc.want {
				t.Errorf("SanitizeSlackMrkdwn(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSlackMrkdwnIsIdempotent(t *testing.T) {
	for _, tc := range sanitizeCases() {
		t.Run(tc.name, func(t *testing.T) {
			once := SanitizeSlackMrkdwn(tc.in)
			twice := SanitizeSlackMrkdwn(once)
			if once != twice {
				t.Errorf("not idempotent for %q\n once: %q\ntwice: %q", tc.in, once, twice)
			}
		})
	}
}

func TestSanitizeSlackMrkdwnNativeInputIsByteIdentical(t *testing.T) {
	native := []string{
		"*bold* and _italic_ and ~strike~",
		"• *Governance:* preflight\n• second",
		"<https://linear.app/x|AGE-1286> and <https://example.com>",
		"cc <@U0123ABC> in <#C0456DEF|general> :white_check_mark:",
		"```\nline one\nline two\n```",
		"> quoted\n>>> block",
		"1. first\n2. second",
		"a &lt; b &amp; c &gt; d",
	}
	for _, in := range native {
		if got := SanitizeSlackMrkdwn(in); got != in {
			t.Errorf("native input changed\n  in: %q\n out: %q", in, got)
		}
	}
}

func sanitizeStringAt(t *testing.T, v any, path ...any) string {
	t.Helper()
	cur := v
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("expected an object at %q, got %T", key, cur)
			}
			cur = m[key]
		case int:
			a, ok := cur.([]any)
			if !ok || key >= len(a) {
				t.Fatalf("expected an array with index %d, got %T", key, cur)
			}
			cur = a[key]
		}
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("expected a string, got %T", cur)
	}
	return s
}

func TestSanitizeBlocksJSON(t *testing.T) {
	in := `[
		{"type":"section","text":{"type":"mrkdwn","text":"**bold** &"},
		 "fields":[{"type":"mrkdwn","text":"# H"},{"type":"plain_text","text":"**keep** <x>"}]},
		{"type":"context","elements":[{"type":"mrkdwn","text":"[a](https://x.com)"},{"type":"plain_text","text":"__keep__"}]},
		{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"**raw**"}]}]},
		{"type":"section","text":{"type":"plain_text","text":"p"},
		 "accessory":{"type":"overflow","options":[{"text":{"type":"mrkdwn","text":"**n**"},"value":"**v**"}]}}
	]`

	out, err := SanitizeBlocksJSON([]byte(in))
	if err != nil {
		t.Fatalf("SanitizeBlocksJSON returned an error: %v", err)
	}

	var blocks any
	if err := json.Unmarshal(out, &blocks); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	checks := []struct {
		name string
		path []any
		want string
	}{
		{"section text is sanitised", []any{0, "text", "text"}, "*bold* &amp;"},
		{"section mrkdwn field is sanitised", []any{0, "fields", 0, "text"}, "*H*"},
		{"section plain_text field is untouched", []any{0, "fields", 1, "text"}, "**keep** <x>"},
		{"context mrkdwn element is sanitised", []any{1, "elements", 0, "text"}, "<https://x.com|a>"},
		{"context plain_text element is untouched", []any{1, "elements", 1, "text"}, "__keep__"},
		{"rich_text is untouched", []any{2, "elements", 0, "elements", 0, "text"}, "**raw**"},
		{"nested mrkdwn text object is sanitised", []any{3, "accessory", "options", 0, "text", "text"}, "*n*"},
		{"non text-object strings are untouched", []any{3, "accessory", "options", 0, "value"}, "**v**"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeStringAt(t, blocks, c.path...); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	again, err := SanitizeBlocksJSON(out)
	if err != nil {
		t.Fatalf("second pass returned an error: %v", err)
	}
	if string(again) != string(out) {
		t.Errorf("SanitizeBlocksJSON is not idempotent\n once: %s\ntwice: %s", out, again)
	}
}

func TestSanitizeBlocksJSONKeepsLargeNumbers(t *testing.T) {
	in := `[{"type":"section","text":{"type":"mrkdwn","text":"x"},"extra":12345678901234567890}]`
	out, err := SanitizeBlocksJSON([]byte(in))
	if err != nil {
		t.Fatalf("SanitizeBlocksJSON returned an error: %v", err)
	}
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("large number was altered: %s", out)
	}
}

func TestSanitizeBlocksJSONRejectsMalformedInput(t *testing.T) {
	for _, in := range []string{"", "not json", `[{"type":`, "[1] x", "[] []"} {
		if _, err := SanitizeBlocksJSON([]byte(in)); err == nil {
			t.Errorf("expected an error for %q", in)
		}
	}
}
