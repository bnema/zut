//go:build windows

package daemon

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows AF_UNIX has no peer UID check. Access is guarded by the verified
// directory DACL, inherited by the socket and lock file.
func checkPeerUID(net.Conn, int) (int, error) { return -1, nil }

func secureSocketDir(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get socket owner SID: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return fmt.Errorf("build socket directory DACL: %w", err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return fmt.Errorf("protect socket directory: %w", err)
	}
	actual, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read socket directory DACL: %w", err)
	}
	control, _, err := actual.Control()
	if err != nil {
		return err
	}
	got, _, err := actual.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 || got == nil || got.AceCount != 1 {
		return fmt.Errorf("socket directory DACL is not private")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(got, 0, &ace); err != nil {
		return err
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || ace.Mask != (windows.STANDARD_RIGHTS_REQUIRED|windows.SYNCHRONIZE|0x1ff) || !sid.Equals(user.User.Sid) {
		return fmt.Errorf("socket directory DACL does not match current user")
	}
	return nil
}
