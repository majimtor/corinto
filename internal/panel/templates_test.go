package panel

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template/parse"
)

// The words a template may hold: the name of the product, which no
// language translates.
var neutralWords = []string{"Corinto"}

// The texts the code asks for by a key it puts together or holds in a
// table, and that no template names: the tests of the catalogues have
// them.
var keysOfTheCode = []string{"format.", "month.", "link.state.", "language.name", "error."}

// The attributes of an element whose value a person reads or hears.
var attributesPeopleRead = regexp.MustCompile(`\s(alt|title|aria-label|aria-description|placeholder|label|summary|value)="([^"]*)"`)

var (
	tag  = regexp.MustCompile(`<[^>]*>`)
	word = regexp.MustCompile(`\p{L}+`)
)

// written is what a set of templates holds besides markup: the text
// between its actions, with a mark where each action was, and the calls
// that ask a catalogue for a text.
type written struct {
	skeleton strings.Builder
	calls    []call
	// strays are the texts written in an action that are not the key of a
	// text or the name of one of its values.
	strays []string
}

// call is a text asked of a catalogue: its key, when the template writes
// it, and the names of the values it gives.
type call struct {
	function string
	key      string
	literal  bool
	names    []string
}

const action = "\x00"

func (w *written) walk(node parse.Node) {
	switch n := node.(type) {
	case nil:
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				w.walk(child)
			}
		}
	case *parse.TextNode:
		w.skeleton.Write(n.Text)
	case *parse.ActionNode:
		w.skeleton.WriteString(action)
		w.pipe(n.Pipe)
	case *parse.IfNode:
		w.branch(&n.BranchNode)
	case *parse.RangeNode:
		w.branch(&n.BranchNode)
	case *parse.WithNode:
		w.branch(&n.BranchNode)
	case *parse.TemplateNode:
		w.skeleton.WriteString(action)
		w.pipe(n.Pipe)
	}
}

func (w *written) branch(n *parse.BranchNode) {
	w.pipe(n.Pipe)
	w.walk(n.List)
	w.walk(n.ElseList)
}

func (w *written) pipe(pipe *parse.PipeNode) {
	if pipe == nil {
		return
	}
	for _, command := range pipe.Cmds {
		first := 0
		if name, ok := command.Args[0].(*parse.IdentifierNode); ok && (name.Ident == "t" || name.Ident == "plural") && len(command.Args) > 1 {
			asked := call{function: name.Ident}
			if key, ok := command.Args[1].(*parse.StringNode); ok {
				asked.key, asked.literal = key.Text, true
			}
			// After the key come the pairs, a name and a value; a text
			// that counts takes its number before them.
			first = 2
			if name.Ident == "plural" {
				first = 3
			}
			for i := first; i < len(command.Args); i++ {
				if name, ok := command.Args[i].(*parse.StringNode); ok && (i-first)%2 == 0 {
					asked.names = append(asked.names, "{"+name.Text+"}")
				} else {
					w.argument(command.Args[i])
				}
			}
			w.calls = append(w.calls, asked)
			continue
		}
		for _, argument := range command.Args {
			w.argument(argument)
		}
	}
}

func (w *written) argument(node parse.Node) {
	switch n := node.(type) {
	case *parse.StringNode:
		w.strays = append(w.strays, n.Text)
	case *parse.PipeNode:
		w.pipe(n)
	}
}

// read parses every template of the panel and returns what they hold.
func read(t *testing.T) *written {
	t.Helper()
	files, err := templateFiles.ReadDir("templates")
	if err != nil || len(files) == 0 {
		t.Fatalf("the templates could not be listed: %v", err)
	}
	var found written
	for _, file := range files {
		text, err := templateFiles.ReadFile("templates/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		tree := parse.New(file.Name())
		tree.Mode = parse.SkipFuncCheck
		trees := make(map[string]*parse.Tree)
		if _, err := tree.Parse(string(text), "", "", trees); err != nil {
			t.Fatalf("%s does not parse: %v", file.Name(), err)
		}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range trees {
				if !yield(name) {
					return
				}
			}
		}) {
			found.walk(trees[name].Root)
			found.skeleton.WriteString("\n")
		}
	}
	return &found
}

// wordsOf returns the words of a text that no language-neutral list
// excuses.
func wordsOf(text string) []string {
	var words []string
	for _, found := range word.FindAllString(text, -1) {
		if !slices.Contains(neutralWords, found) {
			words = append(words, found)
		}
	}
	return words
}

// Templates carry no literal interface text (the engineering rules): not
// in an element, not in an attribute a person reads, and not in what an
// action writes. Every text comes from a catalogue, so every text exists
// in both languages.
func TestTheTemplatesHoldNoTextOfTheirOwn(t *testing.T) {
	found := read(t)
	skeleton := found.skeleton.String()
	if !strings.Contains(skeleton, "<html") || !strings.Contains(skeleton, action) {
		t.Fatal("the templates were not read: no page and no action in them")
	}
	if words := wordsOf(tag.ReplaceAllString(skeleton, " ")); len(words) > 0 {
		t.Errorf("the templates hold words of their own between their elements: %q", words)
	}
	for _, attribute := range attributesPeopleRead.FindAllStringSubmatch(skeleton, -1) {
		if words := wordsOf(attribute[2]); len(words) > 0 {
			t.Errorf("a template holds words of its own in the attribute %s: %q", attribute[1], words)
		}
	}
	if len(found.strays) > 0 {
		t.Errorf("a template writes texts of its own in its actions: %q", found.strays)
	}
}

// Every text a template asks for is in the catalogue, with the values the
// template gives it; and every text of the catalogue is asked for by a
// template, or is one of the few the code puts the key of together.
func TestTheTemplatesAndTheCatalogueAgree(t *testing.T) {
	texts, err := readCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	found := read(t)
	used := make(map[string]bool)
	literal := 0
	for _, asked := range found.calls {
		if !asked.literal {
			continue
		}
		literal++
		used[asked.key] = true
		text, ok := texts[spanish][asked.key]
		if !ok {
			t.Errorf("a template asks for %q, which the catalogue lacks", asked.key)
			continue
		}
		if text.counts != (asked.function == "plural") {
			t.Errorf("a template asks for %q with %s, and the text counts something: %t", asked.key, asked.function, text.counts)
		}
		given := slices.Clone(asked.names)
		if text.counts {
			given = append(given, "{n}")
		}
		slices.Sort(given)
		for _, form := range []string{text.one, text.other} {
			if want := placeholdersOf(form); form != "" && !slices.Equal(given, want) {
				t.Errorf("a template gives %q the values %v, and the text takes %v", asked.key, given, want)
			}
		}
	}
	if literal < 20 {
		t.Fatalf("only %d texts asked for by their key were found in the templates", literal)
	}
	for key := range texts[spanish] {
		if used[key] || slices.ContainsFunc(keysOfTheCode, func(prefix string) bool { return strings.HasPrefix(key, prefix) }) {
			continue
		}
		t.Errorf("the catalogue has %q, which no template asks for", key)
	}
}
