package apierr

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// constDecl finds every `CodeX Code = "x"` declaration in code.go.
var constDecl = regexp.MustCompile(`(?m)^\s*(Code[A-Za-z0-9]+)\s+Code\s*=\s*"([a-z0-9_]+)"`)

// TestExitMapIsTotal is the §11.4 guarantee. It reads the declarations out of
// code.go rather than a hand-maintained list, so a code added tomorrow without
// an exit mapping fails the build instead of silently exiting 1 — which is
// exactly the gsc-cli `default: return 1` bug this map replaces.
func TestExitMapIsTotal(t *testing.T) {
	src, err := os.ReadFile("code.go")
	if err != nil {
		t.Fatalf("read code.go: %v", err)
	}
	matches := constDecl.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no Code constants found in code.go; the regexp needs updating")
	}
	declared := make(map[Code]string, len(matches))
	for _, m := range matches {
		declared[Code(m[2])] = m[1]
	}

	for code, ident := range declared {
		exit, ok := exitByCode[code]
		if !ok {
			t.Errorf("%s (%q) has no entry in exitByCode", ident, code)
			continue
		}
		if exit < 0 || exit > MaxExit {
			t.Errorf("%s maps to exit %d, outside 0..%d", ident, exit, MaxExit)
		}
		if exit == ExitOK {
			t.Errorf("%s maps to exit 0; an error code is never a success", ident)
		}
	}
	for code := range exitByCode {
		if _, ok := declared[code]; !ok {
			t.Errorf("exitByCode has %q, which is not declared as a constant in code.go", code)
		}
	}
	if len(declared) != len(exitByCode) {
		t.Errorf("declared %d codes but mapped %d", len(declared), len(exitByCode))
	}
	if got := len(Codes()); got != len(exitByCode) {
		t.Errorf("Codes() returned %d, want %d", got, len(exitByCode))
	}
}

// TestExitStatusesAreNeverShellTerritory guards §11.4's "never 125-128 or 130".
func TestExitStatusesAreNeverShellTerritory(t *testing.T) {
	forbidden := map[int]bool{125: true, 126: true, 127: true, 128: true, 130: true}
	for _, c := range Codes() {
		if forbidden[c.Exit()] {
			t.Errorf("%s exits %d, which is shell/signal territory", c, c.Exit())
		}
	}
}

// TestExitByClass pins the §11.4 table, class by class.
func TestExitByClass(t *testing.T) {
	tests := []struct {
		exit  int
		codes []Code
	}{
		{ExitInternal, []Code{CodeInternal, CodeUnknown, CodeCacheCorrupt, CodeUpdateVerificationFailed, CodeAuditWriteFailed}},
		{ExitAuth, []Code{CodeAuthMissing, CodeAuthInvalid, CodeAuthRequired, CodeAuthLocked, CodeAuthUnverifiedEmail, CodeAuthInsecurePermissions, CodeAuthHelperFailed}},
		{ExitThrottled, []Code{CodeRateLimited, CodeDocLocked, CodeServerBusy}},
		{ExitNotFound, []Code{CodeDocNotFound, CodeRouteNotFound, CodeVersionNotFound, CodeSelectorNoMatch}},
		{ExitValidation, []Code{CodeValidationFailed, CodeQueryPathInvalid, CodeInvalidArgs, CodeInvalidWhereSyntax, CodeInvalidSortField, CodeUnknownField, CodeInvalidOption, CodeInvalidID, CodeBadRequestBody, CodeWhereRequired, CodeFileMissing, CodeNotUploadCollection, CodeUnsupportedOperator, CodeBulkLimitExceeded, CodeRequestTooLarge, CodeFormatUnsupported, CodeInvalidPathExpr, CodeSelectorAmbiguous, CodeFieldAmbiguous, CodeNoInput, CodeNoEdits}},
		{ExitNetwork, []Code{CodeNetworkUnreachable, CodeDNSFailure, CodeTLSError, CodeTimeout, CodeServerError, CodeServerUnavailable, CodeNonJSONResponse}},
		{ExitPartial, []Code{CodePartialFailure}},
		{ExitAccessDenied, []Code{CodeAccessDenied, CodeAdminAccessDenied}},
		{ExitConfig, []Code{CodeConfigMissing, CodeConfigSecretInPlaintext, CodeProfileUnknown, CodeBaseURLInvalid, CodeEndpointNotPayload, CodeAuthCollectionUnknown}},
		{ExitCapability, []Code{CodeCollectionUnknown, CodeGlobalUnknown, CodeFeatureUnavailable, CodeOperationUnsupported, CodeDiscoveryFailed, CodeGraphQLDisabled, CodeSchemaStale, CodeBlockTypeUnknown}},
		{ExitConfirmationRequired, []Code{CodeConfirmationRequired}},
	}
	seen := 0
	for _, tc := range tests {
		for _, c := range tc.codes {
			if got := c.Exit(); got != tc.exit {
				t.Errorf("%s.Exit() = %d, want %d", c, got, tc.exit)
			}
			seen++
		}
		got := CodesByExit()[tc.exit]
		want := append([]Code(nil), tc.codes...)
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if len(got) != len(want) {
			t.Errorf("exit %d groups %d codes, want %d", tc.exit, len(got), len(want))
		}
	}
	if seen != len(exitByCode) {
		t.Errorf("the class table covers %d codes, the map has %d", seen, len(exitByCode))
	}
}

// TestRetriable pins §11.4's auto-retry column. 500 and exit 7 must never be
// retriable: both can already have committed a write.
func TestRetriable(t *testing.T) {
	tests := []struct {
		code Code
		want bool
	}{
		{CodeRateLimited, true},
		{CodeDocLocked, true},
		{CodeServerBusy, true},
		{CodeServerUnavailable, true},
		{CodeTimeout, true},
		{CodeDNSFailure, true},
		{CodeNetworkUnreachable, true},
		{CodeServerError, false},
		{CodePartialFailure, false},
		{CodeNonJSONResponse, false},
		{CodeValidationFailed, false},
		{CodeAuthInvalid, false},
		{CodeAccessDenied, false},
	}
	for _, tc := range tests {
		if got := tc.code.Retriable(); got != tc.want {
			t.Errorf("%s.Retriable() = %v, want %v", tc.code, got, tc.want)
		}
	}
	for _, c := range Codes() {
		if c.Retriable() && c.Exit() != ExitThrottled && c.Exit() != ExitNetwork {
			t.Errorf("%s is retriable but exits %d; only classes 3 and 6 auto-retry", c, c.Exit())
		}
	}
}

func TestUnknownCodeDoesNotPanic(t *testing.T) {
	c := Code("something_nobody_declared")
	if c.Known() {
		t.Error("Known() = true for an undeclared code")
	}
	if got := c.Exit(); got != ExitInternal {
		t.Errorf("Exit() = %d, want %d", got, ExitInternal)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"apierr", New(CodeValidationFailed, "x"), 5},
		{"wrapped apierr", Wrap(New(CodePartialFailure, "x"), CodeAccessDenied, "y"), 8},
		{"plain error", errString("boom"), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestEveryCodeHasAHint enforces the §10.5 doctrine that every "you cannot X"
// is followed by "do Y instead".
func TestEveryCodeHasAHint(t *testing.T) {
	for _, c := range Codes() {
		if DefaultHint(c) == "" {
			t.Errorf("%s has no default hint", c)
		}
	}
}
