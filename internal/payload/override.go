package payload

import (
	"net/http"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// HeaderMethodOverride is Payload's read-method override header (§6.2).
const HeaderMethodOverride = "X-Payload-HTTP-Method-Override"

// FormURLEncoded is the Content-Type the override path requires. Payload's
// check is `=== 'application/x-www-form-urlencoded'`, so a `; charset=utf-8`
// suffix makes it silently discard the entire body — verified live: the same
// request with the suffix returned the default 10 documents instead of the 2
// the body asked for. This constant is compared byte-for-byte before any
// override request is allowed to leave.
const FormURLEncoded = "application/x-www-form-urlencoded"

// URLBudget is §6.2's promotion threshold. Node's default
// --max-http-header-size is 16,384 bytes for the whole header block, and the
// request line is only part of it.
const URLBudget = 8000

// promoteIfTooLong applies the pre-emptive half of §6.2: a read whose URL
// exceeds the budget is rewritten before it is ever sent.
func promoteIfTooLong(p *requestPlan, req *Request) error {
	if len(p.url) <= URLBudget || !canPromote(p, req) {
		return nil
	}
	return promotePlan(p, req)
}

// canPromote encodes the hard restriction of §6.2: READ-shaped requests only.
// The override is never used for PATCH or DELETE, because Payload silently
// discards a body whose Content-Type it does not recognise, and a bulk write
// whose `where` was discarded acts on every document in the collection.
func canPromote(p *requestPlan, req *Request) bool {
	if req != nil && req.NoOverride {
		return false
	}
	if p.promoted || p.multipart != nil {
		return false
	}
	switch p.method {
	case http.MethodGet:
		return p.body == nil
	default:
		return false
	}
}

// promotePlan rewrites a long GET as the override POST.
func promotePlan(p *requestPlan, req *Request) error {
	if !canPromote(p, req) {
		return apierr.New(apierr.CodeRequestTooLarge,
			"the request is too large for this server and %s cannot use the read-method override", p.method).
			WithHint("narrow --where, or split the ids across several commands: PayCLI never sends a " +
				"PATCH or DELETE through the override, because a discarded body would act on every document")
	}
	path, query, _ := strings.Cut(p.url, "?")
	p.method = http.MethodPost
	p.url = path
	p.body = []byte(query)
	p.contentType = FormURLEncoded
	p.override = http.MethodGet
	p.promoted = true
	p.readOnly = true
	p.retriable = true
	return nil
}

// assertOverride is the runtime assertion §6.2 demands. It runs inside
// transport.go immediately before the header is set, so no call site can
// construct an override request that Payload would misread.
func assertOverride(p *requestPlan) error {
	if p.override != http.MethodGet {
		return apierr.New(apierr.CodeInternal,
			"the method override may only request GET; got %q", p.override)
	}
	if p.method != http.MethodPost {
		return apierr.New(apierr.CodeInternal,
			"the method override is only valid on POST; got %s", p.method)
	}
	if p.contentType != FormURLEncoded {
		return apierr.New(apierr.CodeInternal,
			"the method override requires the Content-Type %q byte-for-byte; got %q",
			FormURLEncoded, p.contentType).
			WithHint("a `; charset=utf-8` suffix makes Payload discard the whole body and answer with defaults")
	}
	return nil
}
