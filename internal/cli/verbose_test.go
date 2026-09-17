package cli

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// `pay --help` promises "-v, --verbose  log every request at debug level".
// Before the transport grew a per-request line, both -v and --log-level debug
// resolved correctly and then produced ZERO bytes of output, so an agent whose
// --where matched nothing had no way to see the URL that was actually sent.
func TestVerboseLogsEveryRequest(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	for _, flags := range [][]string{{"-v"}, {"--log-level", "debug"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			args := append([]string{"find", "pages", "--limit", "1", "--where", "title contains Home"}, flags...)
			res := cliRun(t, invocation{
				Home: home, Args: args, Env: seededEnv(srv.URL),
			})
			if !strings.Contains(res.Stderr, "http request") {
				t.Fatalf("%v produced no per-request log line:\nstderr:%q\nstdout:%s",
					flags, res.Stderr, res.Stdout)
			}
			for _, want := range []string{"method=GET", "status=", "duration_ms=", "attempt=", "request_id="} {
				if !strings.Contains(res.Stderr, want) {
					t.Errorf("request line is missing %q:\n%s", want, res.Stderr)
				}
			}
			// The encoded filter is the whole reason an agent turns this on.
			if !strings.Contains(res.Stderr, "where") {
				t.Errorf("the request line does not show the --where that was sent:\n%s", res.Stderr)
			}
		})
	}
}

// §5.3: no log byte may carry the credential, in any level or format.
func TestVerboseNeverLogsTheCredential(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"find", "pages", "--limit", "1", "-v", "--log-format", "json"},
		Env: []string{
			"PAY_BASE_URL=" + srv.URL,
			"PAY_API_KEY=" + fixtureKey,
			"PAY_AUTH_COLLECTION=users",
		},
	})
	if strings.Contains(res.Stderr, fixtureKey) || strings.Contains(res.Stdout, fixtureKey) {
		t.Fatalf("the API key reached the output:\n%s\n%s", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, `"authorization":"<redacted:fp=`+redact.Fingerprint(fixtureKey)+`>"`) {
		t.Fatalf("§5.3's fixed Authorization literal is missing from the JSON log line:\n%s", res.Stderr)
	}
}

// --quiet is an explicit request for silence and beats -v.
func TestQuietBeatsVerbose(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"find", "pages", "--limit", "1", "-v", "--quiet"},
		Env:  seededEnv(srv.URL),
	})
	if strings.Contains(res.Stderr, "http request") {
		t.Fatalf("--quiet still logged requests:\n%s", res.Stderr)
	}
}

// The default level stays silent: the flags are what turn logging on.
func TestDefaultLevelLogsNothing(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home, Args: []string{"find", "pages", "--limit", "1"}, Env: seededEnv(srv.URL),
	})
	if strings.Contains(res.Stderr, "http request") {
		t.Fatalf("the default level logged a request:\n%s", res.Stderr)
	}
}
