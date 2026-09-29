package provider

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// checkOwnedBy checks that p has mode perm and is owned by the user name
// and their primary group.
func checkOwnedBy(p, name string, perm os.FileMode) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&(os.ModeType|os.ModePerm) != perm {
		return fmt.Errorf("%s has mode %v, want %v", p, info.Mode(), perm)
	}
	st := info.Sys().(*syscall.Stat_t)
	if strconv.FormatUint(uint64(st.Uid), 10) != u.Uid || strconv.FormatUint(uint64(st.Gid), 10) != u.Gid {
		return fmt.Errorf("%s is owned by %d:%d, want %s:%s", p, st.Uid, st.Gid, u.Uid, u.Gid)
	}
	return nil
}

func TestAccSSHAuthorizedKey_realUser(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfssh")
	home := filepath.Join(t.TempDir(), name)
	sshDir := filepath.Join(home, ".ssh")
	keys := filepath.Join(sshDir, "authorized_keys")
	pub := testSSHKey(t, 7)
	keyText := testSSHKeyText(pub)
	fp := sshKeyFingerprint(pub)
	other := testSSHKeyText(testSSHKey(t, 8)) + " added by hand"
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config := fmt.Sprintf(`
resource "sysutils_user" "u" {
  name        = %q
  home        = %q
  create_home = true
  shell       = "/bin/sh"
}

resource "sysutils_ssh_authorized_key" "test" {
  user    = sysutils_user.u.name
  key     = %q
  options = ["from=\"127.0.0.1\"", "no-agent-forwarding"]
}
`, name, home, keyText+" "+name+"@test")
	wantLine := `from="127.0.0.1",no-agent-forwarding ` + keyText + " " + name + "@test"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			// The user is gone, and userdel keeps the home directory; the
			// managed line went before the user did.
			data, err := os.ReadFile(keys)
			if err != nil {
				return err
			}
			if string(data) != other+"\n" {
				return fmt.Errorf("%s after destroy = %q, want only the hand-added key", keys, data)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSSHKeyResource, "id", name+":"+fp),
					resource.TestCheckResourceAttr(testSSHKeyResource, "path", keys),
					resource.TestCheckResourceAttr(testSSHKeyResource, "line", wantLine),
					func(*terraform.State) error {
						data, err := os.ReadFile(keys)
						if err != nil {
							return err
						}
						if string(data) != wantLine+"\n" {
							return fmt.Errorf("%s = %q", keys, data)
						}
						if err := checkOwnedBy(sshDir, name, os.ModeDir|0o700); err != nil {
							return err
						}
						return checkOwnedBy(keys, name, 0o600)
					},
				),
			},
			{
				ResourceName:      testSSHKeyResource,
				ImportState:       true,
				ImportStateId:     name + ":" + fp,
				ImportStateVerify: true,
			},
			{
				// Removed outside Terraform, next to a key added by hand.
				PreConfig: func() {
					if err := os.WriteFile(keys, []byte(other+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSSHKeyResource, plancheck.ResourceActionCreate)},
				},
				Check: func(*terraform.State) error {
					data, err := os.ReadFile(keys)
					if err != nil {
						return err
					}
					if string(data) != other+"\n"+wantLine+"\n" {
						return fmt.Errorf("%s = %q", keys, data)
					}
					return checkOwnedBy(keys, name, 0o600)
				},
			},
			{
				// The user points authorized_keys at a file only root may
				// read: refused, and the file is neither read into the
				// user's file nor changed.
				PreConfig: func() {
					if err := os.Remove(keys); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(secret, keys); err != nil {
						t.Fatal(err)
					}
				},
				Config:      config,
				ExpectError: regexp.MustCompile(`is\s+a\s+symbolic\s+link`),
			},
			{
				PreConfig: func() {
					if data, _ := os.ReadFile(secret); string(data) != "top secret\n" {
						t.Errorf("symlink target changed to %q", data)
					}
					if err := os.Remove(keys); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(keys, []byte(wantLine+"\n"+other+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})

	if _, err := user.Lookup(name); err == nil {
		t.Errorf("user %s still exists", name)
	}
}
