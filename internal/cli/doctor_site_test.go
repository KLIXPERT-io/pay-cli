package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFS is a PATH probe over an in-memory file set; links maps a path to
// what it resolves to.
func fakeFS(files []string, links map[string]string) pathProbe {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	return pathProbe{
		exists: func(p string) bool { return set[p] },
		resolve: func(p string) (string, error) {
			if to, ok := links[p]; ok {
				return to, nil
			}
			if set[p] {
				return p, nil
			}
			return "", errors.New("no such file")
		},
	}
}

func TestBinaryPathCheck(t *testing.T) {
	const home = "/home/u"
	exe := "/home/u/.local/bin/pay"
	sep := string(filepath.ListSeparator)
	cases := []struct {
		name, exe, path, status string
		files                   []string
		links                   map[string]string
		wantInHint              []string
	}{
		{"on PATH", exe, "/usr/bin" + sep + "/home/u/.local/bin", CheckOK, []string{exe}, nil, nil},
		{"on PATH through a symlink", "/opt/pay/pay", "/usr/local/bin", CheckOK,
			[]string{"/opt/pay/pay", "/usr/local/bin/pay"}, map[string]string{"/usr/local/bin/pay": "/opt/pay/pay"}, nil},
		{"the agent's PATH lacks the install dir", exe, "/usr/local/bin" + sep + "/usr/bin", CheckWarn, []string{exe}, nil,
			[]string{"ln -sf /home/u/.local/bin/pay /usr/local/bin/pay", `export PATH="/home/u/.local/bin:$PATH"`, "~/.profile", "absolute path"}},
		{"a conventional per-user bin dir is preferred", exe, "/home/u/.cache/tool/bin" + sep + "/home/u/bin" + sep + "/usr/bin", CheckWarn, []string{exe}, nil,
			[]string{"ln -sf /home/u/.local/bin/pay /home/u/bin/pay"}},
		{"an arbitrary dir under HOME is not suggested", exe, "/home/u/.cache/tool/bin" + sep + "/usr/bin", CheckWarn, []string{exe}, nil,
			[]string{"ln -sf /home/u/.local/bin/pay /usr/local/bin/pay"}},
		{"another pay shadows this one", exe, "/usr/bin" + sep + "/home/u/.local/bin", CheckWarn,
			[]string{exe, "/usr/bin/pay"}, nil, []string{"/usr/bin/pay"}},
		{"unknown executable", "", "/usr/bin", CheckUnknown, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, info := binaryPathCheck(tc.exe, tc.path, home, "linux", fakeFS(tc.files, tc.links))
			if c.Status != tc.status || c.Name != "binary_on_path" {
				t.Fatalf("check = %+v", c)
			}
			for _, want := range tc.wantInHint {
				if !strings.Contains(c.Hint, want) {
					t.Errorf("hint does not contain %q:\n%s", want, c.Hint)
				}
			}
			if tc.status == CheckOK && info["on_path"] != true {
				t.Errorf("info = %v", info)
			}
		})
	}
}

// doctor reports the binary and the F9 site/preview configuration, and never
// prints the preview secret.
func TestDoctorReportsBinaryAndSite(t *testing.T) {
	fs := newFakeSite(t)
	home := t.TempDir()
	seedDiscovery(t, home, fs.URL)

	check := func(res cliResult, name string) map[string]any {
		t.Helper()
		for _, c := range res.data(t)["checks"].([]any) {
			if m := c.(map[string]any); m["name"] == name {
				return m
			}
		}
		t.Fatalf("doctor has no %q check:\n%s", name, res.Stdout)
		return nil
	}

	res := urlRun(t, fs, home, nil, "doctor")
	if c := check(res, "binary_on_path"); c["status"] != CheckUnknown {
		t.Errorf("binary_on_path without an executable = %v", c["status"])
	}
	if c := check(res, "site_url"); c["status"] != CheckOK {
		t.Errorf("site_url = %v", c)
	}
	if c := check(res, "preview"); c["status"] != CheckWarn || !strings.Contains(c["hint"].(string), "pay auth preview-secret --stdin") {
		t.Errorf("preview without a secret = %v", c)
	}

	res = urlRun(t, fs, home, []string{"PAY_PREVIEW_SECRET=" + testPreviewSecret}, "doctor")
	if strings.Contains(res.Stdout, testPreviewSecret) {
		t.Fatal("doctor printed the preview secret")
	}
	c := check(res, "preview")
	if c["status"] != CheckOK || !strings.Contains(c["detail"].(string), "env:PAY_PREVIEW_SECRET") {
		t.Errorf("preview with a secret = %v", c)
	}
	site := res.data(t)["site"].(map[string]any)
	if site["preview_key_source"] != "env:PAY_PREVIEW_SECRET" || site["preview_key_fingerprint"] == nil {
		t.Errorf("site = %v", site)
	}
}
