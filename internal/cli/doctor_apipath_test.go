package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// cmsAPIProxy fronts a fixture Payload instance the way a reverse proxy with
// `routes: { api: '/cms-api' }` does: the project is reachable only under
// /cms-api, and /api answers Payload's own "Route not found" 404. It is the
// §21.1 H1 portability case reduced to one handler.
func cmsAPIProxy(t *testing.T, upstream string, prefix string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("upstream url: %v", err)
	}
	// Not http.DefaultClient: TestMain replaces it with panicTransport so a
	// unit test that reaches the real network fails loudly (§17.1). This hop
	// is loopback-to-loopback and gets a transport of its own.
	client := &http.Client{Transport: &http.Transport{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Route not found \"`+r.URL.Path+`\""}]}`)
			return
		}
		out := *r.URL
		out.Scheme, out.Host = target.Scheme, target.Host
		out.Path = "/api" + strings.TrimPrefix(r.URL.Path, prefix)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, out.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDoctorProbesTheResolvedAPIPath is finding 18's doctor half. §7.2's
// api_path ladder resolves /cms-api and doctor prints it under
// connection.api_path, but the reachability probe used to run before the
// manifest was adopted and therefore knocked on the configured /api. doctor
// then reported a healthy project as unreachable — and contradicted itself
// inside one envelope, which is the worst possible answer from the command an
// agent runs when it is already confused.
func TestDoctorProbesTheResolvedAPIPath(t *testing.T) {
	fixture := payloadtest.NewServer(t)
	proxy := cmsAPIProxy(t, fixture.URL, "/cms-api")

	res := cliRun(t, invocation{
		Args: []string{"doctor"},
		Env:  []string{"PAY_BASE_URL=" + proxy.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("doctor must report rather than fail: exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}

	data := res.data(t)
	conn, _ := data["connection"].(map[string]any)
	if conn["api_path"] != "/cms-api" {
		t.Fatalf("connection.api_path = %v, want /cms-api (the ladder never resolved it)", conn["api_path"])
	}

	checks := doctorChecks(t, res)
	reach, ok := checks["reachability"]
	if !ok {
		t.Fatalf("no reachability check: %v", data)
	}
	detail, _ := reach["detail"].(string)
	if reach["status"] != CheckOK {
		t.Fatalf("reachability = %v on a project that is up: %s", reach["status"], detail)
	}
	if !strings.Contains(detail, "/cms-api/access") {
		t.Errorf("reachability detail names the wrong path: %q", detail)
	}
	if strings.Contains(detail, "/api/access") && !strings.Contains(detail, "/cms-api/access") {
		t.Errorf("doctor still reports the configured /api: %q", detail)
	}
	// The whole point: no check may contradict connection.api_path.
	if data["ok"] != true {
		var failed []string
		for name, c := range checks {
			if c["status"] == CheckFail {
				failed = append(failed, name+": "+c["detail"].(string))
			}
		}
		t.Fatalf("doctor reports a reachable project as broken: %v", failed)
	}

	// And the probe really went to the prefix, not just the message: the
	// fixture never saw a request that the proxy had to reject.
	for _, miss := range fixture.Misses() {
		if strings.HasSuffix(miss.Path, "/access") {
			t.Errorf("the fixture saw an unmatched %s %s", miss.Method, miss.Path)
		}
	}
}

// TestDoctorReportsTheConfiguredAPIPathWhenItIsPinned: an explicit --api-path
// outranks the probe (§4.2), so doctor must keep reporting the path it will
// actually use rather than one it re-probed.
func TestDoctorReportsTheConfiguredAPIPathWhenItIsPinned(t *testing.T) {
	fixture := payloadtest.NewServer(t)
	proxy := cmsAPIProxy(t, fixture.URL, "/cms-api")

	res := cliRun(t, invocation{
		Args: []string{"doctor", "--api-path", "/cms-api"},
		Env:  []string{"PAY_BASE_URL=" + proxy.URL, "PAY_AUTH_MODE=anonymous"},
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", res.Code, res.Stdout, res.Stderr)
	}
	checks := doctorChecks(t, res)
	reach := checks["reachability"]
	if reach["status"] != CheckOK {
		t.Fatalf("reachability = %v: %v", reach["status"], reach["detail"])
	}
	if detail, _ := reach["detail"].(string); !strings.Contains(detail, "/cms-api/access") {
		t.Errorf("reachability detail = %q, want the pinned /cms-api", detail)
	}
}
