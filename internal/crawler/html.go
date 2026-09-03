package crawler

import (
	"strings"

	"golang.org/x/net/html"
)

// A handful of helpers over x/net/html, in place of a selector library.
//
// The resolver needs about five queries — anchors by class, anchors by href
// suffix, and the three embed elements — and a dependency that parses CSS
// selectors to serve them would be a lot of surface for that. These walk the
// tree directly and are what the resolver's tests exercise.

// attr returns the value of an attribute, and whether it was present. HTML
// attribute names are case-insensitive, and the parser has already lowercased
// them, but callers pass lowercase literals anyway.
func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

// hasClass reports whether n carries the given class.
//
// Matches whole space-separated tokens, not substrings: "jet-download" must not
// match the element whose class is "jet-download-icon-position-left", which is
// exactly the distinction the Ministry's markup turns on.
func hasClass(n *html.Node, want string) bool {
	classes, ok := attr(n, "class")
	if !ok {
		return false
	}
	for _, c := range strings.Fields(classes) {
		if c == want {
			return true
		}
	}
	return false
}

// walk visits every element node in document order, which is the order the
// resolver reports candidates in when several share a priority.
func walk(n *html.Node, visit func(*html.Node)) {
	if n.Type == html.ElementNode {
		visit(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

// elementsByTag collects every element with the given tag name.
func elementsByTag(root *html.Node, tag string) []*html.Node {
	var out []*html.Node
	walk(root, func(n *html.Node) {
		if n.Data == tag {
			out = append(out, n)
		}
	})
	return out
}

// textOf returns the concatenated text content of a node, whitespace-collapsed.
func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
