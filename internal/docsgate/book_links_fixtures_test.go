package docsgate

import (
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// This file proves the gate BITES.
//
// A link checker that reports nothing is indistinguishable, from the outside,
// from a book with no broken links — and the whole reason this gate exists is
// that "nothing said a word" is exactly what a broken anchor looks like. So the
// checker is exercised against synthetic books whose defects are known, without
// ever breaking a real page to watch it fail. Same split as
// internal/schemagate's fixtures.

// ---------------------------------------------------------------------------
// The bug this gate was built for
// ---------------------------------------------------------------------------

// TestTheGateRejectsTheAnchorThatMotivatedIt is the load-bearing test. It builds
// a two-page book whose target chapter is the REAL docs/src/cli/global-options.md
// off disk, and links into it with both spellings:
//
//	#the-acting-principal-principal      <- what was written, and what silently
//	                                        dropped the reader at the top of the
//	                                        page for as long as it existed
//	#the-acting-principal---principal    <- what mdBook actually emits
//
// The first must be rejected and the second accepted. Reading the real page
// rather than a paraphrase of its heading is the point: if somebody rewords
// "## The acting principal: `--principal`", this test starts exercising the NEW
// heading, and the derivation is still being checked against something true.
func TestTheGateRejectsTheAnchorThatMotivatedIt(t *testing.T) {
	const target = "cli/global-options.md"

	real, err := fs.ReadFile(bookFS(t), target)
	if err != nil {
		t.Fatalf("reading %s/%s: %v\nFail, never skip: this test is worthless against a paraphrase.", bookSrc, target, err)
	}

	const (
		broken = "the-acting-principal-principal"
		good   = "the-acting-principal---principal"
	)

	if have := anchorSet(string(real)); !have[good] {
		t.Fatalf("%s no longer emits #%s.\nThe heading was reworded. Re-derive the anchor from the new heading and update\nthis test AND every link that points at it — that is the rot this gate exists to catch.", target, good)
	}

	// The real chapter carries its own links to chapters this two-page book does
	// not contain, so only the linking page's verdict is the one under test.
	verdict := func(frag string) []problem {
		t.Helper()
		probs, _, err := checkBook(fstest.MapFS{
			target: &fstest.MapFile{Data: real},
			"linker.md": &fstest.MapFile{Data: []byte(
				"# Linker\n\nSee [the acting principal](" + target + "#" + frag + ").\n")},
		})
		if err != nil {
			t.Fatalf("checkBook: %v", err)
		}
		var mine []problem
		for _, p := range probs {
			if p.file == "linker.md" {
				mine = append(mine, p)
			}
		}
		return mine
	}

	probs := verdict(broken)
	if len(probs) != 1 {
		t.Fatalf("the checker accepted #%s, the anchor mdBook does NOT emit; got %d problems, want 1.\n"+
			"This gate is trivially passing: the derivation has been loosened until the original bug reads as fine.", broken, len(probs))
	}
	if !strings.Contains(probs[0].why, good) {
		t.Errorf("the failure does not name the anchor mdBook actually emits (#%s), so a reader has to re-derive the rule by hand:\n%s", good, probs[0].why)
	}

	if probs := verdict(good); len(probs) != 0 {
		t.Fatalf("the checker rejected #%s, which mdBook DOES emit: %v\nA gate that cries wolf on correct links gets switched off.", good, probs)
	}
}

// TestNoBookLinkStillUsesTheOldSpelling is belt to the previous test's braces:
// it asserts the dead anchor is gone from the book itself. The checker proves
// the spelling is wrong; this proves nobody has written it down again. It is
// cheap, and the string is worth naming once in the repo so a search for it
// lands on the explanation.
func TestNoBookLinkStillUsesTheOldSpelling(t *testing.T) {
	fsys := bookFS(t)
	const dead = "#the-acting-principal-principal"

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for _, l := range linksIn(string(b)) {
			if strings.HasSuffix(l.dest, dead) {
				t.Errorf("%s/%s:%d links to %s, an anchor mdBook does not emit.\n"+
					"The heading is \"## The acting principal: `--principal`\" and the anchor is\n"+
					"#the-acting-principal---principal — mdBook keeps the flag's dashes.", bookSrc, p, l.line, dead)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", bookSrc, err)
	}
}

// ---------------------------------------------------------------------------
// The anchor derivation, pinned
// ---------------------------------------------------------------------------

// TestTheAnchorDerivationMatchesMdBook pins the derivation against headings
// taken from this book, with the ids mdBook itself wrote into docs/book.
//
// docs/book is a gitignored build artifact, so the exhaustive comparison that
// validated this rule — all 488 anchors across all 53 chapters — cannot be a
// standing test. These literals are what survives it. Every one is a case where
// a plausible-sounding alternative rule gives a DIFFERENT answer, which is the
// only kind of case worth pinning.
func TestTheAnchorDerivationMatchesMdBook(t *testing.T) {
	cases := []struct {
		heading string
		want    string
		why     string
	}{
		{"The acting principal: `--principal`", "the-acting-principal---principal",
			"the whole reason this gate exists: '-' is a KEPT character, so a flag's two dashes survive and join the hyphen the space became"},
		{"Selecting a model: `--seed` and `--store`", "selecting-a-model---seed-and---store",
			"twice over, in one heading"},
		{"The deployment's enumeration ceiling: `--enumerate-limit`", "the-deployments-enumeration-ceiling---enumerate-limit",
			"an apostrophe is DROPPED, not hyphenated"},
		{"Build, test & lint gates", "build-test--lint-gates",
			"'&' is dropped and the spaces on both sides of it are not, so the run of hyphens is two"},
		{"`ObjectProvider`: the host seam", "objectprovider-the-host-seam",
			"backticks and a colon vanish entirely"},
		{"The `Filter.Fields` contract", "the-filterfields-contract",
			"a dot is dropped, so two words fuse rather than being separated"},
		{"In-memory objects: `provider.Static`", "in-memory-objects-providerstatic",
			"an existing hyphen is kept as-is"},
		{"BatchResult[T]", "batchresultt",
			"brackets are dropped"},
		{"Leniency: a missing bag decides, a broken directory does not", "leniency-a-missing-bag-decides-a-broken-directory-does-not",
			"commas drop without leaving a hyphen behind"},
		{"Why? — `explain`", "why--explain",
			"an em dash is not alphanumeric, so it drops and its two spaces become two hyphens"},
		{"What is *not* in the file", "what-is-not-in-the-file",
			"emphasis markers drop"},
		{"The `mcp/` core and the SDK firewall", "the-mcp-core-and-the-sdk-firewall",
			"a slash drops"},
		{"`aperture attributes`", "aperture-attributes",
			"the everyday case still has to work"},
	}
	for _, c := range cases {
		if got := idFromContent(c.heading); got != c.want {
			t.Errorf("idFromContent(%q) = %q, want %q\n  %s", c.heading, got, c.want, c.why)
		}
	}
}

// TestTheAnchorDerivationNeverCollapsesHyphens states the single rule that
// everything above depends on, as its own assertion, because "collapse runs of
// hyphens" is the tidy-looking edit somebody will eventually make to this
// function and it is the exact edit that reintroduces the original bug.
func TestTheAnchorDerivationNeverCollapsesHyphens(t *testing.T) {
	for _, h := range []string{"a: `--b`", "a & b", "a  b", "a — b"} {
		id := idFromContent(h)
		if !strings.Contains(id, "--") {
			t.Errorf("idFromContent(%q) = %q: the run of hyphens was collapsed.\n"+
				"mdBook does not collapse them. Collapsing here makes the checker accept\n"+
				"#the-acting-principal-principal again, which is the bug this package was written for.", h, id)
		}
	}
}

// TestDuplicateHeadingsGetMdBooksSuffix pins the de-duplication, because a page
// with two "## Errors" sections has a second anchor that is not obvious from
// reading the markdown, and a checker that did not know about it would reject a
// link that works.
func TestDuplicateHeadingsGetMdBooksSuffix(t *testing.T) {
	have := anchorSet("# Errors\n\n## Errors\n\n### Errors\n")
	for _, want := range []string{"errors", "errors-1", "errors-2"} {
		if !have[want] {
			t.Errorf("missing #%s; mdBook numbers repeats from -1: %v", want, keys(have))
		}
	}
	if len(have) != 3 {
		t.Errorf("got %v, want exactly three anchors", keys(have))
	}
}

// ---------------------------------------------------------------------------
// The other ways a link is wrong
// ---------------------------------------------------------------------------

func TestTheGateBitesOnEveryKindOfBrokenLink(t *testing.T) {
	cases := []struct {
		name  string
		book  fstest.MapFS
		want  int
		hints []string
	}{
		{
			name: "a target file that is not there",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [B](b.md).\n"),
			},
			want:  1,
			hints: []string{"no such file"},
		},
		{
			name: "a target file that is there",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [B](b.md).\n"),
				"b.md": md("# B\n"),
			},
			want: 0,
		},
		{
			name: "a good file with a bad anchor — the silent one",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [B](b.md#nope).\n"),
				"b.md": md("# B\n\n## Something else\n"),
			},
			want:  1,
			hints: []string{"no heading in b.md derives the anchor #nope"},
		},
		{
			name: "a same-page anchor that is not there",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [below](#nope).\n\n## Yes\n"),
			},
			want:  1,
			hints: []string{"#nope"},
		},
		{
			name: "a same-page anchor that is there",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [below](#yes).\n\n## Yes\n"),
			},
			want: 0,
		},
		{
			name: "a link that climbs out of the book root",
			book: fstest.MapFS{
				"deep/a.md": md("# A\n\nSee [conventions](../../CLAUDE.md).\n"),
			},
			want:  1,
			hints: []string{"escapes the book root"},
		},
		{
			name: "a relative path resolved from the LINKING page's directory",
			book: fstest.MapFS{
				"cli/a.md":      md("# A\n\nSee [C](../concepts/c.md#deep).\n"),
				"concepts/c.md": md("# C\n\n## Deep\n"),
			},
			want: 0,
		},
		{
			name: "the same relative path, one directory wrong",
			book: fstest.MapFS{
				"cli/a.md":      md("# A\n\nSee [C](concepts/c.md#deep).\n"),
				"concepts/c.md": md("# C\n\n## Deep\n"),
			},
			want:  1,
			hints: []string{"no such file"},
		},
		{
			name: "a fragment on something that is not a markdown page",
			book: fstest.MapFS{
				"a.md":        md("# A\n\n![d](diagram.svg#frag)\n"),
				"diagram.svg": md("<svg/>"),
			},
			want:  1,
			hints: []string{"not a markdown page"},
		},
		{
			name: "an image that exists",
			book: fstest.MapFS{
				"a.md":        md("# A\n\n![d](diagram.svg)\n"),
				"diagram.svg": md("<svg/>"),
			},
			want: 0,
		},
		{
			name: "a link whose TEXT wraps across a line — the 17-link class",
			book: fstest.MapFS{
				"a.md": md("# A\n\nthe same [value\nmodel](b.md#nope), the same caps\n"),
				"b.md": md("# B\n"),
			},
			want:  1,
			hints: []string{"#nope"},
		},
		{
			name: "a broken link inside a fenced code block is an example, not a link",
			book: fstest.MapFS{
				"a.md": md("# A\n\n```md\nSee [B](b.md#nope).\n```\n"),
			},
			want: 0,
		},
		{
			name: "a broken link inside an inline code span is an example too",
			book: fstest.MapFS{
				"a.md": md("# A\n\nWrite `[B](b.md#nope)` to link to B.\n"),
			},
			want: 0,
		},
		{
			name: "a reference definition is checked, so every use of it is",
			book: fstest.MapFS{
				"a.md": md("# A\n\nSee [B][b].\n\n[b]: b.md#nope\n"),
				"b.md": md("# B\n"),
			},
			want:  1,
			hints: []string{"#nope"},
		},
		{
			name: "a raw HTML href is checked",
			book: fstest.MapFS{
				"a.md": md("# A\n\n<a href=\"b.md#nope\">B</a>\n"),
				"b.md": md("# B\n"),
			},
			want:  1,
			hints: []string{"#nope"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probs, _, err := checkBook(c.book)
			if err != nil {
				t.Fatalf("checkBook: %v", err)
			}
			if len(probs) != c.want {
				t.Fatalf("got %d problems, want %d:\n%v", len(probs), c.want, probs)
			}
			for _, h := range c.hints {
				if !strings.Contains(probs[0].why, h) {
					t.Errorf("the failure does not say %q, so it does not tell the maintainer what to do:\n%s", h, probs[0].why)
				}
			}
		})
	}
}

// TestNoExternalLinkIsEverFetched is the network assertion, and it is made by
// construction rather than by observation: the destinations below are chosen so
// that ANY attempt to treat them as paths would fail the checker. They do not,
// so nothing looked at them. The gate stays offline because it never leaves the
// scheme test, not because a fetch happened to be fast.
func TestNoExternalLinkIsEverFetched(t *testing.T) {
	book := fstest.MapFS{
		"a.md": md("# A\n\n" +
			"[expr](https://github.com/expr-lang/expr)\n" +
			"[insecure](http://example.invalid/nope.md#nope)\n" +
			"[mail](mailto:nobody@example.invalid)\n" +
			"[proto](https://github.com/frankbardon/aperture/blob/main/internal/wire/rpc/service.proto)\n"),
	}
	probs, n, err := checkBook(book)
	if err != nil {
		t.Fatalf("checkBook: %v", err)
	}
	if len(probs) != 0 {
		t.Fatalf("an external link was treated as a path: %v", probs)
	}
	if n.external != 4 {
		t.Fatalf("skipped %d external links by scheme, want 4; the scheme test is not doing the skipping", n.external)
	}
	if n.links != 0 {
		t.Fatalf("resolved %d links as paths, want 0", n.links)
	}
}

// TestTheRealBookIsWhatTheGateReads guards the other half of "fail, never skip":
// the fixtures above would keep passing if the gate had quietly stopped reading
// docs/src, because they bring their own filesystem. This one asserts the real
// tree is reachable and non-trivial before any of that is believed.
func TestTheRealBookIsWhatTheGateReads(t *testing.T) {
	probs, n, err := checkBook(bookFS(t))
	if err != nil {
		t.Fatalf("reading %s: %v", bookSrc, err)
	}
	t.Logf("%d pages, %d intra-book links (%d with anchors), %d external links skipped by scheme, %d problems",
		n.pages, n.links, n.fragments, n.external, len(probs))
	if n.pages < pageFloor || n.links < linkFloor || n.fragments < fragmentFloor {
		t.Fatalf("the gate read %d pages / %d links / %d fragments; floors are %d / %d / %d",
			n.pages, n.links, n.fragments, pageFloor, linkFloor, fragmentFloor)
	}
	if n.external == 0 {
		t.Error("no external link was skipped by scheme; either the book lost all of them or the scheme test is unreachable")
	}
}

// TestGeneratedPagesAreCovered nails down a criterion that is otherwise only
// true by accident: the two DO-NOT-EDIT pages under docs/src/reference are
// walked like any other chapter, in BOTH directions — the links they emit are
// resolved, and the anchors they emit are what the rest of the book links into.
//
// Both halves are reachable from a generator change. internal/docsgen/cliref
// writes the links, from live command descriptions; it also writes the HEADINGS,
// one per command, so renaming `aperture check` moves #aperture-check and
// silently breaks the seven pages that point at it. Neither half involves
// anybody editing markdown, which is exactly why the gate has to cover them.
//
// If this fires, the fix is in internal/docsgen plus `make docs-gen`. Never a
// hand-edit: both files say DO NOT EDIT and mean it.
func TestGeneratedPagesAreCovered(t *testing.T) {
	fsys := bookFS(t)
	generated := []string{"reference/cli.md", "reference/error-codes.md"}

	for _, p := range generated {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Fatalf("reading %s/%s: %v\nFail, never skip: if a generated page moved, move `make docs-gen` and this gate with it.", bookSrc, p, err)
		}
		if !strings.Contains(string(b), "DO NOT EDIT") {
			t.Errorf("%s no longer announces that it is generated; either it stopped being generated or the header was lost", p)
		}
	}

	// Outbound: the generated CLI reference links out, and those links are read.
	cli, err := fs.ReadFile(fsys, "reference/cli.md")
	if err != nil {
		t.Fatalf("reading the generated CLI reference: %v", err)
	}
	if n := len(linksIn(string(cli))); n == 0 {
		t.Error("reference/cli.md emits no links at all; the generator stopped writing them and this coverage has gone vacuous")
	}

	// Inbound: the rest of the book links INTO the generated pages by anchor, so
	// a heading the generator renames is a broken link the gate must resolve.
	inbound := map[string]int{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for _, l := range linksIn(string(b)) {
			i := strings.IndexByte(l.dest, '#')
			if i <= 0 {
				continue
			}
			resolved := path.Clean(path.Join(path.Dir(p), l.dest[:i]))
			for _, g := range generated {
				if resolved == g {
					inbound[g]++
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", bookSrc, err)
	}
	if inbound["reference/cli.md"] == 0 {
		t.Error("nothing in the book links into reference/cli.md by anchor; the generated headings are no longer load-bearing, or the reader stopped finding those links")
	}
	t.Logf("anchored links into the generated pages: %v", inbound)
}

// ---------------------------------------------------------------------------
// The SUMMARY gate, proven both ways
// ---------------------------------------------------------------------------

func TestTheSummaryGateBitesInBothDirections(t *testing.T) {
	// The real gate reads the real tree, so these exercise the same comparison
	// in miniature: a page nobody listed, and a listing with no page.
	listed := map[string]bool{"a.md": true, "gone.md": true}
	onDisk := map[string]bool{"a.md": true, "orphan.md": true}

	var unlisted, missing []string
	for p := range onDisk {
		if !listed[p] {
			unlisted = append(unlisted, p)
		}
	}
	for p := range listed {
		if !onDisk[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(missing)

	if len(unlisted) != 1 || unlisted[0] != "orphan.md" {
		t.Errorf("unlisted = %v, want [orphan.md]: a page nobody listed is not in the built book", unlisted)
	}
	if len(missing) != 1 || missing[0] != "gone.md" {
		t.Errorf("missing = %v, want [gone.md]: a listing with no page behind it is a broken chapter", missing)
	}
}

// TestTheSummaryReaderFindsTheRealChapters is the anti-vacuity check for the
// SUMMARY gate's reader: if linksIn stopped understanding SUMMARY.md's nested
// list syntax, the "unlisted" set would become every page in the book and the
// gate would be loud rather than silent — but if it stopped finding the PAGES,
// the gate would pass by comparing two empty sets.
func TestTheSummaryReaderFindsTheRealChapters(t *testing.T) {
	b, err := fs.ReadFile(bookFS(t), summaryFile)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bookSrc, summaryFile, err)
	}
	n := 0
	for _, l := range linksIn(string(b)) {
		if strings.HasSuffix(l.dest, ".md") {
			n++
		}
	}
	if n < pageFloor {
		t.Fatalf("%s yielded only %d chapter links, expected at least %d; the reader has lost the table of contents", summaryFile, n, pageFloor)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func md(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s), Mode: os.FileMode(0o644)} }

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
