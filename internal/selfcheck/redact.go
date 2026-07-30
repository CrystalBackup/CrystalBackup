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
// the derivation is in this file and the inputs are in the report.
//
// So the salt is 32 bytes from crypto/rand, generated per report and thrown away with the process.
// The consequence is deliberate and is the whole design: correlation is preserved WITHIN a report
// (the same namespace is the same token in the inventory, in the leak samples and in every alert
// breach) and destroyed BETWEEN reports. Two reports from the same cluster share no tokens. Nobody,
// including the person who ran it, can reverse one afterwards.
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

// NewRedactor builds a redactor. full=true disables hashing entirely — for a report shared
// privately, with someone the operator has decided to trust with their namespace names.
func NewRedactor(full bool) (*Redactor, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		// Failing closed is the only acceptable behaviour here. A redactor that quietly fell back
		// to a fixed salt would produce a report that LOOKS redacted and is reversible by anyone
		// who reads this file, which is worse than refusing to produce one at all.
		return nil, fmt.Errorf("generate redaction salt: %w", err)
	}
	return &Redactor{salt: salt, full: full}, nil
}

// Full reports whether identifiers are being passed through unredacted.
func (r *Redactor) Full() bool { return r.full }

// Describe is the Redaction block that goes into the report, so a reader never has to guess which
// mode produced the file in front of them.
func (r *Redactor) Describe() Redaction {
	if r.full {
		return Redaction{
			Mode:          "full",
			SaltDisclosed: false,
			Note: "UNREDACTED. Namespace, tenant, PVC, location, bucket, endpoint and cluster " +
				"identifiers appear verbatim. Share privately only. Secrets, repository passwords, " +
				"S3 credentials and key material are still never included — they are not read.",
		}
	}
	return Redaction{
		Mode:          "hashed",
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
