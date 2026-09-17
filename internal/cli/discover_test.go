package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// mountedServer re-serves a fixture server under a non-default routes.api
// prefix and answers Payload's own "Route not found" for everything outside
// it — which is exactly what a project configured with
// `routes: { api: '/cms-api' }` looks like from the outside.
type mountedServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

// mountTransport is the only path in this package that may reach the loopback
// interface: TestMain replaces http.DefaultTransport with one that panics, and
// the mount proxy has to forward to the fixture server for real.
var mountTransport = &http.Client{Transport: &http.Transport{}}

func mountAt(t *testing.T, prefix, upstream string) *mountedServer {
	t.Helper()
	m := &mountedServer{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen = append(m.seen, r.URL.Path)
		m.mu.Unlock()

		if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			w.WriteHeader(http.StatusNotFound)
			body, _ := json.Marshal(map[string]any{"errors": []any{
				map[string]any{"message": `Route not found "` + r.URL.Path + `"`}}})
			_, _ = w.Write(body)
			return
		}
		out := upstream + "/api" + strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.RawQuery != "" {
			out += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, out, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := mountTransport.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(m.Server.Close)
	return m
}

// misdirected returns the paths the CLI asked for outside the mount, i.e. the
// ones a project with a custom routes.api answers 404 for.
func (m *mountedServer) misdirected(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.seen {
		if p != prefix && !strings.HasPrefix(p, prefix+"/") {
			out = append(out, p)
		}
	}
	return out
}

func resMeta(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	if r.Env == nil {
		t.Fatalf("stdout is not a JSON envelope:\n%s", r.Stdout)
	}
	m, _ := r.Env["meta"].(map[string]any)
	if m == nil {
		t.Fatalf("envelope has no meta block:\n%s", r.Stdout)
	}
	return m
}

// §7.2 autodiscovers api_path and §21.1 H1 promises PayCLI works against any
// Payload project. Both are worthless unless the probed prefix is adopted by
// the client that issues the data request: discovery finds the project at
// /cms-api, caches a manifest saying so, and every read and write still goes
// to the configured /api and comes back route_not_found (exit 4).
func TestProbedAPIPathIsAdoptedByDataCommands(t *testing.T) {
	srv := payloadtest.NewServer(t)
	mounted := mountAt(t, "/cms-api", srv.URL)
	home := t.TempDir()

	run := func() cliResult {
		return cliRun(t, invocation{
			Home: home,
			Args: []string{"find", "pages", "--limit", "2"},
			Env:  []string{"PAY_BASE_URL=" + mounted.URL, "PAY_AUTH_MODE=anonymous"},
		})
	}

	// Cold cache: discovery runs, probes the ladder, and must hand the answer
	// to the client the command already took.
	cold := run()
	if cold.Code != 0 {
		t.Fatalf("cold run exit = %d, want 0\n%s\n%s", cold.Code, cold.Stdout, cold.Stderr)
	}
	if got := resMeta(t, cold)["api_path"]; got != "/cms-api" {
		t.Errorf("cold meta.api_path = %v, want /cms-api", got)
	}

	// Warm cache: the manifest comes back from disk and the adoption has to
	// happen on that path too, or every invocation after the first is broken.
	warm := run()
	if warm.Code != 0 {
		t.Fatalf("warm run exit = %d, want 0\n%s\n%s", warm.Code, warm.Stdout, warm.Stderr)
	}
	if got := resMeta(t, warm)["api_path"]; got != "/cms-api" {
		t.Errorf("warm meta.api_path = %v, want /cms-api", got)
	}

	// The api_path ladder itself legitimately probes /api once per cold run;
	// no *data* request may.
	for _, p := range mounted.misdirected("/cms-api") {
		if strings.Contains(p, "/pages") {
			t.Errorf("a data request went to the configured prefix: %s", p)
		}
	}
}

// An explicitly configured api_path outranks a probe: --api-path, PAY_API_PATH
// and a profile key all say "the API is here", and neither a fresh probe nor a
// stale cached manifest may redirect it.
func TestExplicitAPIPathIsNeverOverriddenByAProbe(t *testing.T) {
	srv := payloadtest.NewServer(t)
	mounted := mountAt(t, "/cms-api", srv.URL)

	res := cliRun(t, invocation{
		Args: []string{"find", "pages", "--limit", "2", "--api-path", "/api"},
		Env:  []string{"PAY_BASE_URL=" + mounted.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if got := resMeta(t, res)["api_path"]; got != "/api" {
		t.Errorf("meta.api_path = %v, want the configured /api", got)
	}
}

// A project on the default prefix must be entirely unaffected by the adoption.
func TestDefaultAPIPathStaysDefault(t *testing.T) {
	srv := payloadtest.NewServer(t)
	res := cliRun(t, invocation{
		Args: []string{"find", "pages", "--limit", "2"},
		Env:  []string{"PAY_BASE_URL=" + srv.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if got := resMeta(t, res)["api_path"]; got != "/api" {
		t.Errorf("meta.api_path = %v, want /api", got)
	}
}
