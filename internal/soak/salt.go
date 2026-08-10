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

package soak

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/CrystalBackup/CrystalBackup/internal/selfcheck"
)

// ---------------------------------------------------------------------------------------------
// Where the soak's redaction salt comes from — a NAMED choice, never an implicit one.
//
// A soak's whole product is a fortnight read as one series: the same namespace has to be the same
// token in a metric on day 2, in an event on day 9 and in the self-check on day 13. That holds
// only while the salt holds, so how the salt is obtained is a first-class decision and the
// archive says which one was made.
//
// TWO METHODS, AND THE DEFAULT DERIVES RATHER THAN GENERATES.
//
// SaltMethodFromSecret is a Secret the admin creates by hand. It works, and it has a failure mode
// that costs a fortnight: the Secret can be lost or silently regenerated — a helm uninstall, a
// recreated namespace, somebody redoing it "to be safe" — and when it changes mid-soak the
// correlation breaks with nothing to signal it. Seven days of tokens on one side, seven on the
// other, and no way to join them.
//
// SaltMethodAuto derives from the UID of the operator's own namespace: unique per cluster,
// constant for the life of the namespace, and — the point — NOTHING GENERATES IT, so there is
// nothing to regenerate.
//
// WHY THE CHART MUST NOT GENERATE A SECRET FOR THIS. The obvious third option is for the chart to
// generate a random Secret and pin it with `lookup`. This repository already documents why that
// does not work, on the one thing it does generate:
// charts/crystal-backup/templates/webhook.yaml says of its certificate that "`lookup` would not
// fix it: Argo CD renders with `helm template`, which has no cluster to look anything up in."
// Under continuous rendering the salt would change on every refresh — three minutes by default.
// And it would be WORSE than the certificate case: a regenerated certificate rolls the Deployment,
// which is loud and gets noticed; a regenerated salt changes nothing visible at all. The collector
// keeps running, the reports keep being written, and only the correlation silently dies. This
// project ships Argo CD and Flux install pages, so those are its users. Deriving is immune to all
// of it.
// ---------------------------------------------------------------------------------------------

// The salt methods. Named values, because the archive states which one produced it and a reader
// deciding whether a file is safe to attach is deciding on that statement.
const (
	// SaltMethodAuto derives the salt from the operator namespace's UID. The default.
	SaltMethodAuto = "auto"
	// SaltMethodFromSecret reads it from a mounted Secret (--redaction-salt-file).
	SaltMethodFromSecret = "from-secret"
)

// SaltMethods is the valid set, in the order the refusal message lists them.
var SaltMethods = []string{SaltMethodAuto, SaltMethodFromSecret}

// saltDomainSeparator makes the derived key a key FOR THIS PURPOSE.
//
// The namespace UID is not used raw, for two reasons and neither is ceremony. It is a
// 36-character string, so it would clear MinSaltBytes by accident rather than by construction —
// a floor that is met by the shape of a UUID is a floor nobody is actually enforcing. And a raw
// UID used as an HMAC key here could not be used as a key anywhere else without the two uses
// sharing one secret; hashing under a versioned domain string means the same UID can key
// something else later, and means this derivation can be revised (v2) without colliding with
// archives produced under v1.
const saltDomainSeparator = selfcheck.SaltNamespaceUIDDomain

// DeriveNamespaceSalt turns a namespace UID into the 32-byte salt.
//
// SHA256(domain || uid), which is 32 bytes exactly — MinSaltBytes — by construction rather than
// by luck. An empty UID is REFUSED: it is what an unreadable namespace or a typo'd namespace name
// produces, and hashing it would yield a perfectly valid-looking salt that is the SAME on every
// cluster where that read failed, silently making unrelated archives correlate with each other.
func DeriveNamespaceSalt(uid string) ([]byte, error) {
	if strings.TrimSpace(uid) == "" {
		return nil, fmt.Errorf("the operator namespace has no UID: refusing to derive a salt from " +
			"nothing, because the result would be identical on every cluster this happened on")
	}
	sum := sha256.Sum256([]byte(saltDomainSeparator + uid))
	return sum[:], nil
}

// ResolveSalt turns the named method into the bytes and the provenance string that goes in the
// report.
//
// NO SILENT FALLBACK, in either direction. An unknown method is refused rather than defaulted; a
// fromSecret run whose Secret is missing or short is refused rather than quietly derived; a
// derivation that cannot read the namespace is refused rather than quietly randomised. All three
// have the same shape of consequence — a report that claims one guarantee and holds another —
// which is the reason LoadRedactionSalt already refuses to degrade into a random salt.
//
// namespaceUID is a function rather than a string so the API read happens ONLY for the method
// that needs it: a fromSecret collector must not fail to start because it cannot get its own
// namespace, and must not appear to need a grant it does not use.
func ResolveSalt(method, saltFile string, namespaceUID func() (string, error)) ([]byte, string, error) {
	switch method {
	case SaltMethodAuto:
		uid, err := namespaceUID()
		if err != nil {
			return nil, "", fmt.Errorf("--salt-method=%s needs to read the operator namespace to "+
				"derive the salt, and could not: %w\n\n"+
				"  The collector's ClusterRole must allow `get` on namespaces.\n"+
				"  Refusing to fall back: a random salt would produce an archive whose tokens\n"+
				"  correlate with nothing, which is exactly what a soak is for",
				SaltMethodAuto, err)
		}
		salt, err := DeriveNamespaceSalt(uid)
		if err != nil {
			return nil, "", err
		}
		return salt, selfcheck.SaltNamespaceUID, nil

	case SaltMethodFromSecret:
		if saltFile == "" {
			return nil, "", fmt.Errorf("--salt-method=%s needs --redaction-salt-file, and none was "+
				"given.\n\n"+
				"  Refusing to fall back to --salt-method=%s: the archive would claim one\n"+
				"  guarantee and hold another, and nothing about it would look wrong",
				SaltMethodFromSecret, SaltMethodAuto)
		}
		salt, err := selfcheck.LoadRedactionSalt(saltFile)
		if err != nil {
			return nil, "", fmt.Errorf("--salt-method=%s: %w\n\n"+
				"  Refusing to fall back to --salt-method=%s. A missing Secret is exactly the\n"+
				"  case that method exists to make visible",
				SaltMethodFromSecret, err, SaltMethodAuto)
		}
		return salt, selfcheck.SaltCallerSupplied, nil

	default:
		return nil, "", fmt.Errorf("unknown --salt-method %q; valid values are %s",
			method, strings.Join(quoted(SaltMethods), " and "))
	}
}

// checkSaltFlags is the STARTUP refusal: everything about the salt choice that can be decided
// before a client exists or a scrape is attempted.
//
// The third case is the one worth arguing for. A --redaction-salt-file passed under the default
// method would simply be IGNORED — the Secret is mounted, the flag is set, the manifest looks
// exactly like a fromSecret collector, and the archive is salted from the namespace UID instead.
// Nothing about that is visible in a running collector. So it is a refusal, and it is the same
// principle as the rest of this file: the method is a named value, and a configuration that
// implies one method while requesting another is a question, not a default.
func checkSaltFlags(method, saltFile string) error {
	if !slices.Contains(SaltMethods, method) {
		return fmt.Errorf("unknown --salt-method %q; valid values are %s",
			method, strings.Join(quoted(SaltMethods), " and "))
	}
	if method == SaltMethodFromSecret && saltFile == "" {
		return fmt.Errorf("--salt-method=%s needs --redaction-salt-file naming the mounted Secret",
			SaltMethodFromSecret)
	}
	if method == SaltMethodAuto && saltFile != "" {
		return fmt.Errorf("--redaction-salt-file=%s is set but --salt-method is %s, which derives "+
			"the salt from the operator namespace's UID and would IGNORE that file.\n\n"+
			"  Pass --salt-method=%s to use the Secret, or drop the file to use the derived salt.\n"+
			"  Refusing to choose for you: the two produce archives with different guarantees and\n"+
			"  a running collector looks identical either way",
			saltFile, SaltMethodAuto, SaltMethodFromSecret)
	}
	return nil
}

// namespaceUID returns the reader ResolveSalt calls for --salt-method=auto: a `get` on ONE
// namespace, the operator's own.
//
// It is a closure rather than a value so nothing is read for a run that does not need it, and so
// a fromSecret collector cannot fail to start on a cluster where that grant is missing. The
// collector's ClusterRole already carries `get` on namespaces (it lists them for the CR-state
// stream), so `auto` adds no new grant — which is worth stating, because a default that silently
// widened RBAC would be a bad default however convenient.
func namespaceUID(ctx context.Context, cs kubernetes.Interface, ns string) func() (string, error) {
	return func() (string, error) {
		obj, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("get namespace %q: %w", ns, err)
		}
		return string(obj.UID), nil
	}
}

// noNamespaceToRead is the namespace reader handed to ResolveSalt on the paths that run BEFORE any
// cluster connection — soak-collect's from-secret salt read, which is deliberately ordered ahead of
// the connection because an unreadable salt file is a usage error and not a cluster failure.
//
// ResolveSalt only calls its namespaceUID for --salt-method=auto, and those call sites resolve auto
// elsewhere, so this never runs. It is a function that errors rather than a nil parameter so that a
// future method needing the cluster from here fails with a sentence somebody can act on instead of a
// nil-func panic in a collector's first second.
func noNamespaceToRead() (string, error) {
	return "", fmt.Errorf("internal: the salt was resolved before any cluster connection, so there " +
		"is no namespace to read a UID from here")
}

// exportNamespaceUID is namespaceUID for soak-export, which builds no client of its own.
//
// It builds one LAZILY, inside the closure, for the same reason namespaceUID is a closure at all:
// `soak-export --salt-method=fromSecret` and `--full` must not acquire a dependency on the API
// server to read a tarball off a volume. Export runs through `kubectl exec` into the collector's
// own pod, so when it IS called it has the collector's ServiceAccount token and reads the same
// namespace the collector did.
func exportNamespaceUID(namespace string) func() (string, error) {
	return func() (string, error) {
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return "", fmt.Errorf("no Kubernetes configuration: %w", err)
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return "", fmt.Errorf("cannot build a Kubernetes client: %w", err)
		}
		return namespaceUID(context.Background(), cs, namespace)()
	}
}

func quoted(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range slices.Sorted(slices.Values(in)) {
		out = append(out, `"`+s+`"`)
	}
	return out
}
