package mastodon

import (
	"regexp"
	"strings"

	"github.com/fulldump/datame-suitcase/plugin"
)

var (
	anchor     = regexp.MustCompile(`(?is)<a\s[^>]*?href\s*=\s*"([^"]*)"[^>]*>(.*?)</a>`)
	tag        = regexp.MustCompile(`(?s)<[^>]*>`)
	invisible  = regexp.MustCompile(`(?is)class\s*=\s*"[^"]*\binvisible\b`)
	urlLike    = regexp.MustCompile(`^(https?://)?[^\s/]+\.[^\s]+$`)
	codeBlocks = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`)
	inlineCode = regexp.MustCompile(`(?is)<code[^>]*>(.*?)</code>`)
	bold       = regexp.MustCompile(`(?is)<(strong|b)\b[^>]*>(.*?)</(strong|b)>`)
	italic     = regexp.MustCompile(`(?is)<(em|i)\b[^>]*>(.*?)</(em|i)>`)
	quoteBlock = regexp.MustCompile(`(?is)<blockquote[^>]*>(.*?)</blockquote>`)
)

// htmlToMarkdown turns the HTML of a post into readable Markdown. Mastodon
// posts use a small set of tags (p, br, a, span, and on some servers
// strong, em, code, blockquote, lists), so a few rules are enough; the
// original HTML is always kept in the JSON.
func htmlToMarkdown(s string) string {
	if s == "" {
		return ""
	}
	s = anchor.ReplaceAllStringFunc(s, func(m string) string {
		sub := anchor.FindStringSubmatch(m)
		href, inner := sub[1], sub[2]
		text := strings.TrimSpace(tag.ReplaceAllString(inner, ""))
		switch {
		case invisible.MatchString(inner):
			// A link Mastodon shortened for display: show the whole address.
			return href
		case strings.HasPrefix(text, "@"), strings.HasPrefix(text, "#"):
			return "[" + text + "](" + href + ")"
		case text == "" || text == href || urlLike.MatchString(text) && strings.Contains(href, strings.TrimPrefix(strings.TrimPrefix(text, "https://"), "http://")):
			return href
		default:
			return "[" + text + "](" + href + ")"
		}
	})
	s = codeBlocks.ReplaceAllString(s, "\n\n```\n$1\n```\n\n")
	s = inlineCode.ReplaceAllString(s, "`$1`")
	s = bold.ReplaceAllString(s, "**$2**")
	s = italic.ReplaceAllString(s, "_${2}_")
	s = quoteBlock.ReplaceAllStringFunc(s, func(m string) string {
		inner := plugin.HTMLToText(quoteBlock.FindStringSubmatch(m)[1])
		return "\n\n> " + strings.ReplaceAll(inner, "\n", "\n> ") + "\n\n"
	})
	// HTMLToText strips the remaining tags, keeps the paragraphs and decodes
	// the entities once, after the links were rewritten.
	return plugin.HTMLToText(s)
}
