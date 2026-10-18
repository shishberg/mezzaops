package telegram

import "strings"

var (
	htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

// markdownToHTML converts the Markdown subset the frontends write (see
// service.WebhookInfo.FormatMessage) to Telegram's HTML parse mode: **bold**,
// `code`, fenced code blocks, [text](url) links and "> " quote lines. Telegram's
// MarkdownV2 would need nearly every punctuation character escaped, so HTML is
// the easier target. &, < and > are escaped everywhere. Anything unmatched (a
// lone "**", an unclosed backtick or fence) is left as literal text.
func markdownToHTML(s string) string {
	lines := strings.Split(s, "\n")

	var out strings.Builder
	started := false
	afterBlock := false
	// emit appends one piece, newline-separated from the previous one. Block
	// elements (pre, blockquote) already end the line in Telegram's renderer,
	// so no newline is added after them.
	emit := func(piece string, block bool) {
		if started && !afterBlock {
			out.WriteByte('\n')
		}
		out.WriteString(piece)
		started = true
		afterBlock = block
	}

	for i := 0; i < len(lines); {
		line := lines[i]

		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if end := closingFence(lines, i+1); end >= 0 {
				code := strings.Join(lines[i+1:end], "\n")
				emit("<pre>"+htmlEscaper.Replace(code)+"</pre>", true)
				i = end + 1
				continue
			}
		}

		if strings.HasPrefix(line, "> ") {
			var quoted []string
			for ; i < len(lines) && strings.HasPrefix(lines[i], "> "); i++ {
				quoted = append(quoted, inlineToHTML(lines[i][2:]))
			}
			emit("<blockquote>"+strings.Join(quoted, "\n")+"</blockquote>", true)
			continue
		}

		emit(inlineToHTML(line), false)
		i++
	}
	return out.String()
}

// closingFence returns the index of the first line at or after from that is
// a bare ``` fence, or -1.
func closingFence(lines []string, from int) int {
	for j := from; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "```" {
			return j
		}
	}
	return -1
}

// inlineToHTML converts bold, inline code and links within a single line.
func inlineToHTML(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if end := codeEnd(s, i); end > 0 {
				sb.WriteString("<code>" + htmlEscaper.Replace(s[i+1:end]) + "</code>")
				i = end + 1
				continue
			}
		case strings.HasPrefix(s[i:], "**"):
			if end := boldEnd(s, i+2); end > i+2 {
				sb.WriteString("<b>" + inlineToHTML(s[i+2:end]) + "</b>")
				i = end + 2
				continue
			}
			sb.WriteString("**")
			i += 2
			continue
		case s[i] == '[':
			if html, next, ok := linkToHTML(s, i); ok {
				sb.WriteString(html)
				i = next
				continue
			}
		}
		sb.WriteString(htmlEscaper.Replace(s[i : i+1]))
		i++
	}
	return sb.String()
}

// codeEnd returns the index of the backtick closing the code span opened at
// s[i], or -1 if there is none or the span would be empty.
func codeEnd(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '`')
	if j <= 0 {
		return -1
	}
	return i + 1 + j
}

// boldEnd returns the index of the "**" closing a bold span whose content
// starts at from, skipping over code spans so a "**" inside code doesn't
// close it. It returns -1 if there is none.
func boldEnd(s string, from int) int {
	for p := from; p < len(s); {
		if s[p] == '`' {
			if end := codeEnd(s, p); end > 0 {
				p = end + 1
				continue
			}
		}
		if strings.HasPrefix(s[p:], "**") {
			return p
		}
		p++
	}
	return -1
}

// linkToHTML parses a [text](url) link starting at s[i]. It returns the HTML,
// the index just past the link, and whether s[i:] began with a link.
func linkToHTML(s string, i int) (string, int, bool) {
	closeText := strings.IndexByte(s[i+1:], ']')
	if closeText <= 0 {
		return "", 0, false
	}
	k := i + 1 + closeText
	if k+1 >= len(s) || s[k+1] != '(' {
		return "", 0, false
	}
	closeURL := strings.IndexByte(s[k+2:], ')')
	if closeURL <= 0 {
		return "", 0, false
	}
	href := s[k+2 : k+2+closeURL]
	if strings.ContainsAny(href, " \t") {
		return "", 0, false
	}
	html := `<a href="` + attrEscaper.Replace(href) + `">` + inlineToHTML(s[i+1:k]) + "</a>"
	return html, k + 2 + closeURL + 1, true
}
