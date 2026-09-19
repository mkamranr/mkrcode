//go:build windows

package secureio

import (
	"strings"
	"testing"
)

// The Windows confidentiality guarantee lives entirely in this SDDL string,
// because the file mode bits the rest of the code passes around have no
// effect on this platform. A CI run on a real Windows host showed the audit
// log being created mode 0666, which is what prompted this package.
func TestSDDLIsOwnerOnly(t *testing.T) {
	sddl, err := buildSDDL()
	if err != nil {
		t.Fatalf("buildSDDL: %v", err)
	}

	// Protected: the DACL must not inherit permissive entries from a parent
	// directory that happens to grant Users access.
	if !strings.HasPrefix(sddl, "D:P") {
		t.Errorf("SDDL %q does not start with D:P; an unprotected DACL inherits parent permissions", sddl)
	}

	// The current user's SID must be present, or the owner cannot read
	// their own audit log.
	if !strings.Contains(sddl, "S-1-5-21") && !strings.Contains(sddl, "S-1-5-") {
		t.Errorf("SDDL %q contains no user SID", sddl)
	}

	// The well-known groups that must NOT appear. WD is Everyone, BU is
	// Builtin Users, AU is Authenticated Users. Any of these would make the
	// file readable by other people on the machine.
	for _, bad := range []string{";WD)", ";BU)", ";AU)", ";IU)"} {
		if strings.Contains(sddl, bad) {
			t.Errorf("SDDL %q grants access to a broad group (%s)", sddl, bad)
		}
	}

	// LocalSystem and Administrators are deliberately included; excluding
	// them would break backup without improving confidentiality.
	for _, want := range []string{";SY)", ";BA)"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("SDDL %q is missing %s", sddl, want)
		}
	}
}

func TestSecurityAttributesBuild(t *testing.T) {
	sa, free, err := securityAttributes()
	if err != nil {
		t.Fatalf("securityAttributes: %v", err)
	}
	defer free()

	if sa.SecurityDescriptor == 0 {
		t.Error("no security descriptor was produced")
	}
	if sa.InheritHandle != 0 {
		t.Error("the handle must not be inheritable by child processes")
	}
	if sa.Length == 0 {
		t.Error("Length must be set or the API ignores the structure")
	}
}
