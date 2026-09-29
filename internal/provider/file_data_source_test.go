package provider

import (
	"crypto/md5" //nolint:gosec // Only used to compute the expected content_md5.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const testFileDataSource = "data.sysutils_file.test"

func fileDataSourceConfig(path string) string {
	return fmt.Sprintf(`
data "sysutils_file" "test" {
  path = %q
}`, path)
}

func fileDataSourceFollowConfig(path string, follow bool) string {
	return fmt.Sprintf(`
data "sysutils_file" "test" {
  path            = %q
  follow_symlinks = %t
}`, path, follow)
}

// expectedFileChecks returns checks for all content and metadata attributes
// of the data source against the file at p, as observed by the test itself.
func expectedFileChecks(t *testing.T, p string, content []byte) resource.TestCheckFunc {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	owner, group := ownerAndGroup(st)
	sha := sha256.Sum256(content)
	md := md5.Sum(content) //nolint:gosec // See import.
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(testFileDataSource, "content_base64", base64.StdEncoding.EncodeToString(content)),
		resource.TestCheckResourceAttr(testFileDataSource, "content_sha256", hex.EncodeToString(sha[:])),
		resource.TestCheckResourceAttr(testFileDataSource, "content_md5", hex.EncodeToString(md[:])),
		resource.TestCheckResourceAttr(testFileDataSource, "size", strconv.Itoa(len(content))),
		resource.TestCheckResourceAttr(testFileDataSource, "mode", formatMode(info.Mode())),
		resource.TestCheckResourceAttr(testFileDataSource, "owner", owner),
		resource.TestCheckResourceAttr(testFileDataSource, "group", group),
		resource.TestCheckResourceAttr(testFileDataSource, "uid", strconv.FormatUint(uint64(st.Uid), 10)),
		resource.TestCheckResourceAttr(testFileDataSource, "gid", strconv.FormatUint(uint64(st.Gid), 10)),
		resource.TestCheckResourceAttr(testFileDataSource, "modified", info.ModTime().UTC().Format(time.RFC3339)),
	)
}

func TestAccFileDataSource_regular(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "hello.conf")
	content := []byte("greeting = hi\nüñï\n")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Chmod explicitly so the setgid bit is set regardless of the umask.
	if err := os.Chmod(p, 0o640|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, 2, 29, 12, 34, 56, 789, time.FixedZone("CET", 3600))
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fileDataSourceConfig(p),
				Check: resource.ComposeAggregateTestCheckFunc(
					expectedFileChecks(t, p, content),
					resource.TestCheckResourceAttr(testFileDataSource, "id", p),
					resource.TestCheckResourceAttr(testFileDataSource, "path", p),
					resource.TestCheckNoResourceAttr(testFileDataSource, "follow_symlinks"),
					resource.TestCheckResourceAttr(testFileDataSource, "content", string(content)),
					resource.TestCheckResourceAttr(testFileDataSource, "mode", "2640"),
					resource.TestCheckResourceAttr(testFileDataSource, "owner", "root"),
					resource.TestCheckResourceAttr(testFileDataSource, "group", "root"),
					resource.TestCheckResourceAttr(testFileDataSource, "uid", "0"),
					resource.TestCheckResourceAttr(testFileDataSource, "gid", "0"),
					resource.TestCheckResourceAttr(testFileDataSource, "modified", "2024-02-29T11:34:56Z"),
				),
			},
		},
	})
}

func TestAccFileDataSource_empty(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fileDataSourceConfig(p),
				Check: resource.ComposeAggregateTestCheckFunc(
					expectedFileChecks(t, p, nil),
					resource.TestCheckResourceAttr(testFileDataSource, "content", ""),
					resource.TestCheckResourceAttr(testFileDataSource, "size", "0"),
				),
			},
		},
	})
}

func TestAccFileDataSource_binary(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "blob.bin")
	// Invalid UTF-8, including a NUL byte and a lone continuation byte.
	content := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe, 0x80, '\n'}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fileDataSourceConfig(p),
				Check: resource.ComposeAggregateTestCheckFunc(
					expectedFileChecks(t, p, content),
					resource.TestCheckNoResourceAttr(testFileDataSource, "content"),
				),
			},
		},
	})
}

func TestAccFileDataSource_symlink(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	content := []byte("secret\n")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      fileDataSourceConfig(link),
				ExpectError: regexp.MustCompile(`Symbolic\s+link\s+refused`),
			},
			{
				Config:      fileDataSourceFollowConfig(link, false),
				ExpectError: regexp.MustCompile(`Set\s+follow_symlinks\s+=\s+true`),
			},
			{
				Config: fileDataSourceFollowConfig(link, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					// Metadata describes the target, not the link.
					expectedFileChecks(t, target, content),
					resource.TestCheckResourceAttr(testFileDataSource, "id", link),
					resource.TestCheckResourceAttr(testFileDataSource, "path", link),
					resource.TestCheckResourceAttr(testFileDataSource, "follow_symlinks", "true"),
					resource.TestCheckResourceAttr(testFileDataSource, "content", string(content)),
					resource.TestCheckResourceAttr(testFileDataSource, "mode", "0600"),
				),
			},
		},
	})
}

func TestAccFileDataSource_symlinkToDirectory(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nonexistent"), dangling); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      fileDataSourceConfig(link),
				ExpectError: regexp.MustCompile(`Symbolic\s+link\s+refused`),
			},
			{
				Config:      fileDataSourceFollowConfig(link, true),
				ExpectError: regexp.MustCompile(`Is\s+a\s+directory`),
			},
			{
				Config:      fileDataSourceConfig(dangling),
				ExpectError: regexp.MustCompile(`Symbolic\s+link\s+refused`),
			},
			{
				Config:      fileDataSourceFollowConfig(dangling, true),
				ExpectError: regexp.MustCompile(`symbolic\s+link\s+whose\s+target\s+does\s+not\s+exist`),
			},
		},
	})
}

func TestAccFileDataSource_errors(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      fileDataSourceConfig(filepath.Join(dir, "missing")),
				ExpectError: regexp.MustCompile(`File\s+not\s+found`),
			},
			{
				// A missing parent directory is reported the same way.
				Config:      fileDataSourceConfig(filepath.Join(dir, "missing", "file")),
				ExpectError: regexp.MustCompile(`File\s+not\s+found`),
			},
			{
				Config:      fileDataSourceConfig(dir),
				ExpectError: regexp.MustCompile(`Is\s+a\s+directory`),
			},
			{
				// Opened non-blocking, so this must fail rather than hang.
				Config:      fileDataSourceConfig(fifo),
				ExpectError: regexp.MustCompile(`Not\s+a\s+regular\s+file`),
			},
			{
				Config:      fileDataSourceConfig("relative/file"),
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
			},
			{
				Config:      fileDataSourceConfig("/tmp/../etc/passwd"),
				ExpectError: regexp.MustCompile(`must\s+be\s+in\s+canonical\s+form`),
			},
			{
				Config:      fileDataSourceConfig("/"),
				ExpectError: regexp.MustCompile(`Invalid\s+path`),
			},
		},
	})
}
