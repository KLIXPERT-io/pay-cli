package update

import "testing"

func TestDetectManaged(t *testing.T) {
	env := Env{GOPATH: "/home/u/go", GOBIN: "/home/u/bin", Home: "/home/u"}
	tests := []struct {
		name        string
		path        string
		env         Env
		wantManaged bool
		wantManager string
	}{
		{"a plain local install", "/home/u/.local/bin/pay", env, false, ""},
		{"/usr/local/bin is not managed", "/usr/local/bin/pay", env, false, ""},
		{"GOBIN", "/home/u/bin/pay", env, true, "go install"},
		{"GOPATH/bin", "/home/u/go/bin/pay", env, true, "go install"},
		{"default go bin without GOPATH", "/home/u/go/bin/pay", Env{Home: "/home/u"}, true, "go install"},
		{"nix", "/nix/store/abc-pay-1.0/bin/pay", env, true, "nix"},
		{"asdf", "/home/u/.asdf/installs/pay/1.0/bin/pay", env, true, "asdf"},
		{"mise", "/home/u/.local/share/mise/installs/pay/1.0/pay", env, true, "mise"},
		{"homebrew arm", "/opt/homebrew/bin/pay", env, true, "homebrew"},
		{"homebrew cellar", "/usr/local/Cellar/pay/1.0/bin/pay", env, true, "homebrew"},
		{"snap", "/snap/bin/pay", env, true, "snap"},
		{"flatpak", "/var/lib/flatpak/app/pay/current/pay", env, true, "flatpak"},
		{"program files", `C:\Program Files\pay\pay.exe`, env, true, "system install"},
		{"chocolatey", `C:\ProgramData\chocolatey\bin\pay.exe`, env, true, "chocolatey"},
		{"scoop", "/home/u/scoop/apps/pay/current/pay.exe", env, true, "scoop"},
		{"empty path", "", env, false, ""},
		{"a path that merely starts with the same letters", "/nix-store-backup/pay", env, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectManaged(tc.path, tc.env)
			if got.Managed != tc.wantManaged || got.Manager != tc.wantManager {
				t.Fatalf("DetectManaged(%q) = %+v, want managed=%v manager=%q",
					tc.path, got, tc.wantManaged, tc.wantManager)
			}
			if got.Managed && got.Hint == "" {
				t.Error("a managed install must carry a hint telling the user what to run instead")
			}
		})
	}
}
