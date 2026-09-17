package payload

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// TestLargeUnparseableErrorBodyIsCappedUnderTheBufferLimit is finding 17's
// remaining half. MaxBodyBytes (64 MiB) only fires on a body that OVERFLOWS it,
// so an error body UNDER the limit was still retained whole: a 20 MiB
// uncompressed text/html 500 produced a ~21 MB envelope on the stdout an LLM
// agent has to read (measured: 20,973,222 bytes, 225 MB RSS, 3.8 s).
func TestLargeUnparseableErrorBodyIsCappedUnderTheBufferLimit(t *testing.T) {
	const bodyBytes = 20 << 20 // the reported repro, byte for byte
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		chunk := bytes.Repeat([]byte("A"), 1<<16)
		for sent := 0; sent < bodyBytes; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	// DefaultMaxBodyBytes on purpose: the whole point is that 20 MiB never
	// reaches the gzip-bomb path.
	c, _ := newTestClient(t, s, nil)

	resp, err := c.Do(context.Background(), &Request{
		Path:     "/pages",
		Classify: ClassifyContext{IncludeRaw: true},
	})
	if err == nil {
		t.Fatal("a 500 must be an error")
	}
	if resp == nil {
		t.Fatal("the response is needed for diagnosis")
	}
	if int64(len(resp.Body)) > maxRetainedBodyBytes {
		t.Fatalf("Response.Body kept %d bytes; error.raw and --output raw inline it verbatim, so it must stay within %d",
			len(resp.Body), maxRetainedBodyBytes)
	}
	if resp.Bytes != bodyBytes {
		t.Errorf("Response.Bytes = %d, want the truthful wire size %d", resp.Bytes, bodyBytes)
	}
	if !bytes.Contains(resp.Body, []byte("truncated")) {
		t.Errorf("the retained body must say it is a prefix, or an agent reads it as the whole answer: %q",
			tail(resp.Body, 200))
	}
	// The classification is unchanged: a capped body is still an HTML 500.
	e, _ := apierr.As(err)
	if e == nil || e.Code != apierr.CodeNonJSONResponse {
		t.Fatalf("err = %v, want non_json_response", err)
	}
	if e.HTTP == nil || len(e.HTTP.BodyExcerpt) == 0 {
		t.Error("§11.5's body_excerpt must survive the cap")
	}
	// The envelope's own bound: error.raw is what lands on stdout.
	if len(e.Raw) > maxRetainedBodyBytes+1024 {
		t.Fatalf("error.raw is %d bytes, want a bounded excerpt", len(e.Raw))
	}
}

// TestParseableJSONErrorBodyKeepsItsStructure guards the other half of the
// trade. An error body PayCLI can parse is NOT truncated, because §11.2 reads
// errors[0].name out of it and a bulk 400 reports the committed documents in
// docs[] beside the failures (§12.5) — capping those bytes would silently
// demote a precise partial_failure to a by-status guess.
func TestParseableJSONErrorBodyKeepsItsStructure(t *testing.T) {
	docs := make([]map[string]any, 0, 64)
	for i := 0; i < 64; i++ {
		docs = append(docs, map[string]any{"id": i, "title": strings.Repeat("t", 2048)})
	}
	body, err := json.Marshal(map[string]any{
		"docs":   docs,
		"errors": []map[string]any{{"id": 99, "message": "nope"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= maxRetainedBodyBytes {
		t.Fatalf("the fixture must exceed the %d-byte cap to be a regression test (got %d)",
			maxRetainedBodyBytes, len(body))
	}
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	})
	c, _ := newTestClient(t, s, nil)

	resp, doErr := c.Do(context.Background(), &Request{Method: http.MethodPatch, Path: "/pages"})
	if resp == nil {
		t.Fatal("no response")
	}
	if !bytes.Equal(resp.Body, body) {
		t.Fatalf("Response.Body = %d bytes, want the whole %d-byte JSON document", len(resp.Body), len(body))
	}
	if !apierr.HasCode(doErr, apierr.CodePartialFailure) {
		t.Fatalf("err = %v, want partial_failure (a truncated body cannot be classified)", doErr)
	}
}

// TestSmallErrorBodyIsUntouched: the cap is a ceiling, not a rewrite. An
// ordinary Payload error body must reach the classifier byte for byte.
func TestSmallErrorBodyIsUntouched(t *testing.T) {
	const body = `{"errors":[{"name":"ValidationError","message":"The following field is invalid: title"}]}`
	s := newStub(t, jsonHandler(http.StatusBadRequest, body))
	c, _ := newTestClient(t, s, nil)

	resp, err := c.Do(context.Background(), &Request{Method: http.MethodPatch, Path: "/pages/1"})
	if resp == nil {
		t.Fatal("no response")
	}
	if string(resp.Body) != body {
		t.Fatalf("Response.Body = %q, want it verbatim", resp.Body)
	}
	e, _ := apierr.As(err)
	if e == nil || e.HTTP == nil || e.HTTP.PayloadErrorName != "ValidationError" {
		t.Fatalf("the classifier did not read errors[0].name out of the body: %v", err)
	}
}

// TestLargeNonJSONSuccessBodyIsNotTruncated: only ERROR bodies are capped. A
// 2xx body is data — truncating it would hand a caller a short document that
// parses, which is the silent-wrong-answer class the cap exists to avoid.
func TestLargeNonJSONSuccessBodyIsNotTruncated(t *testing.T) {
	body := strings.Repeat("x", 4*maxRetainedBodyBytes)
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = io.WriteString(w, body)
	})
	c, _ := newTestClient(t, s, nil)

	resp, err := c.Do(context.Background(), &Request{Path: "/export", Accept: "*/*"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(resp.Body) != len(body) {
		t.Fatalf("Response.Body = %d bytes, want the whole %d", len(resp.Body), len(body))
	}
}

// TestRetainBodyNeverExceedsTheCap: the explanation is counted against the
// budget, never added on top of it — including when the caller's MaxBodyBytes
// is smaller than the notice itself.
func TestRetainBodyNeverExceedsTheCap(t *testing.T) {
	for _, n := range []int{0, 1, 32, maxRetainedBodyBytes / 2, maxRetainedBodyBytes, maxRetainedBodyBytes + 4096} {
		got := retainBody(bytes.Repeat([]byte("A"), n), "because")
		want := n
		if want > maxRetainedBodyBytes {
			want = maxRetainedBodyBytes
		}
		if len(got) > want {
			t.Errorf("retainBody(%d bytes) = %d bytes, want at most %d", n, len(got), want)
		}
	}
}

// TestRetainBodyKeepsValidUTF8: a cut that lands inside a multi-byte rune must
// not leave bytes that are not valid UTF-8 on their own — error.raw is encoded
// as a JSON string, and mojibake in a diagnosis is a diagnosis nobody trusts.
func TestRetainBodyKeepsValidUTF8(t *testing.T) {
	for off := 0; off < 4; off++ {
		body := append(bytes.Repeat([]byte("a"), maxRetainedBodyBytes-off), []byte("日本語のエラー")...)
		got := retainBody(body, "because")
		if !utf8.Valid(got) {
			t.Fatalf("offset %d produced invalid UTF-8 at the cut", off)
		}
	}
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}
