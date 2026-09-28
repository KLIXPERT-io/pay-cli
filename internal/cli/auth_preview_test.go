package cli

import (
	"os"
	"strings"
	"testing"
)

func TestAuthPreviewSecretStoresReportsAndClears(t *testing.T) {
	home := t.TempDir()
	env := []string{"PAY_BASE_URL=http://localhost:3900", "PAY_AUTH_MODE=anonymous"}

	res := cliRun(t, invocation{Home: home, Env: env, Args: []string{"auth", "preview-secret"}})
	if res.Code != 0 || res.data(t)["configured"] != false {
		t.Fatalf("status before storing: %d\n%s", res.Code, res.Stdout)
	}

	res = cliRun(t, invocation{Home: home, Env: env, Stdin: testPreviewSecret, Args: []string{"auth", "preview-secret", "--stdin"}})
	if res.Code != 0 {
		t.Fatalf("store: %d\n%s", res.Code, res.Stdout)
	}
	if strings.Contains(res.Stdout, testPreviewSecret) || strings.Contains(res.Stderr, testPreviewSecret) {
		t.Fatal("the stored secret was echoed")
	}
	d := res.data(t)
	if d["configured"] != true || d["source"] != "file:credentials.json (preview_secret)" || d["fingerprint"] == nil {
		t.Fatalf("after store: %v", d)
	}
	creds, _ := d["credentials_file"].(string)
	if !strings.HasPrefix(creds, home) {
		t.Fatalf("credentials_file %q is outside PAY_HOME", creds)
	}
	if info, err := os.Stat(creds); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.json = %v, %v", info, err)
	}

	// An exported variable wins and the report says so.
	res = cliRun(t, invocation{Home: home, Env: append(env, "PAY_PREVIEW_SECRET=other-secret-value-123"),
		Stdin: testPreviewSecret, Args: []string{"auth", "preview-secret", "--stdin"}})
	shadowed := false
	for _, w := range res.warnings(t) {
		if w["code"] == "preview_secret_shadowed" {
			shadowed = true
		}
	}
	if !shadowed || res.data(t)["source"] != "env:PAY_PREVIEW_SECRET" {
		t.Errorf("shadowing not reported: %s", res.Stdout)
	}

	// auth status must not call a preview-only record "authenticated".
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"auth", "status"}})
	if res.data(t)["authenticated"] != false {
		t.Errorf("a preview-only record made auth status report authenticated: %s", res.Stdout)
	}

	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"auth", "preview-secret", "--clear"}})
	if d := res.data(t); d["removed"] != true || d["configured"] != false {
		t.Fatalf("clear: %v", d)
	}
	res = cliRun(t, invocation{Home: home, Env: env, Args: []string{"auth", "preview-secret", "--clear", "--stdin"}})
	if res.Code != 5 {
		t.Errorf("--clear --stdin: exit %d", res.Code)
	}
}
