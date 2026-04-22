package provider

import (
	"fmt"
	"math/rand"
	"os/user"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func uniqueUsername(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano()%1_000_000, rand.Intn(1_000_000))
}

func TestAccUser_lifecycle(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfuser")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_user" "u" {
  name        = %q
  shell       = "/usr/sbin/nologin"
  system      = true
  create_home = false
}`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_user.u", "name", name),
					resource.TestCheckResourceAttr("sysutils_user.u", "shell", "/usr/sbin/nologin"),
					checkUserExists(name),
				),
			},
			{
				Config: fmt.Sprintf(`
resource "sysutils_user" "u" {
  name        = %q
  shell       = "/bin/sh"
  system      = true
  create_home = false
}`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_user.u", "shell", "/bin/sh"),
					checkUserShell(name, "/bin/sh"),
				),
			},
		},
	})

	if _, err := user.Lookup(name); err == nil {
		t.Fatalf("expected user %s to be deleted", name)
	}
}

func TestAccUser_explicitUID(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfuid")
	const uid = int64(60123)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_user" "u" {
  name        = %q
  uid         = %d
  system      = true
  create_home = false
}`, name, uid),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_user.u", "uid", fmt.Sprintf("%d", uid)),
					checkUserUID(name, fmt.Sprintf("%d", uid)),
				),
			},
		},
	})
}

func checkUserExists(name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := user.Lookup(name); err != nil {
			return fmt.Errorf("user %s not found: %w", name, err)
		}
		return nil
	}
}

func checkUserUID(name, wantUID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		u, err := user.Lookup(name)
		if err != nil {
			return err
		}
		if u.Uid != wantUID {
			return fmt.Errorf("uid for %s: got %s want %s", name, u.Uid, wantUID)
		}
		return nil
	}
}

func checkUserShell(name, wantShell string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := readLoginShell(name)
		if err != nil {
			return err
		}
		if got != wantShell {
			return fmt.Errorf("shell for %s: got %s want %s", name, got, wantShell)
		}
		return nil
	}
}
