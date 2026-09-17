package payload

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// debugClient is newTestClient with a debug logger wired to a buffer, which is
// exactly what `-v` / `--log-level debug` builds at the CLI layer.
func debugClient(t *testing.T, s *stub, opts logging.Options) (*Client, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	opts.Output = &buf
	if opts.Level == "" {
		opts.Level = logging.LevelDebug
	}
	c, _ := newTestClient(t, s, func(cfg *Config) {
		cfg.Logger = logging.New(opts)
	})
	return c, &buf
}

// §6 / §5.3. Before this existed, `-v` and `--log-level debug` enabled a level
// at which the request path logged nothing at all: an agent whose --where
// returned no documents could not see the URL that was actually sent.
func TestDebugLoggingEmitsOneLinePerRequest(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[]}`))
	c, buf := debugClient(t, s, logging.Options{})

	_, err := c.Do(context.Background(), &Request{
		Path:  "/pages",
		Query: "where%5Btitle%5D%5Bcontains%5D=Payload",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	log := buf.String()
	if n := strings.Count(log, "http request"); n != 1 {
		t.Fatalf("want exactly one request line, got %d:\n%s", n, log)
	}
	for _, want := range []string{
		`method=GET`, `status=200`, `duration_ms=`, `attempt=1`, `retries=0`, `request_id=`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("request line is missing %q:\n%s", want, log)
		}
	}
	// The whole point: the encoded filter must be visible.
	if !strings.Contains(log, "where") || !strings.Contains(log, "Payload") {
		t.Errorf("the request line does not show the query that was sent:\n%s", log)
	}
}

// §5.3's hard transport rule: the fixed literal, never the header value, and
// never the credential itself in any byte of any line.
func TestDebugLoggingRedactsTheAuthorizationHeader(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{Secrets: []string{"test-key-0123456789"}})
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	log := buf.String()
	if strings.Contains(log, "test-key-0123456789") {
		t.Fatalf("the credential reached a log line:\n%s", log)
	}
	want := "<redacted:fp=" + redact.Fingerprint("test-key-0123456789") + ">"
	if !strings.Contains(log, want) {
		t.Fatalf("§5.3's fixed Authorization literal %s is missing:\n%s", want, log)
	}
	if strings.Contains(log, "API-Key") {
		t.Fatalf("the Authorization header value reached a log line:\n%s", log)
	}
}

// A request that suppresses auth must not advertise a fingerprint it never sent.
func TestDebugLoggingReportsNoAuthorizationForAnUnauthenticatedProbe(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{})
	if _, err := c.Do(context.Background(), &Request{Path: "/users/init", NoAuth: true}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(buf.String(), "authorization="+redact.Mask) {
		t.Fatalf("a NoAuth probe must not publish a fingerprint:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "<redacted:fp=") {
		t.Fatalf("a NoAuth probe advertised a credential it never sent:\n%s", buf.String())
	}
}

// A URL that carries a credential in the query string goes through redact.URL.
func TestDebugLoggingRedactsCredentialsInTheURL(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{})
	if _, err := c.Do(context.Background(), &Request{
		Path:  "/pages",
		Query: "api-key=super-secret-value",
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if strings.Contains(buf.String(), "super-secret-value") {
		t.Fatalf("a credential in the query string reached the log:\n%s", buf.String())
	}
}

// Every attempt logs, and the retry counter advances with it.
func TestDebugLoggingCountsRetries(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, attempt int) {
		if attempt < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})
	c, buf := debugClient(t, s, logging.Options{})
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	log := buf.String()
	if n := strings.Count(log, `msg="http request"`); n != 3 {
		t.Fatalf("want one line per attempt (3), got %d:\n%s", n, log)
	}
	for _, want := range []string{"attempt=1 retries=0", "attempt=2 retries=1", "attempt=3 retries=2"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log, "status=503") || !strings.Contains(log, "status=200") {
		t.Errorf("the per-attempt status is not logged:\n%s", log)
	}
}

// --quiet beats -v: an explicit request for silence wins at the transport too.
func TestQuietSuppressesTheRequestLog(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{Verbose: true, Quiet: true})
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("--quiet still produced log output:\n%s", buf.String())
	}
}

// The default level must stay silent; the flags are what turn logging on.
func TestInfoLevelLogsNoRequests(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{Level: logging.LevelInfo})
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("info level logged a request:\n%s", buf.String())
	}
}

// --log-format json renders the same line as one JSON object.
func TestDebugLoggingRendersAsJSON(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{Format: logging.FormatJSON})
	if _, err := c.Do(context.Background(), &Request{Path: "/pages"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("the request line is not one JSON object: %v\n%s", err, line)
	}
	for _, key := range []string{"method", "url", "status", "duration_ms", "attempt", "retries", "request_id", "authorization"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("JSON request line is missing %q: %v", key, rec)
		}
	}
}

// A transport failure logs too — that is the case with no response to inspect.
func TestDebugLoggingCoversATransportFailure(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, buf := debugClient(t, s, logging.Options{})
	s.Close()
	_, _ = c.Do(context.Background(), &Request{Path: "/pages"})
	if !strings.Contains(buf.String(), `msg="http request"`) {
		t.Fatalf("a failed round trip logged nothing:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "status=0") {
		t.Errorf("a failed round trip must report status=0:\n%s", buf.String())
	}
}
