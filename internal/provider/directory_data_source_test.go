package provider

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const testDirDataSource = "data.sysutils_directory.test"

func directoryDataSourceConfig(path string) string {
	return fmt.Sprintf(`
data "sysutils_directory" "test" {
  path = %q
}`, path)
}

func TestAccDirectoryDataSource_existing(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	for _, name := range []string{"b.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Dangling symlinks are listed too; entries are names, not targets.
	if err := os.Symlink("/nonexistent", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// Nested entries must not appear: the listing is not recursive.
	if err := os.WriteFile(filepath.Join(dir, "a", "nested"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Chmod explicitly so the setgid bit is set regardless of the umask.
	if err := os.Chmod(dir, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: directoryDataSourceConfig(dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "id", dir),
					resource.TestCheckResourceAttr(testDirDataSource, "path", dir),
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "true"),
					resource.TestCheckResourceAttr(testDirDataSource, "mode", "2750"),
					resource.TestCheckResourceAttr(testDirDataSource, "owner", "root"),
					resource.TestCheckResourceAttr(testDirDataSource, "group", "root"),
					resource.TestCheckResourceAttr(testDirDataSource, "uid", "0"),
					resource.TestCheckResourceAttr(testDirDataSource, "gid", "0"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.#", "4"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.0", ".hidden"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.1", "a"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.2", "b.txt"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.3", "link"),
				),
			},
		},
	})
}

func TestAccDirectoryDataSource_numericOwnership(t *testing.T) {
	requireRoot(t)

	uid := unusedID(t, func(id string) error { _, err := user.LookupId(id); return err })
	gid := unusedID(t, func(id string) error { _, err := user.LookupGroupId(id); return err })
	dir := filepath.Join(t.TempDir(), "orphaned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	uidStr, gidStr := strconv.Itoa(uid), strconv.Itoa(gid)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: directoryDataSourceConfig(dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "true"),
					resource.TestCheckResourceAttr(testDirDataSource, "mode", "0700"),
					resource.TestCheckResourceAttr(testDirDataSource, "owner", uidStr),
					resource.TestCheckResourceAttr(testDirDataSource, "group", gidStr),
					resource.TestCheckResourceAttr(testDirDataSource, "uid", uidStr),
					resource.TestCheckResourceAttr(testDirDataSource, "gid", gidStr),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.#", "0"),
				),
			},
		},
	})
}

func TestAccDirectoryDataSource_missing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: directoryDataSourceConfig(dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "id", dir),
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "false"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "mode"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "owner"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "group"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "uid"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "gid"),
					resource.TestCheckNoResourceAttr(testDirDataSource, "entries.#"),
				),
			},
		},
	})
}

func TestAccDirectoryDataSource_followsSymlink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o711); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: directoryDataSourceConfig(link),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "true"),
					resource.TestCheckResourceAttr(testDirDataSource, "mode", "0711"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.#", "1"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.0", "inside"),
				),
			},
		},
	})
}

func TestAccDirectoryDataSource_root(t *testing.T) {
	// "/" may be read even though resources refuse to manage it.
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: directoryDataSourceConfig("/"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "true"),
					resource.TestCheckResourceAttrSet(testDirDataSource, "mode"),
					resource.TestCheckResourceAttrSet(testDirDataSource, "entries.#"),
				),
			},
		},
	})
}

func TestAccDirectoryDataSource_errors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      directoryDataSourceConfig(file),
				ExpectError: regexp.MustCompile(`Not a directory`),
			},
			{
				Config:      directoryDataSourceConfig("relative/dir"),
				ExpectError: regexp.MustCompile(`must be absolute`),
			},
			{
				Config:      directoryDataSourceConfig("/tmp/../etc"),
				ExpectError: regexp.MustCompile(`must be in canonical form`),
			},
		},
	})
}

// TestAccDirectoryDataSource_withResource reads a directory managed in the
// same configuration, so the data source is deferred until apply and must
// reflect the resource's result, including later in-place updates.
func TestAccDirectoryDataSource_withResource(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "managed")
	config := func(mode string) string {
		return fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = %q
}

resource "sysutils_file" "test" {
  path    = "${sysutils_directory.test.path}/config.txt"
  content = "hello"
}

data "sysutils_directory" "test" {
  path       = sysutils_directory.test.path
  depends_on = [sysutils_file.test]
}`, dir, mode)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config("0750"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "exists", "true"),
					resource.TestCheckResourceAttrPair(testDirDataSource, "mode", testDirResource, "mode"),
					resource.TestCheckResourceAttrPair(testDirDataSource, "owner", testDirResource, "owner"),
					resource.TestCheckResourceAttrPair(testDirDataSource, "group", testDirResource, "group"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.#", "1"),
					resource.TestCheckResourceAttr(testDirDataSource, "entries.0", "config.txt"),
				),
			},
			{
				Config: config("0700"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirDataSource, "mode", "0700"),
				),
			},
		},
	})
}

// unusedID returns a numeric ID for which lookup fails, i.e. one with no
// passwd/group entry, so that name resolution falls back to the number.
func unusedID(t *testing.T, lookup func(id string) error) int {
	t.Helper()
	for id := 54321; id < 54321+1000; id++ {
		if lookup(strconv.Itoa(id)) != nil {
			return id
		}
	}
	t.Fatal("no unused numeric ID found")
	return 0
}
