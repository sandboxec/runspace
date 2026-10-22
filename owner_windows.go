//go:build windows

//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// ownedByCurrentUser reports whether path is owned by the user of this
// process. Like git, it also accepts an owner of BUILTIN\Administrators when
// the process runs with that group's rights. A file system with no owner
// information, such as FAT, is not owned.
func ownedByCurrentUser(path string) (bool, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if errors.Is(err, windows.ERROR_NO_SECURITY_ON_OBJECT) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get the owner of %q: %w", path, err)
	}

	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, fmt.Errorf("read the owner of %q: %w", path, err)
	}

	if owner == nil {
		return false, nil
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, fmt.Errorf("get the current user: %w", err)
	}

	if owner.Equals(user.User.Sid) {
		return true, nil
	}

	return ownedByAdministrators(owner)
}

// ownedByAdministrators reports whether owner is BUILTIN\Administrators and
// the process token is a member of that group.
func ownedByAdministrators(owner *windows.SID) (bool, error) {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("get the Administrators group: %w", err)
	}

	if !owner.Equals(admins) {
		return false, nil
	}

	member, err := windows.Token(0).IsMember(admins)
	if err != nil {
		return false, fmt.Errorf("check membership of the Administrators group: %w", err)
	}

	return member, nil
}
