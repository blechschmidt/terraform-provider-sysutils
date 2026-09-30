package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAlternatives_removeOnDestroyKeepsForeignSelection checks that destroy
// with remove_on_destroy unregisters the alternative the resource applied,
// not whichever one the link group points to after someone selected
// another alternative outside Terraform: refresh puts that one into path.
func TestAlternatives_removeOnDestroyKeepsForeignSelection(t *testing.T) {
	for _, kind := range []string{alternativesDebian, alternativesRHEL} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeAlternatives(t, kind)
			addEditorGroup(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				CheckDestroy: resource.ComposeAggregateTestCheckFunc(
					checkAltChanges(f, "remove editor /opt/editor"),
					checkAltGroup(f, "editor", true, "/bin/nano"),
					func(*terraform.State) error {
						if _, ok := (&alternativesStatus{Alternatives: f.get("editor").alts}).entry("/bin/nano"); !ok {
							return fmt.Errorf("/bin/nano was unregistered")
						}
						return nil
					},
				),
				Steps: []resource.TestStep{
					{
						Config: alternativesTF("editor", "/opt/editor", `  link              = "/usr/bin/editor"
  priority          = 10
  remove_on_destroy = true`),
						Check: checkAltGroup(f, "editor", true, "/opt/editor"),
					},
					{
						PreConfig: func() {
							f.set("editor", true, "/bin/nano")
							f.clearCalls()
						},
						RefreshState:       true,
						ExpectNonEmptyPlan: true,
						Check:              resource.TestCheckResourceAttr(testAlternativesResource, "path", "/bin/nano"),
					},
				},
			})
		})
	}
}

// untrustedAltTree creates, in a directory only root can write to, a
// directory owned by nobody with a program in it, a world-writable
// directory, a sticky world-writable directory, a world-writable program
// and a symlink to the program owned by nobody.
func untrustedAltTree(t *testing.T) (dir string) {
	t.Helper()
	requireRoot(t)
	dir = t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(p string, mode os.FileMode, uid int) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	prog := func(p string, mode os.FileMode, uid int) {
		mustWrite(t, p, "#!/bin/sh\n")
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join(dir, "safe"), 0o755, 0)
	mk(filepath.Join(dir, "nobody"), 0o755, 65534)
	mk(filepath.Join(dir, "open"), 0o777, 0)
	mk(filepath.Join(dir, "sticky"), os.ModeSticky|0o777, 0)
	prog(filepath.Join(dir, "safe", "tool"), 0o755, 0)
	prog(filepath.Join(dir, "safe", "writable"), 0o757, 0)
	prog(filepath.Join(dir, "safe", "theirs"), 0o755, 65534)
	prog(filepath.Join(dir, "nobody", "tool"), 0o755, 65534)
	if err := os.Symlink(filepath.Join(dir, "nobody", "tool"), filepath.Join(dir, "safe", "via-nobody")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tool", filepath.Join(dir, "safe", "relative")); err != nil {
		t.Fatal(err)
	}
	// A directory that nobody could swap for a symlink.
	if err := os.Symlink(filepath.Join(dir, "safe"), filepath.Join(dir, "nobody", "safe")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckAlternativesPaths(t *testing.T) {
	dir := untrustedAltTree(t)
	safeLink := filepath.Join(dir, "safe", "link")
	for _, tc := range []struct {
		link, path, wantErr string
	}{
		{safeLink, filepath.Join(dir, "safe", "tool"), ""},
		{"", filepath.Join(dir, "safe", "relative"), ""},
		// Missing directories and files are left to the tool.
		{filepath.Join(dir, "missing", "link"), filepath.Join(dir, "missing", "tool"), ""},
		{safeLink, filepath.Join(dir, "nobody", "tool"), "belongs to user 65534"},
		{safeLink, filepath.Join(dir, "safe", "via-nobody"), "belongs to user 65534"},
		{safeLink, filepath.Join(dir, "nobody", "safe", "tool"), "belongs to user 65534"},
		{safeLink, filepath.Join(dir, "safe", "theirs"), "belongs to user 65534"},
		{safeLink, filepath.Join(dir, "safe", "writable"), "is writable by other users"},
		{safeLink, filepath.Join(dir, "open", "tool"), "is writable by other users"},
		{filepath.Join(dir, "nobody", "link"), filepath.Join(dir, "safe", "tool"), "belongs to user 65534"},
		{filepath.Join(dir, "open", "link"), filepath.Join(dir, "safe", "tool"), "is writable by other users"},
		{filepath.Join(dir, "sticky", "link"), filepath.Join(dir, "safe", "tool"), "is writable by other users, who could replace the link"},
	} {
		err := checkAlternativesPaths(tc.link, tc.path)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("checkAlternativesPaths(%q, %q) = %v, want nil", tc.link, tc.path, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("checkAlternativesPaths(%q, %q) = %v, want an error containing %q", tc.link, tc.path, err, tc.wantErr)
		}
	}
}

// TestAlternatives_untrustedPathRefused checks that the resource refuses,
// before running any command, to select or register an alternative that
// another user could replace, or a link another user could redirect.
func TestAlternatives_untrustedPathRefused(t *testing.T) {
	dir := untrustedAltTree(t)
	f := newFakeAlternatives(t, alternativesDebian)
	f.checkPaths = checkAlternativesPaths
	theirs := filepath.Join(dir, "nobody", "tool")
	f.add("editor", "/usr/bin/editor",
		alternativeEntry{Path: "/bin/nano", Priority: 40},
		alternativeEntry{Path: theirs, Priority: 30})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:      alternativesTF("editor", theirs, ""),
				ExpectError: regexp.MustCompile(`Untrusted path(.|\n)*belongs\s+to\s+user\s+65534`),
				Check:       checkAltChanges(f),
			},
			{
				Config: alternativesTF("editor", filepath.Join(dir, "safe", "tool"), fmt.Sprintf(`  link     = %q
  priority = 10`, filepath.Join(dir, "sticky", "editor"))),
				ExpectError: regexp.MustCompile(`Untrusted path(.|\n)*writable\s+by\s+other\s+users`),
			},
			{
				// Nothing was changed by the refused applies.
				Config:             alternativesTF("editor", "/bin/nano", ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
				PreConfig: func() {
					if got := f.changes(); len(got) != 0 {
						t.Errorf("commands run: %q", got)
					}
				},
			},
		},
	})
}
