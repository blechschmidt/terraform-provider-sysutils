package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"os/user"
	"strconv"
	"syscall"
)

// Unix permission bits for the special modes, as written in octal mode strings.
const (
	octalSetuid = 0o4000
	octalSetgid = 0o2000
	octalSticky = 0o1000

	// maxOctalMode is the largest value a mode string may hold: permission
	// bits plus setuid, setgid and sticky.
	maxOctalMode = 0o7777
)

// parseMode parses an octal mode string such as "0644", "755" or "4755" into
// an fs.FileMode. The setuid (04000), setgid (02000) and sticky (01000) bits
// are translated into their fs.ModeSetuid, fs.ModeSetgid and fs.ModeSticky
// equivalents so the result can be passed directly to os.Chmod. Values that
// are empty, not octal, or larger than 07777 are rejected.
func parseMode(s string) (fs.FileMode, error) {
	if s == "" {
		return 0, fmt.Errorf("invalid octal mode %q: must not be empty", s)
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %q: %w", s, err)
	}
	if v > maxOctalMode {
		return 0, fmt.Errorf("invalid octal mode %q: exceeds %#o", s, maxOctalMode)
	}

	mode := fs.FileMode(v) & fs.ModePerm
	if v&octalSetuid != 0 {
		mode |= fs.ModeSetuid
	}
	if v&octalSetgid != 0 {
		mode |= fs.ModeSetgid
	}
	if v&octalSticky != 0 {
		mode |= fs.ModeSticky
	}
	return mode, nil
}

// formatMode renders the permission and special bits of m as a four-digit
// octal string (e.g. "0644", "4755"), the inverse of parseMode. File type bits
// such as fs.ModeDir are ignored.
func formatMode(m fs.FileMode) string {
	v := uint32(m & fs.ModePerm)
	if m&fs.ModeSetuid != 0 {
		v |= octalSetuid
	}
	if m&fs.ModeSetgid != 0 {
		v |= octalSetgid
	}
	if m&fs.ModeSticky != 0 {
		v |= octalSticky
	}
	return fmt.Sprintf("%04o", v)
}

// parseID parses a numeric UID/GID string. Negative values and values that
// do not fit in a uint32 are rejected.
func parseID(s string) (int, bool) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// lookupUID resolves a username to a UID. If no user with that name exists
// and name is a non-negative integer, it is used as a numeric UID.
func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			if id, ok := parseID(name); ok {
				return id, nil
			}
		}
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

// lookupGID resolves a group name to a GID. If no group with that name exists
// and name is a non-negative integer, it is used as a numeric GID.
func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			if id, ok := parseID(name); ok {
				return id, nil
			}
		}
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// resolveOwnership resolves owner and group names (or numeric IDs) to a UID
// and GID. An empty value resolves to -1, which chown interprets as "leave
// unchanged".
func resolveOwnership(owner, group string) (uid, gid int, err error) {
	uid, gid = -1, -1
	if owner != "" {
		if uid, err = lookupUID(owner); err != nil {
			return -1, -1, fmt.Errorf("looking up owner %q: %w", owner, err)
		}
	}
	if group != "" {
		if gid, err = lookupGID(group); err != nil {
			return -1, -1, fmt.Errorf("looking up group %q: %w", group, err)
		}
	}
	return uid, gid, nil
}

// uidName returns the username for uid, or the decimal UID if it has no
// passwd entry.
func uidName(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil {
		return u.Username
	}
	return id
}

// gidName returns the group name for gid, or the decimal GID if it has no
// group entry.
func gidName(gid uint32) string {
	id := strconv.FormatUint(uint64(gid), 10)
	if g, err := user.LookupGroupId(id); err == nil {
		return g.Name
	}
	return id
}

// ownerAndGroup returns the owner and group names of st, falling back to
// numeric IDs for accounts without a name.
func ownerAndGroup(st *syscall.Stat_t) (owner, group string) {
	return uidName(st.Uid), gidName(st.Gid)
}

// reconcileOwner returns the value to store in state for an owner attribute
// whose configured value is configured and whose actual UID is uid. If the
// configured value (name or numeric) resolves to uid it is kept verbatim so a
// numeric configuration does not show a spurious diff against the name;
// otherwise the actual owner name is returned.
func reconcileOwner(configured string, uid uint32) string {
	if configured != "" {
		if id, err := lookupUID(configured); err == nil && uint64(id) == uint64(uid) {
			return configured
		}
	}
	return uidName(uid)
}

// reconcileGroup is the group counterpart of reconcileOwner.
func reconcileGroup(configured string, gid uint32) string {
	if configured != "" {
		if id, err := lookupGID(configured); err == nil && uint64(id) == uint64(gid) {
			return configured
		}
	}
	return gidName(gid)
}

// reconcileMode returns the value to store in state for a mode attribute
// whose configured value is configured and whose actual mode is actual. If
// the configured string (e.g. "755") parses to the actual mode it is kept
// verbatim so equivalent spellings do not produce a diff; otherwise the
// canonical four-digit form of actual is returned.
func reconcileMode(configured string, actual fs.FileMode) string {
	if configured != "" {
		if m, err := parseMode(configured); err == nil && m == actual&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) {
			return configured
		}
	}
	return formatMode(actual)
}
