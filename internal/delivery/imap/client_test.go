package imap

import (
	"errors"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

type fileArg struct {
	uid    goimap.UID
	folder string
}

// fakeFiler records filing ops and rejects move/copy into a folder that doesn't
// yet "exist", so the create-on-failure retry is exercised.
type fakeFiler struct {
	capset   goimap.CapSet
	exists   map[string]bool
	moved    []fileArg
	copied   []fileArg
	deleted  []goimap.UID
	expunged []goimap.UID
	created  []string
}

func (f *fakeFiler) caps() goimap.CapSet { return f.capset }

func (f *fakeFiler) create(mailbox string) error {
	f.created = append(f.created, mailbox)
	f.exists[mailbox] = true
	return nil
}

func (f *fakeFiler) move(uid goimap.UID, mailbox string) error {
	if !f.exists[mailbox] {
		return errors.New("no such mailbox")
	}
	f.moved = append(f.moved, fileArg{uid, mailbox})
	return nil
}

func (f *fakeFiler) copy(uid goimap.UID, mailbox string) error {
	if !f.exists[mailbox] {
		return errors.New("no such mailbox")
	}
	f.copied = append(f.copied, fileArg{uid, mailbox})
	return nil
}

func (f *fakeFiler) markDeleted(uid goimap.UID) error {
	f.deleted = append(f.deleted, uid)
	return nil
}

func (f *fakeFiler) uidExpunge(uid goimap.UID) error {
	f.expunged = append(f.expunged, uid)
	return nil
}

func capSet(caps ...goimap.Cap) goimap.CapSet {
	s := goimap.CapSet{}
	for _, c := range caps {
		s[c] = struct{}{}
	}
	return s
}

func TestFileByLadder_MovePathCreatesAndMoves(t *testing.T) {
	f := &fakeFiler{capset: capSet(goimap.CapMove), exists: map[string]bool{}}
	warned := 0
	if err := fileByLadder(f, 7, "Triage/Done", func() { warned++ }); err != nil {
		t.Fatalf("file: %v", err)
	}
	// folder didn't exist: create then move
	if len(f.created) != 1 || f.created[0] != "Triage/Done" {
		t.Fatalf("folder not created on first use: %v", f.created)
	}
	if len(f.moved) != 1 || f.moved[0] != (fileArg{7, "Triage/Done"}) {
		t.Fatalf("move: got %v, want [{7 Triage/Done}]", f.moved)
	}
	if len(f.copied) != 0 {
		t.Error("MOVE path must not COPY")
	}
	if warned != 0 {
		t.Error("a MOVE-capable server must not warn about duplication")
	}
}

func TestFileByLadder_UIDPlusCopiesDeletesAndExpungesOneUID(t *testing.T) {
	f := &fakeFiler{capset: capSet(goimap.CapUIDPlus), exists: map[string]bool{"Triage/Done": true}}
	if err := fileByLadder(f, 9, "Triage/Done", func() { t.Error("UIDPLUS path must not warn") }); err != nil {
		t.Fatalf("file: %v", err)
	}
	if len(f.copied) != 1 || f.copied[0] != (fileArg{9, "Triage/Done"}) {
		t.Fatalf("copy: %v", f.copied)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 9 {
		t.Fatalf("must flag exactly the one uid deleted: %v", f.deleted)
	}
	if len(f.expunged) != 1 || f.expunged[0] != 9 {
		t.Fatalf("must UID EXPUNGE exactly the one uid (never a bare EXPUNGE): %v", f.expunged)
	}
	if len(f.moved) != 0 {
		t.Error("UIDPLUS path must not MOVE")
	}
}

func TestFileByLadder_CopyOnlyPathWarnsAndDuplicates(t *testing.T) {
	f := &fakeFiler{capset: capSet(), exists: map[string]bool{"Done": true}} // neither MOVE nor UIDPLUS
	warned := 0
	if err := fileByLadder(f, 1, "Done", func() { warned++ }); err != nil {
		t.Fatalf("file: %v", err)
	}
	if len(f.copied) != 1 {
		t.Fatalf("copy-only path must copy: %v", f.copied)
	}
	if len(f.deleted) != 0 || len(f.expunged) != 0 {
		t.Error("copy-only path must not delete/expunge (it can't safely)")
	}
	if warned != 1 {
		t.Errorf("copy-only path must invoke the duplication warner: got %d", warned)
	}
}

func TestFolderPath(t *testing.T) {
	cases := []struct {
		prefix, delim, leaf, want string
	}{
		{"Triage", "/", "Needs Review", "Triage/Needs Review"},
		{"Triage", "\\", "Escalated", "Triage\\Escalated"}, // Exchange-style delimiter
		{"", "/", "Needs Review", "Needs Review"},          // no prefix
	}
	for _, tc := range cases {
		m := &imapMailbox{folderPrefix: tc.prefix, delim: tc.delim}
		if got := m.folderPath(tc.leaf); got != tc.want {
			t.Errorf("folderPath(%q) with prefix %q delim %q = %q, want %q", tc.leaf, tc.prefix, tc.delim, got, tc.want)
		}
	}
}
