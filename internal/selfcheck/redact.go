/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package selfcheck

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Redactor turns identifiers into stable, per-report tokens.
//
// # Why a random salt and not a derived one
//
// The obvious implementation — hash the name, maybe with a fixed pepper — is broken, and broken in
// the way that matters. The value space is tiny and predictable: `production`, `staging`,
// `default`, `kube-system`, the fifty most common tenant names, the customer's own name. Any of
// those is recovered by hashing a dictionary and comparing, in seconds, by anyone who reads the
// issue. Deriving the salt from something in the cluster is the same defect wearing a hat, because
// the derivation is in this file and the inputs are in the cluster.
//
// That last sentence is now load-bearing rather than rhetorical: SaltNamespaceUID exists and
// derives exactly that way. It is not a reversal of this paragraph, it is the paragraph applied.
// The derived mode is for a SOAK ARCHIVE, whose product is a fortnight read as one series and
// which goes to a maintainer; against a stranger holding only the file, the namespace UID is 122
// bits they do not have, and against anyone who can read the namespace, the tokens fall to a
// dictionary in seconds. Describe() says that in those words, so the mode is chosen with its cost
// rather than instead of it — and the default for a one-shot `crystal-backup selfcheck`, the
// report that gets attached to a public issue, is still the random salt.
//
// So the salt is 32 bytes from crypto/rand, generated per report and thrown away with the process.
// The consequence is deliberate and is the whole design: correlation is preserved WITHIN a report
// (the same namespace is the same token in the inventory, in the leak samples and in every alert
// breach) and destroyed BETWEEN reports. Two reports from the same cluster share no tokens. Nobody,
// including the person who ran it, can reverse one afterwards.
//
// # And why there is nevertheless an opt-in stable salt
//
// That property is right for the case it was built for — one report, attached to one public issue.
// It is wrong for a soak. Fourteen daily self-checks with fourteen salts share no tokens at all, so
// nothing in them can be read as a series: a namespace that grew, a namespace that drifted, a
// namespace that stopped being backed up on day nine all look like fourteen unrelated strangers.
// The exercise measures change over time and the redaction destroys exactly that axis.
//
// --redaction-salt-file therefore lets an administrator supply the salt once, from a file, and the
// reasoning above is not weakened by it — it is accepted and paid for. A caller-supplied salt IS a
// stable pseudonym for as long as the file exists, and IS dictionary-reversible by whoever holds
// the file. Two things keep that honest rather than merely convenient. The salt never enters the
// report in any form (not the bytes, not a digest of them, not the path), so a reader who does not
// already have the file gains nothing. And Describe() says which of the two constructions produced
// the file in front of it, because "HMAC-SHA256 over a 32-byte random salt" printed over a fixed
// salt is a false provenance line, and a false provenance line on a redacted report is worse than
// no line at all.
//
// # What is never redacted, because it is never anyone's identity
//
// Phases, conditions, modes, cron expressions, image digests and tags, version strings, counts and
// timestamps. And what is never printed in ANY mode, --full included: repository passwords, S3
// credentials, KEK or DEK material, CA bundle contents. Those are not redacted — they are not read.
// spec/05-observability.md §4 states that discipline for the logs; a report attached to an issue is
// the same exposure with a longer half-life.
type Redactor struct {
	salt []byte
	full bool
	// saltSource records WHERE the salt came from — crypto/rand, a caller's file, or a derivation
	// from something in the cluster. It changes nothing about how a token is computed and
	// everything about what a token PROMISES, so it exists to be reported, not to be branched on.
	// It is a value rather than a bool because there are now three answers and they make three
	// different promises; a bool would have forced two of them to share a sentence.
	saltSource string
	// known is every identifier the collector has registered through Learn, sorted longest-first so
	// Detail's free-text substitution cannot corrupt a name that contains a shorter one.
	known []knownName
}

// tokenBytes is how much of the HMAC ends up in a token: 4 bytes, 8 hex characters.
//
// Long enough that a collision needs thousands of distinct names in ONE report (the birthday bound
// on 2^32 is around 9,000 for a 1% chance), which no real installation reaches; short enough that
// a page full of them stays readable, which matters because a reader has to visually match
// `ns-a3f2c1d9` against the same token six sections away.
const tokenBytes = 4

// MinSaltBytes is the shortest caller-supplied salt this package will accept.
//
// It is the length crypto/rand produces above, and the floor is not ceremony: the salt an
// administrator supplies is the one somebody eventually types by hand, and a typed salt is short,
// low-entropy and guessable — which would put the token space back inside dictionary range for a
// reader who has nothing but the report. Accepting a short one silently is the failure mode.
const MinSaltBytes = 32

// NewRedactor builds a redactor.
//
// full=true disables hashing entirely — for a report shared privately, with someone the operator
// has decided to trust with their namespace names.
//
// salt=nil is the default and the documented behaviour of this command: 32 fresh bytes from
// crypto/rand, per report. A non-nil salt is the caller's, must be at least MinSaltBytes long, and
// buys cross-report correlation at the cost of the guarantees the type comment sets out.
func NewRedactor(full bool, salt []byte) (*Redactor, error) {
	return NewRedactorWithSource(full, salt, SaltCallerSupplied)
}

// NewRedactorWithSource is NewRedactor for a caller that knows something more specific about
// where its salt came from than "the caller supplied it".
//
// The source is REPORTED, never branched on, and it is a parameter rather than something this
// package infers because it cannot be inferred: 32 bytes from a file and 32 bytes derived from a
// namespace UID are the same 32 bytes to everything here, and they make different promises to a
// reader. source is ignored when salt is nil (a random salt describes itself).
func NewRedactorWithSource(full bool, salt []byte, source string) (*Redactor, error) {
	if len(salt) > 0 {
		if source == "" {
			// A salt with no stated provenance would render as the random-salt note, which is the
			// one sentence that is certainly false about it.
			return nil, fmt.Errorf("a supplied redaction salt must name its source")
		}
		if full {
			// The type would otherwise hold a salt it can never use, and a caller who asked for both
			// has a wrong mental model of what they are about to get — see RunSelfcheck, which
			// catches this first with a message naming the two flags.
			return nil, fmt.Errorf("a redaction salt is meaningless in full mode: nothing is hashed")
		}
		if len(salt) < MinSaltBytes {
			return nil, fmt.Errorf("redaction salt is %d bytes, need at least %d", len(salt), MinSaltBytes)
		}
		// full is provably false here, and is carried anyway rather than left to the zero value: a
		// Redactor whose flags disagree with its constructor's arguments is a trap for whoever
		// relaxes the check above.
		return &Redactor{salt: salt, full: full, saltSource: source}, nil
	}
	salt = make([]byte, MinSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		// Failing closed is the only acceptable behaviour here. A redactor that quietly fell back
		// to a fixed salt would produce a report that LOOKS redacted and is reversible by anyone
		// who reads this file, which is worse than refusing to produce one at all.
		return nil, fmt.Errorf("generate redaction salt: %w", err)
	}
	return &Redactor{salt: salt, full: full}, nil
}

// LoadRedactionSalt reads a salt file, whole and verbatim.
//
// The file's bytes ARE the salt — no trimming, no hex decoding, no newline stripping. That is the
// contract because it is the only one that stays true when the file is copied between machines: a
// loader that quietly dropped a trailing newline would agree with one that did not on every file
// except the ones an administrator actually creates with a shell redirect.
//
// Every error here is fatal to the run by design. A missing or unreadable salt file must never
// degrade into "generate a random one instead": the report that came back would look exactly like a
// correlatable one, would be filed beside thirteen others, and the gap would only be noticed by the
// person who later cannot explain why day nine has no tokens in common with day eight.
func LoadRedactionSalt(path string) ([]byte, error) {
	salt, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path is the entire feature
	if err != nil {
		return nil, fmt.Errorf("read redaction salt file: %w", err)
	}
	if len(salt) < MinSaltBytes {
		return nil, fmt.Errorf("redaction salt file %s holds %d bytes, need at least %d: "+
			"generate one with `head -c 32 /dev/urandom > %s`", path, len(salt), MinSaltBytes, path)
	}
	return salt, nil
}

// Full reports whether identifiers are being passed through unredacted.
func (r *Redactor) Full() bool { return r.full }

// Describe is the Redaction block that goes into the report, so a reader never has to guess which
// mode produced the file in front of them.
func (r *Redactor) Describe() Redaction {
	if r.full {
		return Redaction{
			Mode:          ModeFull,
			SaltDisclosed: false,
			Note: "UNREDACTED. Namespace, tenant, PVC, location, bucket, endpoint and cluster " +
				"identifiers appear verbatim. Share privately only. Secrets, repository passwords, " +
				"S3 credentials and key material are still never included — they are not read.",
		}
	}
	if r.saltSource == SaltNamespaceUID {
		return Redaction{
			Mode:       ModeHashed,
			SaltSource: SaltNamespaceUID,
			Algorithm: "HMAC-SHA256 over SHA256(\"" + SaltNamespaceUIDDomain + "\" || the operator " +
				"namespace's UID), truncated to 8 hex characters, prefixed by kind",
			SaltDisclosed: false,
			Note: "Identifiers are replaced by tokens computed from a salt DERIVED FROM THIS " +
				"CLUSTER — the UID of the operator's own namespace — rather than from a random one. " +
				"That is deliberate and it is what makes a fortnight of reports readable as one " +
				"series: the same namespace is the same token here, in every other report from this " +
				"cluster, and in every stream of the same soak archive. Nothing generates the UID, so " +
				"nothing can regenerate it and break the series halfway through. WHAT IT PROTECTS " +
				"AGAINST, PLAINLY: against a stranger reading a public issue, the UID is 122 bits " +
				"they do not have and the tokens are opaque. Against ANYONE WHO CAN `get` THE " +
				"OPERATOR'S NAMESPACE — anyone with cluster read access, now or later — the tokens " +
				"are REVERSIBLE BY DICTIONARY in seconds, because namespace names come from a small " +
				"guessable set (`production`, `staging`, `default`, your customer's name). The salt " +
				"is not in this file in any form, but it does not have to be. Before a report leaves " +
				"the cluster, re-run it WITHOUT a fixed salt at all: the default is random per report " +
				"and leaves nothing to correlate and nothing to reverse.",
		}
	}
	if r.saltSource == SaltCallerSupplied {
		return Redaction{
			Mode:          ModeHashed,
			SaltSource:    SaltCallerSupplied,
			Algorithm:     "HMAC-SHA256 over a caller-supplied salt, truncated to 8 hex characters, prefixed by kind",
			SaltDisclosed: false,
			Note: "Identifiers are replaced by tokens computed from a salt SUPPLIED BY WHOEVER RAN " +
				"THIS (--redaction-salt-file), not the usual random one. The tokens are therefore " +
				"stable within this report AND ACROSS EVERY REPORT MADE WITH THE SAME SALT FILE — " +
				"which is the point when you are reading a fortnight of them as one series, and is " +
				"the risk everywhere else: anyone holding two such reports can follow a single " +
				"namespace from one to the other, and anyone holding the salt file can recover the " +
				"names outright, because namespace names come from a small guessable set. The salt " +
				"itself is not in this file, in any form. Before attaching this to a public issue, " +
				"re-run WITHOUT --redaction-salt-file: the default salt is random per report and " +
				"leaves nothing to correlate.",
		}
	}
	return Redaction{
		Mode:          ModeHashed,
		SaltSource:    SaltRandomPerReport,
		Algorithm:     "HMAC-SHA256 over a 32-byte random salt, truncated to 8 hex characters, prefixed by kind",
		SaltDisclosed: false,
		Note: "Identifiers are replaced by tokens that are STABLE WITHIN this report and " +
			"meaningless outside it: the same namespace is the same token everywhere here, and no " +
			"token can be reversed or matched against another report. The salt is random per " +
			"report and is not written into it. Re-run with --full for an unredacted copy to share " +
			"privately.",
	}
}

// The token kinds. A prefix per kind so a reader can tell what a token IS at a glance, and so two
// different kinds of object that happen to share a name do not share a token — `production` the
// namespace and `production` the location are different things and must not look like one.
const (
	kindNamespace = "ns"
	kindTenant    = "tenant"
	kindLocation  = "loc"
	kindSchedule  = "sched"
	kindSync      = "sync"
	kindPVC       = "pvc"
	kindBucket    = "bucket"
	kindHost      = "host"
	kindPrefix    = "prefix"
	kindCluster   = "cluster"
	kindSecret    = "secret"
	kindPod       = "pod"
	kindObject    = "obj"
	kindImage     = "img"
)

// token is the one construction every helper below funnels through, and it does two things rather
// than one.
//
// It computes the token, and it REGISTERS the value so Detail can substitute it out of free text.
// Those are deliberately not separable. A breach's Detail is the one field that carries an object
// name in a sentence rather than in a field, and the only way it can be guaranteed redacted is if
// every identifier the report tokenises anywhere is, by construction, also a substitution rule.
// Splitting the two would mean maintaining a list of "things to also register", and the first
// identifier anyone forgot to add to it would leak a customer name into a public issue.
//
// The registry also makes the token per-VALUE rather than per-(kind, value). That matters because
// the same name genuinely appears under two kinds: a BackupRepository is named after its location,
// so `loc-a3f2c1d9` in one table and `repo-77b1e4c2` in the next would be two names for one thing
// in a document whose entire remaining value is that its tokens correlate.
//
// The empty string maps to the empty string rather than to a token. That is not a shortcut: an
// absent label is meaningful in this system — the cluster repository's namespace label is empty,
// and so is `cluster` when no location claims a default — and hashing "" would turn every one of
// those absences into an identical fake identity that looks like a real one.
func (r *Redactor) token(kind, value string) string {
	if value == "" {
		return ""
	}
	if r.full {
		return value
	}
	for _, n := range r.known {
		if n.value == value {
			return n.token
		}
	}
	mac := hmac.New(sha256.New, r.salt)
	// The kind is part of the message, not just of the prefix, so two DIFFERENT names cannot
	// collide into one token merely by being hashed the same way.
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	tok := kind + "-" + hex.EncodeToString(mac.Sum(nil)[:tokenBytes])
	r.register(value, tok)
	return tok
}

func (r *Redactor) Namespace(s string) string { return r.token(kindNamespace, s) }
func (r *Redactor) Tenant(s string) string    { return r.token(kindTenant, s) }
func (r *Redactor) Location(s string) string  { return r.token(kindLocation, s) }
func (r *Redactor) Schedule(s string) string  { return r.token(kindSchedule, s) }
func (r *Redactor) Sync(s string) string      { return r.token(kindSync, s) }
func (r *Redactor) PVC(s string) string       { return r.token(kindPVC, s) }

// Host redacts a node name. internal/soak needs it: a mover pod's peak memory is only
// interpretable next to the node it ran on, and a node name identifies the cluster as surely as a
// namespace identifies a tenant.
func (r *Redactor) Host(s string) string      { return r.token(kindHost, s) }
func (r *Redactor) Bucket(s string) string    { return r.token(kindBucket, s) }
func (r *Redactor) Prefix(s string) string    { return r.token(kindPrefix, s) }
func (r *Redactor) ClusterID(s string) string { return r.token(kindCluster, s) }
func (r *Redactor) Secret(s string) string    { return r.token(kindSecret, s) }
func (r *Redactor) Pod(s string) string       { return r.token(kindPod, s) }
func (r *Redactor) Object(s string) string    { return r.token(kindObject, s) }

// Endpoint keeps the scheme and the port and redacts the host.
//
// The split is the useful one. `https` vs `http`, and a non-default port, are exactly what someone
// debugging a location that will not connect needs; the hostname is exactly what identifies the
// organisation. A URL that does not parse is replaced wholesale rather than passed through — an
// unparseable endpoint is often a hand-typed one with something odd in it.
func (r *Redactor) Endpoint(raw string) string {
	if raw == "" || r.full {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return r.token(kindHost, raw)
	}
	host := r.token(kindHost, u.Hostname())
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	if u.Scheme == "" {
		return host
	}
	return u.Scheme + "://" + host
}

// ImageRepository redacts a registry+path while leaving the shape of it visible.
//
// A private registry host names the organisation as surely as a namespace does, and so, often, does
// the path (`harbor.example/platform-team/...`). Both go. What survives is in Image(): the TAG,
// which is the project's version string and not the user's information, and the DIGEST, which is a
// content hash of a public artifact and is the single most useful field in this report.
func (r *Redactor) ImageRepository(s string) string { return r.token(kindImage, s) }

// labelKinds maps a metric LABEL NAME to the token kind its value is redacted under. It is the
// bridge that makes a breach's labels carry the same tokens as the inventory tables — the same
// namespace, the same token, in both places, which is the only thing that makes a redacted report
// followable.
//
// A label not in this map is passed through verbatim. That is the risky direction and it is
// deliberate: `origin`, `scope` and `result` are API enums that carry the MEANING of the breach,
// and redacting them would leave a finding nobody can interpret. What bounds the risk is that the
// label names come from metrics.Catalogue() and predicates_test.go asserts every predicate reports
// exactly its series' label set — so a new label cannot appear here without a rule and a catalogue
// entry landing first, both under review.
//
// `source` and `destination` map to the LOCATION kind rather than to kinds of their own: they hold
// location names, and a secondary that appears as a destination in one sync and a source in another
// has to be recognisable as one place.
// The metric label NAMES this map keys on. Spelled as constants for the reason internal/alerts
// spells its own: a label name that appears as a bare string in two places eventually appears in
// two spellings, and a redactor keyed on the misspelling passes the value through in clear.
const (
	labelNamespace   = "namespace"
	labelTenant      = "tenant"
	labelLocation    = "location"
	labelSource      = "source"
	labelDestination = "destination"
	labelSchedule    = "schedule"
	labelSync        = "sync"
	labelPVC         = "pvc"
	labelCluster     = "cluster"
)

var labelKinds = map[string]string{
	labelNamespace:   kindNamespace,
	labelTenant:      kindTenant,
	labelLocation:    kindLocation,
	labelSource:      kindLocation,
	labelDestination: kindLocation,
	labelSchedule:    kindSchedule,
	labelSync:        kindSync,
	labelPVC:         kindPVC,
	labelCluster:     kindCluster,
}

// Labels redacts a breach's metric labels by label name.
func (r *Redactor) Labels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if kind, ok := labelKinds[k]; ok {
			out[k] = r.token(kind, v)
			continue
		}
		out[k] = v
	}
	return out
}

// Detail redacts the free-text half of a breach.
//
// A Breach.Detail is the one field a metric cannot carry — which object, by name — so it is both
// the most useful line in the report and the only place a raw name can survive redaction by
// accident. Rather than trying to parse the sentences, this substitutes every identifier the
// collector has already seen: the caller registers names as it walks the objects, and every
// occurrence is replaced by the same token the structured fields carry.
//
// Substitution is done longest-first, so a name that is a prefix of another (`team` and `team-a`)
// cannot corrupt the longer one, and it is bounded to whole-word occurrences by requiring a
// non-identifier character on each side — otherwise redacting `db` would mutilate every word
// containing it.
func (r *Redactor) Detail(s string) string {
	if s == "" || r.full {
		return s
	}
	for _, n := range r.known {
		s = replaceIdentifier(s, n.value, n.token)
	}
	return s
}

// knownName is one identifier the collector saw, with the token that replaces it in free text.
type knownName struct {
	value string
	token string
}

// Learn tokenises a value for its side effect alone: registering it so Detail substitutes it.
//
// The collector calls it for identifiers it does NOT otherwise put in a field — a location name it
// only needs in order to redact a sentence about it — so that the free-text half of a breach is
// covered even where the structured half is not.
func (r *Redactor) Learn(kind, value string) { _ = r.token(kind, value) }

// LearnLabels registers every identifier in a breach's label set before that breach's Detail is
// redacted.
//
// This is what closes the last gap, and it was a real one: crystalbackup_pvc_volumesnapshot_count's
// breach names a PVC in its Detail, and PVC names are the one identifier class the collector never
// enumerates (there is no per-PVC section in this report), so nothing else would ever have
// registered it. Registering from the labels means any identifier the alert would have carried is
// substituted out of the sentence beside it, whether or not this report has a table for it.
func (r *Redactor) LearnLabels(labels map[string]string) {
	for k, v := range labels {
		if kind, ok := labelKinds[k]; ok {
			r.Learn(kind, v)
		}
	}
}

// register inserts a (value, token) pair in descending length order, which is what makes
// overlapping names safe: `team` cannot be substituted inside `team-a` if `team-a` is tried first.
// Doing it at insertion rather than at use means Detail has no ordering requirement of its own to
// forget.
func (r *Redactor) register(value, token string) {
	i := 0
	for i < len(r.known) && len(r.known[i].value) >= len(value) {
		i++
	}
	r.known = append(r.known, knownName{})
	copy(r.known[i+1:], r.known[i:])
	r.known[i] = knownName{value: value, token: token}
}

// replaceIdentifier replaces whole-identifier occurrences of old in s. A match must not be
// surrounded by characters that could make it part of a longer name, which is what keeps `db` from
// eating the middle of `mariadb-primary`.
func replaceIdentifier(s, old, tok string) string {
	if old == "" || !strings.Contains(s, old) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], old)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + j
		end := start + len(old)
		before := byte(' ')
		if start > 0 {
			before = s[start-1]
		}
		after := byte(' ')
		if end < len(s) {
			after = s[end]
		}
		if isNameByte(before) || isNameByte(after) {
			// Part of a longer identifier: emit it untouched and keep scanning past its first byte
			// so an overlapping later match is still found.
			b.WriteString(s[i : start+1])
			i = start + 1
			continue
		}
		b.WriteString(s[i:start])
		b.WriteString(tok)
		i = end
	}
	return b.String()
}

// isNameByte reports whether c can be part of a Kubernetes object name (RFC 1123 label characters,
// plus '.' for the qualified names that appear in details).
func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.'
}

// ---------------------------------------------------------------------------
// The surface internal/soak needs.
//
// The kind constants above are unexported because nothing outside this package had a reason to
// name one: every identifier reached the report through a typed helper (Namespace, PVC, Location…)
// that already knew its own kind.
//
// internal/soak broke that assumption, and for a good reason. It watches every namespace, PVC,
// location and CR name in the cluster for a fortnight, so its registry of identifiers is far
// better than anything this package could reconstruct at export time — but registering one
// requires naming its kind, and a caller that could not name one would have to invent a prefix,
// which would break the very correlation the construction exists for.
//
// So they are exported as ALIASES rather than a second set of literals: the two cannot drift, and
// a rename here is still a single edit.
// ---------------------------------------------------------------------------

const (
	KindNamespace = kindNamespace
	KindTenant    = kindTenant
	KindLocation  = kindLocation
	KindSchedule  = kindSchedule
	KindSync      = kindSync
	KindPVC       = kindPVC
	KindBucket    = kindBucket
	KindHost      = kindHost
	KindPrefix    = kindPrefix
	KindCluster   = kindCluster
	KindPod       = kindPod
	KindObject    = kindObject
)
