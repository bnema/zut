//go:build windows

package daemon

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecureSocketDirWindows(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// A temp dir has an inherited, non-protected DACL: verify-only must
	// refuse it and leave it untouched.
	open := t.TempDir()
	before, err := windows.GetNamedSecurityInfo(open, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureSocketDir(open, false); err == nil {
		t.Fatal("non-private dir accepted without tightening")
	}
	after, err := windows.GetNamedSecurityInfo(open, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatalf("verify-only changed DACL: %s -> %s", before, after)
	}
	if err := prepareSocketDir(open, nil); err == nil {
		t.Fatal("prepareSocketDir accepted a foreign non-private dir")
	}

	// Tightening a zut-owned dir makes it private; a later verify passes.
	owned := t.TempDir()
	if err := prepareSocketDir(owned, []string{owned}); err != nil {
		t.Fatal(err)
	}
	if err := secureSocketDir(owned, false); err != nil {
		t.Fatalf("tightened dir not private: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(owned, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDACL(sd, user.User.Sid); err != nil {
		t.Fatal(err)
	}

	// A missing dir (and its parents) is created and protected.
	created := filepath.Join(t.TempDir(), "a", "run")
	if err := prepareSocketDir(created, nil); err != nil {
		t.Fatal(err)
	}
	if err := secureSocketDir(created, false); err != nil {
		t.Fatalf("created dir not private: %v", err)
	}

	// mkdirPrivate yields a private DACL directly, with no tightening step.
	direct := filepath.Join(t.TempDir(), "direct")
	if err := mkdirPrivate(direct); err != nil {
		t.Fatal(err)
	}
	if err := secureSocketDir(direct, false); err != nil {
		t.Fatalf("mkdirPrivate dir not private: %v", err)
	}
	if err := mkdirPrivate(direct); err == nil {
		t.Fatal("mkdirPrivate succeeded on an existing dir")
	}

	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if !socketDirOwnerOK(user.User.Sid, user.User.Sid, false) || socketDirOwnerOK(admins, user.User.Sid, false) || !socketDirOwnerOK(admins, user.User.Sid, true) || socketDirOwnerOK(nil, user.User.Sid, true) {
		t.Fatal("owner check")
	}
}
