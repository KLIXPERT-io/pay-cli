package apierr

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// ID types a collection can have (§7.6). "unknown" is a real, common value and
// it disables every local check that depends on the id type: a client-side
// rejection built on a fact PayCLI never learned is a fabricated error.
const (
	IDTypeNumber  = "number"
	IDTypeString  = "string"
	IDTypeUnknown = "unknown"
)

// CheckID pre-empts §11.3's first opaque-500 cause: an id that does not parse
// as the collection's id type produces HTTP 500 "Something went wrong.", and on
// /duplicate it can produce an unintended write. It returns nil — meaning
// "send the request" — whenever idType is unknown or empty, per §9.3's
// tri-state rule.
func CheckID(collection, id, idType string) *Error {
	switch idType {
	case IDTypeNumber:
		if _, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err != nil {
			return New(CodeInvalidID,
				"%q is not a valid id for %q: this collection's ids are numbers.", id, collection).
				WithHint("Pass a numeric id. `pay find %s --limit 5 --select id` lists real ids. Payload answers a non-castable id with an opaque 500, which is why PayCLI rejects it here.", collection)
		}
	case IDTypeString:
		if strings.TrimSpace(id) == "" {
			return New(CodeInvalidID, "An empty id is not valid for %q.", collection)
		}
	}
	return nil
}

// CheckVersions pre-empts §11.3's second opaque-500 cause: /versions on a
// collection without versions enabled. hasVersions is a tri-state — nil means
// "not discovered", and PayCLI then sends the request rather than guessing.
func CheckVersions(collection string, hasVersions *bool) *Error {
	if hasVersions == nil || *hasVersions {
		return nil
	}
	return New(CodeFeatureUnavailable,
		"%q does not have versions enabled, so it has no /versions route.", collection).
		WithHint("Enable versions on the collection in payload.config.ts, or drop the version flags. `pay describe %s` reports this project's flags.", collection)
}

// Relationship describes one relationship value in a request body, used to
// diagnose §11.3's third cause after the fact.
type Relationship struct {
	Path       string // request-body leaf path, e.g. "heroImage"
	Collection string // the relationTo target slug
	ID         any    // the id that was sent
}

// Causes500 ranks the likely causes of an opaque 500 (§11.3). The first two
// rows of that table are rejected client-side before the call, so what remains
// is the relationship diagnosis plus the standing advice that the real message
// is one config flag away.
func Causes500(method, path string, rels []Relationship) []Cause {
	causes := make([]Cause, 0, len(rels)+2)
	for _, rel := range rels {
		detail := fmt.Sprintf("%s references %v", rel.Path, rel.ID)
		fix := "Check that the referenced document exists."
		if rel.Collection != "" {
			detail = fmt.Sprintf("%s -> %s %v", rel.Path, rel.Collection, rel.ID)
			fix = fmt.Sprintf("pay get %s %v", rel.Collection, rel.ID)
		}
		causes = append(causes, Cause{
			Cause:      "relationship_target_missing",
			Confidence: CauseMedium,
			Detail:     detail,
			Fix:        fix,
		})
	}
	if isWriteMethod(method) {
		causes = append(causes, Cause{
			Cause:      "hook_threw",
			Confidence: CauseLow,
			Detail:     "A beforeChange / afterChange hook on this collection may have thrown; hooks are not introspectable over REST or GraphQL.",
			Fix:        "Read the Payload server log for the real stack trace.",
		})
	}
	causes = append(causes, Cause{
		Cause:      "message_masked_by_config",
		Confidence: CauseLow,
		Detail:     "Payload masks non-public errors as \"Something went wrong.\" unless config.debug is true, so the body carries no diagnosis.",
		Fix:        "Set debug: true in payload.config.ts and re-run to see the real error.",
	})
	return causes
}

func isWriteMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "POST", "PATCH", "PUT", "DELETE":
		return true
	}
	return false
}

// maxSuggestions caps did_you_mean so the envelope stays small.
const maxSuggestions = 3

// DidYouMean ranks candidates by closeness to input for error.did_you_mean.
// A prefix or substring match always beats a pure edit-distance match, because
// "page" -> "pages" is the mistake agents actually make.
func DidYouMean(input string, candidates []string) []string {
	if input == "" || len(candidates) == 0 {
		return []string{}
	}
	in := strings.ToLower(input)
	type scored struct {
		value string
		rank  int
		dist  int
	}
	var out []scored
	for _, c := range candidates {
		lc := strings.ToLower(c)
		d := levenshtein(in, lc)
		switch {
		case lc == in:
			out = append(out, scored{c, 0, d})
		case strings.HasPrefix(lc, in) || strings.HasPrefix(in, lc):
			out = append(out, scored{c, 1, d})
		case strings.Contains(lc, in) || strings.Contains(in, lc):
			out = append(out, scored{c, 2, d})
		case d <= maxDistance(len(in)):
			out = append(out, scored{c, 3, d})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		return out[i].value < out[j].value
	})
	res := make([]string, 0, maxSuggestions)
	for _, s := range out {
		if len(res) == maxSuggestions {
			break
		}
		res = append(res, s.value)
	}
	return res
}

// maxDistance scales the tolerance with the input length so a 3-letter slug
// does not "mean" every other 3-letter slug.
func maxDistance(n int) int {
	switch {
	case n <= 4:
		return 1
	case n <= 8:
		return 2
	default:
		return 3
	}
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Network classifies a transport-level failure (§11.4 exit 6). It is here
// rather than in internal/payload so the classification is testable without a
// socket and so every caller agrees on the code.
func Network(err error) *Error {
	if err == nil {
		return nil
	}
	var dnsErr *net.DNSError
	if asErr(err, &dnsErr) {
		return Wrap(err, CodeDNSFailure, "The hostname %q did not resolve.", dnsErr.Name)
	}
	var netErr net.Error
	if asErr(err, &netErr) && netErr.Timeout() {
		return Wrap(err, CodeTimeout, "The request timed out.")
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "context deadline exceeded"), strings.Contains(lower, "timeout"):
		return Wrap(err, CodeTimeout, "The request timed out.")
	case strings.Contains(lower, "x509"), strings.Contains(lower, "tls"), strings.Contains(lower, "certificate"):
		return Wrap(err, CodeTLSError, "TLS handshake failed: %s", msg)
	case strings.Contains(lower, "no such host"):
		return Wrap(err, CodeDNSFailure, "The hostname did not resolve: %s", msg)
	default:
		return Wrap(err, CodeNetworkUnreachable, "The server could not be reached: %s", msg)
	}
}

// asErr is a thin errors.As wrapper so the network matchers stay readable.
func asErr(err error, target any) bool {
	return errors.As(err, target)
}
