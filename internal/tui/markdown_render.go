package tui

import (
	"bytes"

	glamour "charm.land/glamour/v2/ansi"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	emoji "github.com/yuin/goldmark-emoji"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Glamour coalesces adjacent Chroma tokens. Plain logs have just one style,
// so tokenize the whole block at once instead of joining two tokens per line.
func init() {
	plain := lexers.Get("plaintext")
	lexers.Fallback = lexers.Register(chroma.MustNewLexer(plain.Config(), func() chroma.Rules {
		return chroma.Rules{"root": {{Pattern: "(?s).+", Type: chroma.Text}}}
	}))
}

func newMarkdownRenderer(style glamour.StyleConfig, width int) goldmark.Markdown {
	tableWrap := true
	ansi := glamour.NewRenderer(glamour.Options{Styles: style, WordWrap: width, TableWrap: &tableWrap})
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.DefinitionList, emoji.New()),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	md.SetRenderer(renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(ansi, 1000))))
	return md
}

func renderMarkdown(md goldmark.Markdown, source string) string {
	original := []byte(source)
	input := original[:len(original):len(original)]
	doc := md.Parser().Parse(text.NewReader(input))
	// Glamour v2.0.1 concatenates code one line at a time. Give it one
	// segment per multiline block so large tool output takes linear copying.
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (n.Kind() == ast.KindFencedCodeBlock || n.Kind() == ast.KindCodeBlock) {
			lines := n.Lines()
			if lines.Len() > 1 {
				start := len(input)
				for i := range lines.Len() {
					line := lines.At(i)
					input = append(input, line.Value(original)...)
				}
				lines.Clear()
				lines.Append(text.NewSegment(start, len(input)))
			}
		}
		return ast.WalkContinue, nil
	})
	var output bytes.Buffer
	if err := md.Renderer().Render(&output, input, doc); err != nil {
		return source
	}
	return output.String()
}
