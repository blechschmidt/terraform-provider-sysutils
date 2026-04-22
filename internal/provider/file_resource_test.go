package provider

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccFile_createReadUpdateDelete(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")

	config := func(content, mode string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = %q
  mode    = %q
}`, path, content, mode)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config("hello\n", "0640"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_file.test", "content", "hello\n"),
					resource.TestCheckResourceAttr("sysutils_file.test", "mode", "0640"),
					checkFileContent(path, "hello\n"),
					checkFileMode(path, 0o640),
				),
			},
			{
				Config: config("updated\n", "0600"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_file.test", "content", "updated\n"),
					resource.TestCheckResourceAttr("sysutils_file.test", "mode", "0600"),
					checkFileContent(path, "updated\n"),
					checkFileMode(path, 0o600),
				),
			},
		},
	})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, stat err = %v", err)
	}
}

func TestAccFile_ownership(t *testing.T) {
	requireRoot(t)

	// Use an existing low-privilege account that's guaranteed to exist in a
	// Debian-based container. "nobody" is also in the "nogroup" group there.
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody user not available: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "owned.txt")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "owned" {
  path    = %q
  content = "x"
  owner   = "nobody"
}`, path),
				Check: checkFileUID(path, u.Uid),
			},
		},
	})
}

func checkFileContent(path, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("content mismatch at %s: got %q want %q", path, string(got), want)
		}
		return nil
	}
}

func checkFileMode(path string, want os.FileMode) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if got := info.Mode().Perm(); got != want {
			return fmt.Errorf("mode mismatch at %s: got %#o want %#o", path, got, want)
		}
		return nil
	}
}

func checkFileUID(path, wantUID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat_t unavailable on this platform")
		}
		got := strconv.FormatUint(uint64(st.Uid), 10)
		if got != wantUID {
			return fmt.Errorf("uid mismatch at %s: got %s want %s", path, got, wantUID)
		}
		return nil
	}
}
