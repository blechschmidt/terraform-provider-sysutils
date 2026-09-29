package provider

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func writeArchiveFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkTree(dir string, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		var got []string
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || p == dir {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			if d.IsDir() {
				rel += "/"
			}
			got = append(got, rel)
			return nil
		})
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("%s contains %q, want %q", dir, got, want)
		}
		return nil
	}
}

func knownStrings(values ...string) knownvalue.Check {
	checks := make([]knownvalue.Check, len(values))
	for i, v := range values {
		checks[i] = knownvalue.StringExact(v)
	}
	return knownvalue.ListExact(checks)
}

func TestAccArchiveExtract_lifecycle(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "app.tar.gz")
	dest := filepath.Join(work, "opt", "app")
	v1 := gzipBytes(t, makeTar(t,
		arDir("app-1/"),
		testEntry{name: "app-1/bin/app", typ: tar.TypeReg, body: "v1\n", mode: 0o755},
		arFile("app-1/README", "readme v1\n"),
		arFile("app-1/old/notes", "old\n"),
		arSymlink("app-1/current", "bin/app"),
	))
	v2 := gzipBytes(t, makeTar(t,
		testEntry{name: "app-2/bin/app", typ: tar.TypeReg, body: "v2\n", mode: 0o755},
		arFile("app-2/README", "readme v2\n"),
		arFile("app-2/lib/new", "new\n"),
		arSymlink("app-2/current", "bin/app"),
	))
	writeArchiveFile(t, archive, v1)
	config := fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source           = %q
  destination      = %q
  strip_components = 1
}
`, archive, dest)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("files"),
						knownStrings("README", "bin/", "bin/app", "current", "old/", "old/notes")),
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("archive_sha256"),
						knownvalue.StringExact(sha256Hex(v1))),
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("destination_created"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("drifted_entries"), knownvalue.Int64Exact(0)),
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("id"), knownvalue.StringExact(dest)),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTree(dest, "README", "bin/", "bin/app", "current", "old/", "old/notes"),
					checkFileContent(filepath.Join(dest, "bin/app"), "v1\n"),
				),
			},
			// A new version of the archive at the same path plans an update
			// that replaces the files and removes the ones it no longer has.
			{
				PreConfig: func() { writeArchiveFile(t, archive, v2) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_archive_extract.test", plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("archive_sha256"),
							knownvalue.StringExact(sha256Hex(v2))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTree(dest, "README", "bin/", "bin/app", "current", "lib/", "lib/new"),
					checkFileContent(filepath.Join(dest, "bin/app"), "v2\n"),
					checkFileContent(filepath.Join(dest, "README"), "readme v2\n"),
				),
			},
			// Drift: a modified file, a deleted file and a changed mode are
			// detected and repaired.
			{
				PreConfig: func() {
					writeArchiveFile(t, filepath.Join(dest, "README"), []byte("tampered"))
					if err := os.Remove(filepath.Join(dest, "lib/new")); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Join(dest, "bin/app"), 0o777); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_archive_extract.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(dest, "README"), "readme v2\n"),
					checkFileContent(filepath.Join(dest, "lib/new"), "new\n"),
					checkPerm(filepath.Join(dest, "bin/app"), 0o755),
				),
			},
			// No changes afterwards.
			{
				Config:   config,
				PlanOnly: true,
			},
			// A deleted destination is extracted again.
			{
				PreConfig: func() {
					if err := os.RemoveAll(dest); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_archive_extract.test", plancheck.ResourceActionCreate),
					},
				},
				Check: checkTree(dest, "README", "bin/", "bin/app", "current", "lib/", "lib/new"),
			},
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPathGone(dest),
			// Parents created for the destination stay, like mkdir -p.
			func(*terraform.State) error {
				_, err := os.Stat(filepath.Join(work, "opt"))
				return err
			},
		),
	})
}

func checkPerm(p string, want os.FileMode) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if got := info.Mode().Perm(); got != want {
			return fmt.Errorf("mode of %s is %v, want %v", p, got, want)
		}
		return nil
	}
}

func checkOwner(p string, uid, gid uint32) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != uid || st.Gid != gid {
			return fmt.Errorf("%s is owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, gid)
		}
		return nil
	}
}

// TestAccArchiveExtract_existingDestination extracts into a directory with
// other files: a conflicting file needs overwrite, and destroy removes only
// the extracted files.
func TestAccArchiveExtract_existingDestination(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "app.zip")
	dest := filepath.Join(work, "dest")
	mustMkdir(t, filepath.Join(dest, "etc"))
	writeArchiveFile(t, filepath.Join(dest, "keep"), []byte("mine"))
	writeArchiveFile(t, filepath.Join(dest, "etc", "app.conf"), []byte("mine"))
	writeArchiveFile(t, archive, makeZip(t,
		arFile("etc/app.conf", "archive"),
		arFile("etc/app.d/extra.conf", "extra"),
		arFile("bin/app", "app"),
	))
	config := func(overwrite bool) string {
		return fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
  overwrite   = %t
}
`, archive, dest, overwrite)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config(false),
				ExpectError: regexp.MustCompile(`was\s+not\s+extracted\s+by\s+this\s+resource`),
			},
			{
				PreConfig: func() {
					// The failed extraction changed nothing.
					if err := checkTree(dest, "etc/", "etc/app.conf", "keep")(nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: config(true),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("destination_created"), knownvalue.Bool(false)),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTree(dest, "bin/", "bin/app", "etc/", "etc/app.conf", "etc/app.d/", "etc/app.d/extra.conf", "keep"),
					checkFileContent(filepath.Join(dest, "etc/app.conf"), "archive"),
				),
			},
			// A new version without etc/app.d removes that directory, which
			// the first extraction created, but keeps etc, which existed.
			{
				PreConfig: func() {
					writeArchiveFile(t, archive, makeZip(t, arFile("etc/app.conf", "archive v2"), arFile("bin/app", "app v2")))
				},
				Config: config(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTree(dest, "bin/", "bin/app", "etc/", "etc/app.conf", "keep"),
					checkFileContent(filepath.Join(dest, "etc/app.conf"), "archive v2"),
				),
			},
		},
		// etc existed before the extraction, so it stays although it is
		// empty now; bin was created by it.
		CheckDestroy: checkTree(dest, "etc/", "keep"),
	})
}

func TestAccArchiveExtract_zipOwnershipAndModes(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "site.zip")
	dest := filepath.Join(work, "www")
	writeArchiveFile(t, archive, makeZip(t,
		arDir("site/"),
		arFile("site/index.html", "<h1>hi</h1>"),
		testEntry{name: "site/cgi/run", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
	))
	config := func(fileMode string) string {
		return fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source           = %q
  destination      = %q
  strip_components = 1
  owner            = "65534"
  group            = "65534"
  file_mode        = %q
  directory_mode   = "0750"
}
`, archive, dest, fileMode)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config("0640"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("files"),
						knownStrings("cgi/", "cgi/run", "index.html")),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPerm(dest, 0o750),
					checkPerm(filepath.Join(dest, "cgi"), 0o750),
					checkPerm(filepath.Join(dest, "cgi/run"), 0o640),
					checkPerm(filepath.Join(dest, "index.html"), 0o640),
					checkOwner(dest, 65534, 65534),
					checkOwner(filepath.Join(dest, "index.html"), 65534, 65534),
				),
			},
			// Ownership drift is detected.
			{
				PreConfig: func() {
					if err := os.Chown(filepath.Join(dest, "index.html"), 0, 0); err != nil {
						t.Fatal(err)
					}
				},
				Config: config("0640"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_archive_extract.test", plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("drifted_entries"), knownvalue.Int64Exact(0)),
					},
				},
				Check: checkOwner(filepath.Join(dest, "index.html"), 65534, 65534),
			},
			// A new file_mode re-extracts.
			{
				Config: config("0644"),
				Check:  checkPerm(filepath.Join(dest, "index.html"), 0o644),
			},
		},
		CheckDestroy: checkPathGone(dest),
	})
}

func TestAccArchiveExtract_tarXz(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "data.tar.xz")
	dest := filepath.Join(work, "data")
	writeArchiveFile(t, archive, xzBytes(t, makeTar(t, arFile("a/b", "xz"), arHardlink("a/c", "a/b"))))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
}
`, archive, dest),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(filepath.Join(dest, "a/b"), "xz"),
				checkFileContent(filepath.Join(dest, "a/c"), "xz"),
			),
		}},
		CheckDestroy: checkPathGone(dest),
	})
}

// TestAccArchiveExtract_malicious checks that malicious archives fail the
// plan and write nothing.
func TestAccArchiveExtract_malicious(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	dest := filepath.Join(work, "sub", "dest")
	for _, tt := range []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"zip-slip", makeZip(t, arFile("ok", "x"), arFile("../../escaped", "x")), `\.\.`},
		{"symlink escape", gzipBytes(t, makeTar(t, arSymlink("sub/up", ".."), arSymlink("sub/x", "up/.."))), `leads\s+outside`},
		{"absolute hard link", makeTar(t, arHardlink("passwd", "/etc/passwd")), `absolute\s+path`},
		{"bomb", gzipBytes(t, makeTar(t, arFile("zeros", strings.Repeat("\x00", 4<<20)))), `max_size`},
	} {
		archive := filepath.Join(work, tt.name)
		writeArchiveFile(t, archive, tt.data)
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config: fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
  max_size    = 1048576
}
`, archive, dest),
				ExpectError: regexp.MustCompile(`Invalid archive(.|\n)*` + tt.wantErr),
			}},
		})
		for _, p := range []string{filepath.Join(work, "sub"), filepath.Join(work, "escaped")} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("%s: %s exists after the plan failed", tt.name, p)
			}
		}
	}
}

func TestAccArchiveExtract_rootDir(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	archive := filepath.Join(t.TempDir(), "app.tar")
	writeArchiveFile(t, archive, makeTar(t, arFile("bin/app", "app")))
	host := "/opt/sysutils-archive-rootdir-" + filepath.Base(root)
	if _, err := os.Lstat(host); !os.IsNotExist(err) {
		t.Fatalf("%s unexpectedly exists on the host", host)
	}
	// An absolute symlink in the tree is resolved inside the root.
	mustMkdir(t, filepath.Join(root, "srv"))
	if err := os.Symlink("/srv", filepath.Join(root, "opt")); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: rootedProvider(root) + fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
}
`, archive, host),
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("destination"), knownvalue.StringExact(host)),
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(filepath.Join(root, "srv", filepath.Base(host), "bin/app"), "app"),
				checkNotExist(host),
			),
		}},
		CheckDestroy: checkPathGone(filepath.Join(root, "srv", filepath.Base(host))),
	})
}

// TestAccArchiveExtract_keepsForeignFiles checks that destroy leaves a
// created destination in place when it holds files that were not extracted.
func TestAccArchiveExtract_keepsForeignFiles(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "app.tar")
	dest := filepath.Join(work, "app")
	writeArchiveFile(t, archive, makeTar(t, arFile("data/a", "a")))
	config := fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
}
`, archive, dest)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				// A file created in an extracted directory is not drift.
				PreConfig: func() { writeArchiveFile(t, filepath.Join(dest, "data", "local"), []byte("x")) },
				Config:    config,
				PlanOnly:  true,
			},
		},
		CheckDestroy: checkTree(dest, "data/", "data/local"),
	})
}

// TestAccArchiveExtract_sourceRemoved checks that removing the archive after
// extraction does not plan changes.
func TestAccArchiveExtract_sourceRemoved(t *testing.T) {
	requireRoot(t)
	work := t.TempDir()
	archive := filepath.Join(work, "app.tar")
	dest := filepath.Join(work, "app")
	writeArchiveFile(t, archive, makeTar(t, arFile("a", "a")))
	config := fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source      = %q
  destination = %q
}
`, archive, dest)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if err := os.Remove(archive); err != nil {
						t.Fatal(err)
					}
				},
				Config:   config,
				PlanOnly: true,
			},
		},
		CheckDestroy: checkPathGone(dest),
	})
}
