package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveBackupDir is §12.8's chain: --backup-dir > PAY_BACKUP_DIR >
// ./pay.toml > config.toml > off, with each layer's relative value anchored
// where it was written.
func TestResolveBackupDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture paths are POSIX")
	}
	stack := func() Input {
		return Input{
			WorkDir: "/work/sub",
			Flags:   Flags{BackupDir: "flagdir"},
			Env:     Env{"PAY_BACKUP_DIR": "envdir", "HOME": "/home/u"},
			Project: &File{Path: "/repo/pay.toml", Kind: KindProject, Exists: true, BackupDir: ".pay-backups"},
			User:    &File{Path: "/home/u/.config/pay/config.toml", Kind: KindUser, Exists: true, BackupDir: "/abs/user"},
		}
	}
	tests := []struct {
		name    string
		peel    func(*Input)
		wantDir string
		wantSrc string
	}{
		{"flag, relative to the working directory", func(*Input) {}, "/work/sub/flagdir", SourceFlag},
		{"env, relative to the working directory", func(in *Input) { in.Flags.BackupDir = "" },
			"/work/sub/envdir", "env:PAY_BACKUP_DIR"},
		{"set-but-empty env counts as unset", func(in *Input) {
			in.Flags.BackupDir = ""
			in.Env["PAY_BACKUP_DIR"] = ""
		}, "/repo/.pay-backups", "project:/repo/pay.toml"},
		{"project, relative to pay.toml's own directory", func(in *Input) {
			in.Flags.BackupDir = ""
			delete(in.Env, "PAY_BACKUP_DIR")
		}, "/repo/.pay-backups", "project:/repo/pay.toml"},
		{"user, absolute", func(in *Input) {
			in.Flags.BackupDir = ""
			delete(in.Env, "PAY_BACKUP_DIR")
			in.Project.BackupDir = ""
		}, "/abs/user", "user:/home/u/.config/pay/config.toml"},
		{"user, relative to config.toml's directory", func(in *Input) {
			in.Flags.BackupDir = ""
			delete(in.Env, "PAY_BACKUP_DIR")
			in.Project.BackupDir = ""
			in.User.BackupDir = "bk"
		}, "/home/u/.config/pay/bk", "user:/home/u/.config/pay/config.toml"},
		{"tilde expands to HOME", func(in *Input) { in.Flags.BackupDir = "~/pay-backups" },
			"/home/u/pay-backups", SourceFlag},
		{"nothing configured: off", func(in *Input) {
			in.Flags.BackupDir = ""
			delete(in.Env, "PAY_BACKUP_DIR")
			in.Project.BackupDir = ""
			in.User.BackupDir = ""
		}, "", SourceDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := stack()
			tc.peel(&in)
			got, err := Resolve(in)
			if err != nil {
				t.Fatal(err)
			}
			if got.BackupDir != tc.wantDir {
				t.Errorf("BackupDir = %q, want %q", got.BackupDir, tc.wantDir)
			}
			s := got.Explain()["backup_dir"]
			if s.Source != tc.wantSrc || s.Value != tc.wantDir {
				t.Errorf("explain = %+v, want {%q %q}", s, tc.wantDir, tc.wantSrc)
			}
		})
	}
}

func TestResolveNoBackup(t *testing.T) {
	got, err := Resolve(Input{Env: Env{"PAY_NO_BACKUP": "1", "PAY_BACKUP_DIR": "/b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoBackup || got.BackupDir != filepath.Clean("/b") {
		t.Fatalf("NoBackup=%v BackupDir=%q: --no-backup must not forget the directory", got.NoBackup, got.BackupDir)
	}
	if s := got.Explain()["no_backup"]; s.Source != "env:PAY_NO_BACKUP" || s.Value != true {
		t.Errorf("explain no_backup = %+v", s)
	}
	got, _ = Resolve(Input{Flags: Flags{NoBackup: boolp(false)}, Env: Env{"PAY_NO_BACKUP": "1"}})
	if got.NoBackup {
		t.Error("--no-backup=false must beat PAY_NO_BACKUP")
	}
	if _, err := Resolve(Input{Env: Env{"PAY_NO_BACKUP": "maybe"}}); err == nil {
		t.Error("a non-boolean PAY_NO_BACKUP must be rejected")
	}
}

// A relative flag/env value with no working directory stays relative rather
// than Resolve asking the OS for one (§3.1: Resolve is pure).
func TestResolveBackupDirWithoutWorkDir(t *testing.T) {
	got, err := Resolve(Input{Env: Env{"PAY_BACKUP_DIR": "rel/dir"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupDir != filepath.Clean("rel/dir") {
		t.Fatalf("BackupDir = %q", got.BackupDir)
	}
}

func TestParseBackupDirKey(t *testing.T) {
	f, err := Parse([]byte("backup_dir = \".pay-backups\"\n"), "/repo/pay.toml", KindProject)
	if err != nil {
		t.Fatal(err)
	}
	if f.BackupDir != ".pay-backups" || len(f.UnknownKeys) != 0 {
		t.Fatalf("BackupDir=%q unknown=%v", f.BackupDir, f.UnknownKeys)
	}
}
