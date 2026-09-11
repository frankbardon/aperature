package docsgate

import (
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Where the book is
// ---------------------------------------------------------------------------

// bookSrc is the mdBook source root, repo-relative. Everything below it is a
// page this gate governs — there is no registry of chapters to keep in step,
// because a registry is exactly the thing a new page can be forgotten out of.
const bookSrc = "docs/src"

// summaryFile is mdBook's table of contents. A page it does not list is not in
// the built book at all.
const summaryFile = "SUMMARY.md"

// The floors are the anti-vacuity checks, and they are the reason this gate
// cannot pass by doing nothing. A link checker that finds no links passes; a
// walk that finds two chapters passes; an anchor derivation that returns the
// empty string for everything would ALSO pass, if nothing insisted that
// fragments were actually resolved. So the gate counts what it did and fails if
// the count collapsed.
//
// They are floors, not targets. The book had 53 chapters, 503 intra-book links
// and 148 of them carrying a fragment when the gate was written; these sit below
// that with room for the book to be pruned. Lower one in the same commit that
// legitimately shrinks the book — never to make a red build green.
const (
	pageFloor     = 50
	linkFloor     = 400
	fragmentFloor = 100
)

// ---------------------------------------------------------------------------
// The rule
// ---------------------------------------------------------------------------

// problem is one broken link, carrying everything a maintainer needs to fix it
// without grepping: which page, which line, which destination, and what is
// actually wrong with it.
type problem struct {
	file string
	line int
	dest string
	why  string
}

func (p problem) String() string {
	return p.file + ":" + strconv.Itoa(p.line) + ": " + p.dest + "\n      " + p.why
}

// externalScheme reports whether a destination is somebody else's problem.
//
// The skip is by SCHEME and never by allowlist. A test suite that reaches the
// network is slow, flaky, and fails for reasons unrelated to the change under
// test; whether https://example.com still resolves is a different job with a
// different failure mode, and it is not one `make test` may have.
func externalScheme(dest string) bool {
	for _, s := range []string{"http://", "https://", "mailto:", "ftp://", "tel:", "data:"} {
		if strings.HasPrefix(strings.ToLower(dest), s) {
			return true
		}
	}
	return false
}

// tally is what the gate did, so it can prove it did something. See the floors.
type tally struct {
	pages     int
	links     int // relative links resolved (external ones are not counted)
	fragments int // of those, how many carried an anchor that had to resolve
	external  int // skipped by scheme, never fetched
}

// checkBook is the gate as a pure function over a filesystem: hand it the book
// source tree and it returns every broken link plus a tally of what it checked.
//
// Pure and fs.FS-shaped on purpose. It is what lets book_links_fixtures_test.go
// prove the gate BITES against synthetic books without ever breaking a real
// page to watch it fail — the same split internal/schemagate uses.
func checkBook(fsys fs.FS) (probs []problem, n tally, err error) {
	sources := map[string]string{}
	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sources[p] = string(b)
		return nil
	})
	if walkErr != nil {
		return nil, tally{}, walkErr
	}
	n.pages = len(sources)

	// Anchors are derived once per page and reused, so a page linked to a
	// hundred times is parsed once.
	anchors := map[string]map[string]bool{}
	anchorsOf := func(p string) map[string]bool {
		if a, ok := anchors[p]; ok {
			return a
		}
		a := anchorSet(sources[p])
		anchors[p] = a
		return a
	}

	files := make([]string, 0, len(sources))
	for p := range sources {
		files = append(files, p)
	}
	sort.Strings(files)

	for _, from := range files {
		for _, l := range linksIn(sources[from]) {
			dest := strings.TrimSpace(l.dest)
			dest = strings.TrimPrefix(dest, "<")
			dest = strings.TrimSuffix(dest, ">")
			if dest == "" {
				continue
			}
			if externalScheme(dest) {
				n.external++
				continue
			}
			n.links++

			target, frag := dest, ""
			if i := strings.IndexByte(dest, '#'); i >= 0 {
				target, frag = dest[:i], dest[i+1:]
			}
			if decoded, derr := url.PathUnescape(target); derr == nil {
				target = decoded
			}
			if decoded, derr := url.PathUnescape(frag); derr == nil {
				frag = decoded
			}

			// Resolve the target page. An empty target is a same-page anchor.
			resolved := from
			if target != "" {
				resolved = path.Clean(path.Join(path.Dir(from), target))
				if !fs.ValidPath(resolved) || resolved == ".." || strings.HasPrefix(resolved, "../") {
					probs = append(probs, problem{from, l.line, dest,
						"escapes the book root: mdBook serves nothing above " + bookSrc + ", so this is a 404 with extra steps"})
					continue
				}
				if _, ok := sources[resolved]; !ok {
					if _, serr := fs.Stat(fsys, resolved); serr != nil {
						probs = append(probs, problem{from, l.line, dest,
							"no such file in the book: " + resolved})
						continue
					}
					// A non-markdown asset that exists. Nothing to anchor into.
					if frag != "" {
						probs = append(probs, problem{from, l.line, dest,
							resolved + " is not a markdown page, so it has no heading anchors"})
					}
					continue
				}
			}

			if frag == "" {
				continue
			}
			n.fragments++
			if have := anchorsOf(resolved); !have[frag] {
				probs = append(probs, problem{from, l.line, dest,
					"no heading in " + resolved + " derives the anchor #" + frag + nearMiss(frag, have)})
			}
		}
	}
	return probs, n, nil
}

// nearMiss offers the anchor the author probably meant. The motivating bug was
// a hyphen count — #the-acting-principal-principal for
// #the-acting-principal---principal — and a failure that only says "not found"
// makes the reader re-derive the rule by hand to see the difference.
func nearMiss(frag string, have map[string]bool) string {
	squash := func(s string) string {
		var b strings.Builder
		prev := false
		for _, r := range s {
			if r == '-' {
				if prev {
					continue
				}
				prev = true
			} else {
				prev = false
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	want := squash(frag)
	var near []string
	for a := range have {
		if squash(a) == want {
			near = append(near, a)
		}
	}
	sort.Strings(near)
	if len(near) == 0 {
		return ""
	}
	return "\n      did you mean #" + strings.Join(near, " or #") +
		"? mdBook KEEPS the hyphens it derives and never collapses a run of them"
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// TestEveryBookLinkResolves is the gate. It runs in `make test` — it is fast,
// offline and deterministic, so there is nothing to gate it behind. A link
// convention nobody is forced to keep is a convention that is already gone.
func TestEveryBookLinkResolves(t *testing.T) {
	fsys := bookFS(t)

	probs, n, err := checkBook(fsys)
	if err != nil {
		t.Fatalf("reading %s: %v", bookSrc, err)
	}
	switch {
	case n.pages < pageFloor:
		t.Fatalf("read only %d markdown pages under %s, expected at least %d.\n"+
			"Either the book shrank (lower pageFloor in the same commit) or this gate lost the tree and is checking nothing.",
			n.pages, bookSrc, pageFloor)
	case n.links < linkFloor:
		t.Fatalf("resolved only %d intra-book links, expected at least %d.\n"+
			"The reader has stopped finding links; this gate is passing by checking nothing.",
			n.links, linkFloor)
	case n.fragments < fragmentFloor:
		t.Fatalf("resolved only %d link fragments, expected at least %d.\n"+
			"The anchor half of this gate is the half that caught the real bug. If it is no longer\n"+
			"running, the gate has quietly become a file-existence check.",
			n.fragments, fragmentFloor)
	}
	if len(probs) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("the book has ")
	b.WriteString(strconv.Itoa(len(probs)))
	b.WriteString(" broken intra-book link(s):\n\n")
	for _, p := range probs {
		b.WriteString("  " + bookSrc + "/" + p.String() + "\n\n")
	}
	b.WriteString("Fix the link or fix the heading. Do NOT add an exception list — a gate\n" +
		"shipped with links that are allowed to be broken is not a gate.\n" +
		"If the page is docs/src/reference/cli.md or error-codes.md, fix the GENERATOR\n" +
		"under internal/docsgen and re-run `make docs-gen`; both files say DO NOT EDIT\n" +
		"and mean it.")
	t.Error(b.String())
}

// TestEveryBookPageIsInTheSummary is the both-directions half, and it is here
// for the reason TestEveryDialectSchemaIsGoverned exists: a gate that governs
// whichever files somebody remembered to list has a hole in it.
//
// A page on disk that SUMMARY.md never lists is not in the built book — mdBook
// will not render it, no reader can reach it, and a link INTO it resolves here
// while 404-ing in the browser. An entry in SUMMARY.md with no file behind it is
// the same failure wearing the other hat.
func TestEveryBookPageIsInTheSummary(t *testing.T) {
	fsys := bookFS(t)

	summary, err := fs.ReadFile(fsys, summaryFile)
	if err != nil {
		t.Fatalf("reading %s/%s: %v\nFail, never skip: if the table of contents moved, move this gate with it.", bookSrc, summaryFile, err)
	}

	listed := map[string]bool{}
	for _, l := range linksIn(string(summary)) {
		dest := l.dest
		if externalScheme(dest) {
			continue
		}
		if i := strings.IndexByte(dest, '#'); i >= 0 {
			dest = dest[:i]
		}
		if dest == "" || !strings.HasSuffix(dest, ".md") {
			continue
		}
		listed[path.Clean(dest)] = true
	}

	onDisk := map[string]bool{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") && p != summaryFile {
			onDisk[p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", bookSrc, err)
	}
	if len(onDisk) < pageFloor {
		t.Fatalf("found only %d chapters under %s, expected at least %d; this gate has lost the tree", len(onDisk), bookSrc, pageFloor)
	}

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

	if len(unlisted) > 0 {
		t.Errorf("these pages exist under %s but %s never lists them:\n  %s\n\n"+
			"mdBook builds the SUMMARY, not the directory: an unlisted page is not in the book,\n"+
			"so no reader can reach it and a link into it 404s in the browser while passing here.\n"+
			"Add it to %s in the same commit that adds the page.",
			bookSrc, summaryFile, strings.Join(unlisted, "\n  "), summaryFile)
	}
	if len(missing) > 0 {
		t.Errorf("%s lists these chapters but they are not on disk:\n  %s\n\n"+
			"Fail, never skip. If a page moved, move its SUMMARY entry with it.",
			summaryFile, strings.Join(missing, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// Locating the book
// ---------------------------------------------------------------------------

// bookFS opens the book source as a filesystem rooted at docs/src. It FAILS if
// the tree is unreadable — never skips — matching internal/schemagate and
// rules/editor_js_contract_test.go. A gate that goes quiet when its input moves
// is worse than no gate, because it also reports that it ran.
func bookFS(t *testing.T) fs.FS {
	t.Helper()
	dir := filepath.Join(repoRoot(t), filepath.FromSlash(bookSrc))
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("cannot read the book source at %s: %v\nFail, never skip: if docs/src moved, move this gate with it.", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory; this gate cannot read the book", dir)
	}
	return os.DirFS(dir)
}

// repoRoot walks up from the test's working directory to the module root, so
// this package can name the book by a repo-relative path and be moved without
// rewriting it. Not finding it is a failure, never a skip.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.Contains(string(b), "module github.com/frankbardon/aperture") {
				return dir
			}
			t.Fatalf("found a go.mod at %s that is not Aperture's; the gate cannot locate the book", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding Aperture's go.mod; the gate cannot locate the book")
		}
		dir = parent
	}
}
