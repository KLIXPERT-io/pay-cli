package secret

import "testing"

func TestEnvSuffix(t *testing.T) {
	tests := map[string]string{
		"local":      "LOCAL",
		"staging":    "STAGING",
		"my-project": "MY_PROJECT",
		"my.project": "MY_PROJECT",
		"Prod EU":    "PROD_EU",
		"2nd":        "_2ND",
		"a1":         "A1",
		"":           "",
		"ünicode":    "__NICODE",
	}
	for in, want := range tests {
		if got := EnvSuffix(in); got != want {
			t.Errorf("EnvSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvNames(t *testing.T) {
	keys := APIKeyEnvNames("local")
	if len(keys) != 2 || keys[0] != "PAY_API_KEY_LOCAL" || keys[1] != "PAY_API_KEY" {
		t.Errorf("APIKeyEnvNames = %v", keys)
	}
	jwts := JWTEnvNames("staging")
	if len(jwts) != 2 || jwts[0] != "PAY_JWT_STAGING" || jwts[1] != "PAY_JWT" {
		t.Errorf("JWTEnvNames = %v", jwts)
	}
	if names := APIKeyEnvNames(""); len(names) != 1 || names[0] != "PAY_API_KEY" {
		t.Errorf("APIKeyEnvNames(\"\") = %v", names)
	}
}

func TestMapEnvTreatsEmptyAsUnset(t *testing.T) {
	env := MapEnv(map[string]string{"A": "1", "B": ""})
	if v, ok := env.Lookup("A"); !ok || v != "1" {
		t.Errorf("A = %q,%v", v, ok)
	}
	if _, ok := env.Lookup("B"); ok {
		t.Error("an empty value must count as unset")
	}
}
