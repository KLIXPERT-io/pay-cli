package safety

import (
	"strings"
	"testing"
)

func TestOpLevel(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		want Level
	}{
		{"find is read", Op{Command: CmdFind}, L0},
		{"count is read", Op{Command: CmdCount}, L0},
		{"versions list is read", Op{Command: CmdVersionsList}, L0},
		{"download is read", Op{Command: CmdDownload}, L0},
		{"raw GET is read", Op{Command: CmdRaw, Method: "GET"}, L0},
		{"raw POST is a scoped write", Op{Command: CmdRaw, Method: "POST"}, L1},
		{"raw DELETE is destructive", Op{Command: CmdRaw, Method: "DELETE"}, L2},
		{"create", Op{Command: CmdCreate}, L1},
		{"upload", Op{Command: CmdUpload}, L1},
		{"duplicate", Op{Command: CmdDuplicate}, L1},
		{"update by id", Op{Command: CmdUpdate, Selector: SelectorID}, L1},
		{"publish by id", Op{Command: CmdPublish, Selector: SelectorID}, L1},
		{"restore by id", Op{Command: CmdRestore, Selector: SelectorID}, L1},
		{"unpublish by id", Op{Command: CmdUnpublish, Selector: SelectorID}, L2},
		{"globals update", Op{Command: CmdGlobalsUpdate, Global: true}, L2},
		{"versions restore", Op{Command: CmdVersionsRestore, Selector: SelectorID}, L2},
		{"soft delete on a trash collection", Op{Command: CmdDelete, Selector: SelectorID, TrashEnabled: true}, L1},
		{"delete with no trash", Op{Command: CmdDelete, Selector: SelectorID}, L2},
		{"permanent delete", Op{Command: CmdDelete, Selector: SelectorID, Permanent: true, TrashEnabled: true}, L2},
		{"bulk update", Op{Command: CmdUpdate, Selector: SelectorBulk}, L3},
		{"bulk delete", Op{Command: CmdDelete, Selector: SelectorBulk, TrashEnabled: true}, L3},
		{"bulk publish", Op{Command: CmdPublish, Selector: SelectorBulk}, L3},
		{"bulk unpublish", Op{Command: CmdUnpublish, Selector: SelectorBulk}, L3},
		{"case and space insensitive", Op{Command: "  Create "}, L1},
		{"unknown verb fails safe", Op{Command: "frobnicate"}, L2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.Level(); got != tc.want {
				t.Fatalf("Level() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLevelNames(t *testing.T) {
	for _, tc := range []struct {
		lvl     Level
		str     string
		name    string
		audited bool
	}{
		{L0, "L0", "read", false},
		{L1, "L1", "scoped write", true},
		{L2, "L2", "scoped destructive", true},
		{L3, "L3", "bulk", true},
	} {
		if got := tc.lvl.String(); got != tc.str {
			t.Errorf("String() = %q, want %q", got, tc.str)
		}
		if got := tc.lvl.Name(); got != tc.name {
			t.Errorf("Name() = %q, want %q", got, tc.name)
		}
		if got := tc.lvl.Audited(); got != tc.audited {
			t.Errorf("Audited() = %v, want %v", got, tc.audited)
		}
	}
}

func TestOpAction(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		want string
	}{
		{"create", Op{Command: CmdCreate}, ActionCreate},
		{"duplicate is a create", Op{Command: CmdDuplicate}, ActionCreate},
		{"upload", Op{Command: CmdUpload}, ActionUpload},
		{"update", Op{Command: CmdUpdate}, ActionUpdate},
		{"globals update is an update", Op{Command: CmdGlobalsUpdate}, ActionUpdate},
		{"publish", Op{Command: CmdPublish}, ActionPublish},
		{"unpublish", Op{Command: CmdUnpublish}, ActionUnpublish},
		{"restore", Op{Command: CmdRestore}, ActionRestore},
		{"versions restore", Op{Command: CmdVersionsRestore}, ActionRestoreVersion},
		{"soft delete is a trash", Op{Command: CmdDelete, TrashEnabled: true}, ActionTrash},
		{"permanent delete", Op{Command: CmdDelete, TrashEnabled: true, Permanent: true}, ActionDelete},
		{"delete without trash", Op{Command: CmdDelete}, ActionDelete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.Action(); got != tc.want {
				t.Fatalf("Action() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSoftDeleteAndIrreversible(t *testing.T) {
	tests := []struct {
		name         string
		op           Op
		soft         bool
		irreversible bool
	}{
		{"trash collection", Op{Command: CmdDelete, TrashEnabled: true}, true, false},
		{"no trash", Op{Command: CmdDelete}, false, true},
		{"permanent", Op{Command: CmdDelete, TrashEnabled: true, Permanent: true}, false, true},
		{"update is neither", Op{Command: CmdUpdate}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.SoftDelete(); got != tc.soft {
				t.Errorf("SoftDelete() = %v, want %v", got, tc.soft)
			}
			if got := tc.op.Irreversible(); got != tc.irreversible {
				t.Errorf("Irreversible() = %v, want %v", got, tc.irreversible)
			}
		})
	}
}

// TestIrreversibleReasonDistinguishesTheTwoCauses: Irreversible() is true for
// two different reasons and they must not share one sentence.
func TestIrreversibleReasonDistinguishesTheTwoCauses(t *testing.T) {
	tests := []struct {
		name    string
		op      Op
		want    string
		wantNot string
	}{
		{
			name:    "--permanent on a trash-enabled collection",
			op:      Op{Command: CmdDelete, Selector: SelectorID, Permanent: true, TrashEnabled: true},
			want:    "--permanent bypasses this collection's trash",
			wantNot: "no trash",
		},
		{
			name:    "collection without trash",
			op:      Op{Command: CmdDelete, Selector: SelectorID},
			want:    "this collection has no trash",
			wantNot: "--permanent",
		},
		{
			name:    "--permanent on a collection without trash",
			op:      Op{Command: CmdDelete, Selector: SelectorID, Permanent: true},
			want:    "this collection has no trash",
			wantNot: "bypasses",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.op.IrreversibleReason()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("IrreversibleReason() = %q, want it to contain %q", got, tc.want)
			}
			if strings.Contains(got, tc.wantNot) {
				t.Fatalf("IrreversibleReason() = %q, must not contain %q", got, tc.wantNot)
			}
		})
	}
	// A reversible operation has no reason to state.
	if got := (Op{Command: CmdDelete, Selector: SelectorID, TrashEnabled: true}).IrreversibleReason(); got != "" {
		t.Fatalf("a soft delete is reversible, reason = %q", got)
	}
}
