package safety

import (
	"encoding/json"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// SampleLimit is how many ids --dry-run shows in data.sample_ids (§12.2).
const SampleLimit = 5

// DryRunRequest is the request --dry-run would have sent. It is printed
// verbatim (after redaction) so an agent can hand it to `pay raw` or curl.
type DryRunRequest struct {
	Method string          `json:"method"`
	URL    string          `json:"url"`
	Body   json.RawMessage `json:"body"`
}

// DryRunResult is the §12.2 payload: data_kind "op_result", exit 0.
type DryRunResult struct {
	WouldAffect int           `json:"would_affect"`
	SampleIDs   []any         `json:"sample_ids"`
	Truncated   bool          `json:"truncated"`
	Request     DryRunRequest `json:"request"`
}

// NewDryRun builds the result. The URL always passes through redact.URL, and
// the body through redact.JSON unless --no-redact was given: a dry-run body can
// contain a password or an apiKey when the target is the auth collection, and
// §5.3 admits no exception for "it was only a preview".
//
// total is the resolved match count (N from the count call); ids are the
// resolved ids, of which at most SampleLimit are shown.
func NewDryRun(req DryRunRequest, total int, ids []any, noRedact bool) DryRunResult {
	req.URL = redact.URL(req.URL)
	if len(req.Body) > 0 && !noRedact {
		req.Body = json.RawMessage(redact.JSON(req.Body).Data)
	}

	sample := make([]any, 0, SampleLimit)
	for i, id := range ids {
		if i >= SampleLimit {
			break
		}
		sample = append(sample, id)
	}
	if total < 0 {
		total = len(ids)
	}
	// truncated says "sample_ids is not the whole story". A create resolves no
	// ids at all (the document does not exist yet), so an empty sample beside
	// would_affect: 1 is complete, not truncated.
	truncated := len(ids) > len(sample) || (len(ids) == 0 && total > 1)
	return DryRunResult{
		WouldAffect: total,
		SampleIDs:   sample,
		Truncated:   truncated,
		Request:     req,
	}
}

// Envelope wraps the result in the §12.2 envelope. meta.dry_run is forced true
// so it cannot disagree with the data, and the exit code is 0: a dry run that
// resolved its target successfully is a success.
func (r DryRunResult) Envelope(command string, meta output.Meta) *output.Envelope {
	meta.DryRun = true
	return output.New(command, output.KindOpResult, r).WithMeta(meta)
}
