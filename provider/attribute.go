package provider

import (
	"context"
	"slices"

	aerr "github.com/frankbardon/aperture/errors"
)

// The attribute seam.
//
// An ObjectProvider answers "what do you know about THIS OBJECT?". An
// AttributeProvider answers a different question — "what do you know about the
// party ASKING?" — and the difference is not cosmetic. Object metadata is
// resolved per object, once per object in a decision. An attribute bag is
// resolved once per DECISION and then read by every rule against every object in
// it, so the two sit on opposite sides of the fan-out.
//
// Everything about the value the two seams carry is shared, and deliberately so:
// an attribute bag is a Metadata, validated by the same ValidateMetadata
// (metadata.go), holding the same legal shapes, honouring the same depth and
// size caps, spelling dates in the same two canonical forms (date.go), and
// filtered by the same MatchFields predicate (match.go). There is exactly one
// value model in Aperture, and an attribute bag is a value in it. A second model
// would be a second set of shapes the expression evaluator has to survive, and a
// second place for the two to drift.
//
// # The read-only-transitive contract, and why it bites harder here
//
// The package doc's read-only rule applies unchanged: the cache stores a
// provider's map by reference and never copies it on read, so writing through a
// returned Metadata at ANY depth races every other reader. What changes is the
// BLAST RADIUS. An object's metadata is read by the rules evaluating against
// that one object; a mutation through it corrupts one object's view. An
// attribute bag is the principal's — or the account's — bag for the WHOLE
// decision, shared across every object being checked, and (because it is cached
// per slot, keyed by principal id) across every concurrent decision for that
// principal. A single write through an attribute bag is therefore not one bad
// object; it is every rule, for every object, for every in-flight decision that
// principal has. So:
//
//   - an AttributeProvider returns a FRESH map per key, with fresh nested
//     containers, and never hands out a value it also retains and mutates;
//   - no holder — engine, rules, scope, CLI, server, host code — writes into an
//     attribute bag it was given, at any depth;
//   - a consumer that must modify one deep-copies it first.
//
// # Three slots, and no fourth
//
// The attribute registry has a CLOSED set of slots — user, machine, account —
// rather than an open type-keyed map like the object Registry. The object
// registry is open because the host's object types are the host's business and
// Aperture cannot know them. The attribute slots are not: they are the parties a
// decision has, and a decision has exactly these. An open map would invite a
// host to register a fourth "kind" of subject that nothing in the engine knows
// how to fetch, discovered as an empty bag at evaluation time — that is, as a
// silent denial.
//
// The slot keys are opaque string constants declared HERE rather than
// model.PrincipalKind values, because provider imports only identity and errors
// and never model. The closed set is enforced in this package, by these
// constants, and a caller holding a kind string crosses over through
// ParseAttributeSlot.

// AttributeSlot names one of the three parties a decision can carry attributes
// for. It is a defined string type so a slot is never confused with a fetch key
// or an object-type at a call site; the closed set is AttributeSlots(), and any
// other value is APERTURE_ATTRIBUTE_SLOT_UNKNOWN wherever it is presented.
type AttributeSlot string

const (
	// AttributeSlotUser holds attributes for a human principal — the department,
	// clearance, employment status a rule reads off `principal`.
	AttributeSlotUser AttributeSlot = "user"
	// AttributeSlotMachine holds attributes for a non-human principal: a service
	// account, an API client, a job runner. It is a separate slot rather than a
	// field on the user slot because the two are served by different systems in
	// every host that has both, and one provider forced to answer for both would
	// have to invent an empty bag for the half it does not know.
	AttributeSlotMachine AttributeSlot = "machine"
	// AttributeSlotAccount holds attributes for the ACCOUNT a decision is made
	// in — the tenant's plan, region, feature flags — not for any principal in
	// it. It is the bag behind the `account` rule root.
	AttributeSlotAccount AttributeSlot = "account"
)

// attributeWildcardKey is the one string that is never a legal attribute fetch
// key. It is model.AccountWildcard spelled as a literal, because provider may
// not import model.
//
// A wildcard reaching an attribute fetch would mean "the attributes of every
// account", and the only bag that could satisfy it is one account's data served
// as another's. It is refused at the seam, once, rather than at each of the
// callers that could hold one — an error here becomes a denial upstream, which
// is the safe direction, whereas a bag would become an allow.
const attributeWildcardKey = "*"

// AttributeSlots returns the closed set of slots, in a stable order. It is the
// definition of "every slot" for a caller enumerating them (a CLI listing what
// is wired, a test asserting the set has not grown).
func AttributeSlots() []AttributeSlot {
	return []AttributeSlot{AttributeSlotUser, AttributeSlotMachine, AttributeSlotAccount}
}

// String renders the slot as its bare key ("user", "machine", "account").
func (s AttributeSlot) String() string { return string(s) }

// Valid reports whether s is one of the three declared slots.
func (s AttributeSlot) Valid() bool {
	return slices.Contains(AttributeSlots(), s)
}

// ParseAttributeSlot converts a bare string — typically a principal kind that
// arrived from a layer this package cannot import — into a slot, or returns
// APERTURE_ATTRIBUTE_SLOT_UNKNOWN naming the closed set.
//
// It is the one crossing point between "a kind, as a string, from elsewhere" and
// "a slot this registry serves", so an unknown kind fails where it is converted
// rather than silently resolving to no provider and then to an empty bag.
func ParseAttributeSlot(s string) (AttributeSlot, error) {
	slot := AttributeSlot(s)
	if !slot.Valid() {
		return "", aerr.WithContext(aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			"provider: not an attribute slot",
			map[string]any{"slot": s, "slots": slotNames()})
	}
	return slot, nil
}

// slotNames renders the closed set for an error context.
func slotNames() []string {
	slots := AttributeSlots()
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, string(s))
	}
	return out
}

// AttributeRecord pairs one attribute KEY with its bag. It is the attribute-seam
// counterpart of Object, and the difference is the key's type: an Object is
// keyed by identity.Identity, an AttributeRecord by a bare string.
//
// That is not an omission. An object identity is a structured, segmented path
// (account:acme/project:atlas/document:42) precisely so a scope can contain and
// pattern-match it. An attribute key is a principal id or an account id — an
// opaque handle into the host's directory — with no segment structure to match
// and no containment relation to anything. Giving it an identity.Identity would
// spell a hierarchy that does not exist, and would hand this type the shape that
// makes a scope resolver want to enumerate it.
type AttributeRecord struct {
	// ID is the attribute key: the principal id, machine id, or account id the
	// bag belongs to. It is the host's own handle, and Aperture never parses it.
	ID string
	// Attributes is the host-defined bag for ID, in the shared value model.
	// Read-only once returned, transitively — see the file doc.
	Attributes Metadata
}

// AttributeFilter is the criteria an AttributeProvider.Query selects on. Every
// field is optional; the zero AttributeFilter selects everything (equivalent to
// List). The provider evaluates Fields; the AttributeRegistry re-enforces both
// Fields and Limit on the results it returns, so a provider that ignores them is
// still correct, only less efficient.
//
// # Why there is no Pattern
//
// Filter has a Pattern, and its absence here is the point of the type rather
// than an unfinished edge. Two reasons, and either alone is sufficient:
//
//   - There is nothing to match. identity.Pattern matches SEGMENTED identities;
//     an attribute key is a bare opaque string with no segments (see
//     AttributeRecord). A pattern over it could only ever be a substring test
//     dressed up as containment.
//   - Pattern exists on Filter for exactly one purpose: to bound an enumeration
//     to a GRANT'S SCOPE, so implicit and exclusive scope resolvers can ask "the
//     objects of this type, within this scope" through the registry. Attribute
//     enumeration is a system-tier admin read and is never a scope-resolution
//     source — the principal table is not an enumerable object set. Admitting a
//     Pattern here would give this type the one field that makes it look like
//     the seam a scope resolver wants, and shapes get wired to what they look
//     like.
//
// # The Fields contract
//
// Fields means here exactly what it means on Filter, down to the sentence: every
// predicate must hold (the map is an AND); an empty or nil map selects
// everything; a field ABSENT from a bag never matches, not even against a nil
// want; a COLLECTION field matches by MEMBERSHIP; every other field — scalar or
// object — matches by typed EQUALITY, so "5" never equals 5. It is not restated
// as a second rule: MatchFields (match.go) is the implementation both seams
// call, and an implementation that pushes the predicate into storage instead
// owes callers that same answer.
type AttributeFilter struct {
	// Fields are attribute predicates: membership for a collection field,
	// equality for everything else. Passed to the provider untouched;
	// MatchFields is the shared implementation.
	Fields map[string]any
	// Limit bounds the number of results; <= 0 means the provider's own default.
	// The registry re-enforces it on what comes back, honouring a positive value
	// verbatim and substituting DefaultListLimit for a non-positive one.
	Limit int
}

// AttributeProvider is the host-implemented pull source for ONE attribute slot.
// A provider is registered under a slot in an AttributeRegistry and consulted on
// demand; it must be safe for concurrent use.
//
// Implementations return APERTURE_NOT_FOUND (from errors/) for a Fetch of a key
// the host does not know, so the registry — and the consumers above it — can
// tell "there is no such principal" from "the directory is unreachable". The two
// mean opposite things for a decision, and collapsing them is how an outage
// becomes an authorization change. Any error already carrying an APERTURE_* code
// is surfaced verbatim; a plain error is wrapped as
// APERTURE_ATTRIBUTE_PROVIDER_FETCH.
//
// The bag a provider returns is READ-ONLY to every holder, transitively, and the
// provider must not retain and later mutate what it handed out — the blast
// radius note at the top of this file is the reason that matters more here than
// it does for an object.
type AttributeProvider interface {
	// Fetch returns the attribute bag for a bare key: a principal id for the
	// user and machine slots, an account id for the account slot. An unknown key
	// yields an APERTURE_NOT_FOUND coded error.
	Fetch(ctx context.Context, id string) (Metadata, error)
	// List returns every record this provider serves. It is the unfiltered
	// enumeration — the system-tier admin read — and a directory of any size
	// should be read through Query instead.
	List(ctx context.Context) ([]AttributeRecord, error)
	// Query returns the records satisfying filter, per the Fields contract on
	// AttributeFilter.
	Query(ctx context.Context, filter AttributeFilter) ([]AttributeRecord, error)
}

// attributeKeyError rejects a fetch key that can never name one subject: an
// empty string, or the account wildcard. It returns nil for a usable key.
func attributeKeyError(slot AttributeSlot, id string) error {
	switch id {
	case "":
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: attribute key is empty",
			map[string]any{"slot": string(slot)})
	case attributeWildcardKey:
		return aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			"provider: the account wildcard is not an attribute key",
			map[string]any{"slot": string(slot), "key": attributeWildcardKey})
	}
	return nil
}

// attributeError normalises an error returned by a host attribute provider. An
// error already carrying an APERTURE_* code passes through VERBATIM — so a
// provider's APERTURE_NOT_FOUND for an unknown key reaches the caller intact,
// and its registry fixups with it — while a plain error is wrapped as
// APERTURE_ATTRIBUTE_PROVIDER_FETCH.
//
// The guard is written out here rather than left to Wrap: Wrap RE-STAMPS, and a
// wrapped APERTURE_NOT_FOUND would read to every caller as an operational fetch
// failure, which is the one distinction this seam exists to preserve.
func attributeError(err error) error {
	if err == nil {
		return nil
	}
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
		"provider: attribute source returned an error", err)
}
