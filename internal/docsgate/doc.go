// Package docsgate holds the gates that keep the mdBook under docs/src honest.
//
// There is one today: the LINK gate (book_links_test.go), which walks every
// markdown file in the book and insists that every intra-book link resolves —
// both the file it names and the heading anchor it jumps to.
//
// The gate, the tiny markdown reader it is built on, and the fixtures that prove
// it bites all live in this package's _test.go files. Nothing in the shipping
// binary calls them. This file exists so the package has a home for the prose,
// and so `go build ./...` has something to build. That is the same shape as
// internal/schemagate, for the same reason.
//
// # Why it exists
//
// A link in the book has exactly two ways to be wrong, and neither of them is
// visible to a reader who did not already know the right answer:
//
//  1. The FILE is gone or was never there. mdBook renders the link anyway and
//     the reader gets a 404.
//  2. The file is right and the ANCHOR is wrong. This one is worse, because the
//     browser silently drops the reader at the TOP of the page. The link looks
//     like it worked. Nothing — not mdBook, not CI, not review — says a word.
//
// Case 2 is not hypothetical here. Two links in this book pointed at
//
//	cli/global-options.md#the-acting-principal-principal
//
// for an anchor mdBook does not emit. The heading is
//
//	## The acting principal: `--principal`
//
// and mdBook keeps the flag's leading dashes, so the anchor it actually writes
// is `#the-acting-principal---principal` — THREE hyphens, one from the space
// before the flag and two from the flag itself. Both links landed at the top of
// the page for as long as they existed, and they were found by accident while
// somebody was reading the page for an unrelated reason.
//
// The rot compounds. Aperture's Update-Demand rows now oblige one document to
// point at a named section of another, so a book whose cross-document links are
// unverified is a convention enforced by links that may not go anywhere.
//
// # The crux: mdBook's anchors are DERIVED, never listed
//
// The one thing this gate must get right is how mdBook turns a heading into an
// id, and the only honest way to know that is to reproduce mdBook's own rule
// rather than a plausible-sounding approximation of it. mdBook normalizes a
// heading by keeping alphanumerics, '_' and '-', ASCII-lowercasing letters,
// turning every whitespace run's characters into hyphens, and DROPPING
// everything else — punctuation, backticks, colons, parentheses, ampersands.
//
// Dropping rather than collapsing is the whole story. It means:
//
//   - "The acting principal: `--principal`" -> the-acting-principal---principal
//     (the colon and backticks vanish, the space becomes one hyphen, and the
//     flag's two dashes SURVIVE because '-' is a kept character)
//   - "Build, test & lint gates"            -> build-test--lint-gates
//   - "`ObjectProvider`: the host seam"     -> objectprovider-the-host-seam
//   - "The `Filter.Fields` contract"        -> the-filterfields-contract
//
// A rule that "replaces punctuation with a hyphen" or "collapses runs of
// hyphens" gets every one of those wrong, and — this is the trap — gets the
// SIMPLE headings right, so it looks correct on a sample and quietly disagrees
// with mdBook exactly where the bugs live.
//
// The derivation in this package was not reasoned about and then trusted. It was
// checked against the book mdBook actually built: `make docs` writes docs/book,
// whose <h1>..<h6> tags carry the real id= attributes, and the derivation
// reproduced all 488 of them across all 53 chapters before this gate shipped.
// docs/book is a gitignored build artifact, so that comparison cannot be a
// standing test — but TestTheAnchorDerivationMatchesMdBook pins the cases that
// verification covered, spelled as literals, so a "simplification" of the rule
// is build-red rather than silently wrong.
//
// # What the gate checks
//
//   - Every relative link target exists on disk, resolved from the linking
//     file's own directory, and stays inside docs/src (mdBook serves nothing
//     above the book root, so a link that escapes it is a 404 with extra steps).
//   - Every fragment resolves to an anchor the target page actually emits,
//     including same-page "#..." links.
//   - SUMMARY.md and the tree agree in BOTH directions: a page on disk that
//     SUMMARY.md never lists is unreachable in the built book, and an entry in
//     SUMMARY.md with no file behind it is a broken chapter. This is the
//     TestEveryDialectSchemaIsGoverned instinct — a new page must not be able to
//     arrive unchecked — and it needs no registry to maintain, because the gate
//     walks the tree rather than a list somebody has to remember to update.
//
// The GENERATED pages (docs/src/reference/cli.md, docs/src/reference/error-
// codes.md) are covered like any other, deliberately: they are written by
// internal/docsgen from live Go source, so a CLI description that grows a link
// or an error fixup that names a section can break the book without anybody
// editing markdown. If this gate fires on one of them, the fix is in the
// GENERATOR plus `make docs-gen` — never a hand-edit of the DO NOT EDIT file.
//
// # What it does not do
//
// It never touches the network. http://, https:// and mailto: links are skipped
// by SCHEME, not by an allowlist: a test suite that reaches the internet is
// slow, flaky, and fails for reasons that have nothing to do with the change
// under test. Whether an external URL still resolves is a different job with a
// different failure mode, and it does not belong in `make test`.
//
// It also does not render markdown. The reader in markdown_test.go knows about
// fenced code blocks, inline code spans, ATX headings, inline links, image
// links, reference definitions and HTML href attributes — enough to find every
// link and every heading in this book, and no more. A full CommonMark parser
// would be a dependency and a much larger thing to be wrong about.
//
// # Fail, never skip
//
// If docs/src cannot be read, or the glob matches nothing, the gate FAILS. So
// does a SUMMARY.md that has gone missing. A gate that skips when its input
// moves is worse than no gate, because it also reports that it ran. Same rule as
// rules/editor_js_contract_test.go and internal/schemagate.
//
// # If this gate fires
//
// Fix the link, or fix the heading. Do not add an exception list. A gate shipped
// with a list of links that are allowed to be broken is not a gate — it is a
// record of the moment somebody decided the book did not have to be correct.
package docsgate
