//go:build windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows AF_UNIX has no peer UID check. Access is guarded by the verified
// directory owner and DACL, inherited by the socket and lock file.
func checkPeerUID(net.Conn, int) (int, error) { return -1, nil }

// privateDirSD is a protected DACL granting inheritable full control to sid only.
func privateDirSD(sid *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("build socket directory DACL: %w", err)
	}
	return sd, nil
}

// mkdirPrivate creates dir with the private DACL atomically, so it never
// carries the parent's inherited DACL.
func mkdirPrivate(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get socket owner SID: %w", err)
	}
	sd, err := privateDirSD(user.User.Sid)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// secureSocketDir applies a protected current-user DACL when tighten is set
// (dir created by zut or zut-owned), then verifies owner and DACL. Other dirs
// are never modified: they must already be private or startup fails.
func secureSocketDir(dir string, tighten bool) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get socket owner SID: %w", err)
	}
	if tighten {
		sd, err := privateDirSD(user.User.Sid)
		if err != nil {
			return err
		}
		acl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			return fmt.Errorf("protect socket directory %s: %w", dir, err)
		}
	}
	actual, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read socket directory security: %w", err)
	}
	owner, _, err := actual.Owner()
	if err != nil {
		return fmt.Errorf("read socket directory owner: %w", err)
	}
	if !socketDirOwnerOK(owner, user.User.Sid, token.IsElevated()) {
		return fmt.Errorf("socket dir %s is not owned by the current user", dir)
	}
	if err := checkPrivateDACL(actual, user.User.Sid); err != nil {
		return fmt.Errorf("socket dir %s %w; use a private directory or the default socket path", dir, err)
	}
	return nil
}

// socketDirOwnerOK accepts the current user, or the Administrators group
// (the default owner for objects created by an elevated token).
func socketDirOwnerOK(owner, user *windows.SID, elevated bool) bool {
	if owner == nil {
		return false
	}
	if owner.Equals(user) {
		return true
	}
	return elevated && owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// checkPrivateDACL requires a protected DACL with a single inheritable
// full-control ACE for user.
func checkPrivateDACL(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	got, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 || got == nil || got.AceCount != 1 {
		return fmt.Errorf("DACL is not private")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(got, 0, &ace); err != nil {
		return err
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || ace.Mask != (windows.STANDARD_RIGHTS_REQUIRED|windows.SYNCHRONIZE|0x1ff) || !sid.Equals(user) {
		return fmt.Errorf("DACL does not match current user")
	}
	return nil
}
