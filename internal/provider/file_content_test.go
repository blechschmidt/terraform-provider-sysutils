package provider

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Checksum under test.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// binaryPayload is not valid UTF-8, so it can only be managed through
// content_base64 or source.
var binaryPayload = []byte{0x00, 0xff, 0xfe, 'b', 'i', 'n', 0x80, '\n'}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func md5Hex(b []byte) string    { s := md5.Sum(b); return hex.EncodeToString(s[:]) } //nolint:gosec

func checkChecksums(name string, want []byte) resource.TestCheckFunc {
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(name, "content_sha256", sha256Hex(want)),
		resource.TestCheckResourceAttr(name, "content_md5", md5Hex(want)),
	)
}

func checkFileBytes(path string, want []byte) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("content mismatch at %s: got %q want %q", path, got, want)
		}
		return nil
	}
}

// writeLater returns a function that writes data to path, for use as PreConfig.
func writeLater(t *testing.T, path string, data []byte) func() {
	return func() {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	expectUpdatePlan = resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionUpdate),
		},
	}
	expectEmptyPlan = resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
	}
)

func TestAccFile_contentChecksums(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "text.txt")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "hello\n"
}`, target),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// Known at plan time, so dependents can use them.
						plancheck.ExpectKnownValue(testFileResource, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex([]byte("hello\n")))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkChecksums(testFileResource, []byte("hello\n")),
					resource.TestCheckNoResourceAttr(testFileResource, "content_base64"),
					resource.TestCheckNoResourceAttr(testFileResource, "source"),
				),
			},
		},
	})
}

func TestAccFile_contentBase64(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "blob.bin")
	updated := append([]byte{0xc3, 0x28}, binaryPayload...)
	config := func(data []byte) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path           = %q
  content_base64 = %q
  mode           = "0600"
}`, target, base64.StdEncoding.EncodeToString(data))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config(binaryPayload),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileBytes(target, binaryPayload),
					checkFileMode(target, 0o600),
					checkChecksums(testFileResource, binaryPayload),
					resource.TestCheckResourceAttr(testFileResource, "content_base64", base64.StdEncoding.EncodeToString(binaryPayload)),
					resource.TestCheckNoResourceAttr(testFileResource, "content"),
				),
			},
			{
				Config:           config(updated),
				ConfigPlanChecks: expectUpdatePlan,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileBytes(target, updated),
					checkChecksums(testFileResource, updated),
				),
			},
			{
				// A binary file is imported into content_base64.
				ResourceName:      testFileResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccFile_source(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	target := filepath.Join(dir, "copy.bin")
	checksumCopy := filepath.Join(dir, "checksum.txt")
	writeLater(t, src, binaryPayload)()
	second := []byte("second version\n")

	// The second file depends on the first one's checksum, so a change to
	// the source must propagate through content_sha256.
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path   = %q
  source = %q
  mode   = "0640"
}

resource "sysutils_file" "checksum" {
  path    = %q
  content = sysutils_file.test.content_sha256
}`, target, src, checksumCopy)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileBytes(target, binaryPayload),
					checkFileMode(target, 0o640),
					checkChecksums(testFileResource, binaryPayload),
					resource.TestCheckResourceAttr(testFileResource, "source", src),
					resource.TestCheckNoResourceAttr(testFileResource, "content"),
					resource.TestCheckNoResourceAttr(testFileResource, "content_base64"),
					checkFileContent(checksumCopy, sha256Hex(binaryPayload)),
				),
			},
			{
				// Changing the source file, with no config change, must
				// plan an update of the copy and of its dependent.
				PreConfig: writeLater(t, src, second),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testFileResource, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex(second))),
						plancheck.ExpectResourceAction("sysutils_file.checksum", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileBytes(target, second),
					checkChecksums(testFileResource, second),
					checkFileContent(checksumCopy, sha256Hex(second)),
				),
			},
			{
				Config:           config,
				ConfigPlanChecks: expectEmptyPlan,
			},
		},
	})
}

func TestAccFile_sourceErrors(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	config := func(src string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path   = %q
  source = %q
}`, filepath.Join(dir, "copy"), src)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config(filepath.Join(dir, "missing")),
				ExpectError: regexp.MustCompile(`opening\s+source:.*no\s+such\s+file`),
			},
			{
				Config:      config(dir),
				ExpectError: regexp.MustCompile(`is\s+not\s+a\s+regular\s+file`),
			},
		},
	})
}

// TestAccFile_sourceSymlinkTarget checks that source mode keeps refusing to
// write through a symlink at path.
func TestAccFile_sourceSymlinkTarget(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	victim := filepath.Join(dir, "victim")
	link := filepath.Join(dir, "link")
	writeLater(t, src, []byte("payload"))()
	writeLater(t, victim, []byte("secret"))()
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path   = %q
  source = %q
}`, link, src),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
	})

	if got, _ := os.ReadFile(victim); string(got) != "secret" {
		t.Errorf("victim content = %q, want it untouched", got)
	}
}

func TestAccFile_driftBase64(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "blob.bin")
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path           = %q
  content_base64 = %q
}`, target, base64.StdEncoding.EncodeToString(binaryPayload))
	inSync := resource.ComposeAggregateTestCheckFunc(
		checkFileBytes(target, binaryPayload),
		checkChecksums(testFileResource, binaryPayload),
	)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: inSync},
			{
				// Tampered with text: refresh records the new checksum only.
				PreConfig: writeLater(t, target, []byte("tampered\n")),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testFileResource, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex(binaryPayload))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					inSync,
					resource.TestCheckNoResourceAttr(testFileResource, "content"),
				),
			},
			{
				// Tampered with other binary data.
				PreConfig:        writeLater(t, target, []byte{0xff, 0xff}),
				Config:           config,
				ConfigPlanChecks: expectUpdatePlan,
				Check:            inSync,
			},
		},
	})
}

func TestAccFile_driftSource(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	target := filepath.Join(dir, "copy")
	writeLater(t, src, []byte("from source\n"))()
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path   = %q
  source = %q
}`, target, src)
	inSync := resource.ComposeAggregateTestCheckFunc(
		checkFileBytes(target, []byte("from source\n")),
		checkChecksums(testFileResource, []byte("from source\n")),
	)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: inSync},
			{
				PreConfig:          writeLater(t, target, []byte("tampered\n")),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:           config,
				ConfigPlanChecks: expectUpdatePlan,
				Check:            inSync,
			},
			{
				// Deleted out of band: recreated from the source.
				PreConfig: func() {
					if err := os.Remove(target); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionCreate),
					},
				},
				Check: inSync,
			},
		},
	})
}

// TestAccFile_driftContentBinary checks that a text-managed file replaced by
// binary data is repaired instead of failing refresh.
func TestAccFile_driftContentBinary(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "text.txt")
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "text\n"
}`, target)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:        writeLater(t, target, binaryPayload),
				Config:           config,
				ConfigPlanChecks: expectUpdatePlan,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, "text\n"),
					checkChecksums(testFileResource, []byte("text\n")),
				),
			},
		},
	})
}

// TestAccFile_switchContentMode moves the same file between the three
// content attributes.
func TestAccFile_switchContentMode(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	target := filepath.Join(dir, "file")
	data := []byte("same bytes\n")
	writeLater(t, src, data)()
	config := func(attr string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path = %q
  %s
}`, target, attr)
	}

	steps := []string{
		fmt.Sprintf("content = %q", data),
		fmt.Sprintf("content_base64 = %q", base64.StdEncoding.EncodeToString(data)),
		fmt.Sprintf("source = %q", src),
		fmt.Sprintf("content = %q", data),
	}
	var ts []resource.TestStep
	for _, attr := range steps {
		ts = append(ts, resource.TestStep{
			Config: config(attr),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileBytes(target, data),
				checkChecksums(testFileResource, data),
			),
		})
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps:                    ts,
	})
}

func TestAccFile_contentAttributeConflicts(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeLater(t, src, []byte("x"))()
	target := filepath.Join(dir, "file")
	config := func(attrs string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path = %q
  %s
}`, target, attrs)
	}
	combination := regexp.MustCompile(`Invalid\s+Attribute\s+Combination[\s\S]*Exactly\s+one\s+of\s+these\s+attributes\s+must\s+be\s+configured`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config(""),
				ExpectError: regexp.MustCompile(`Missing\s+Attribute\s+Configuration[\s\S]*Exactly\s+one\s+of\s+these\s+attributes\s+must\s+be\s+configured`),
			},
			{
				Config:      config(fmt.Sprintf("content = \"x\"\n  source = %q", src)),
				ExpectError: combination,
			},
			{
				Config:      config("content = \"x\"\n  content_base64 = \"eA==\""),
				ExpectError: combination,
			},
			{
				Config:      config(fmt.Sprintf("content_base64 = \"eA==\"\n  source = %q", src)),
				ExpectError: combination,
			},
			{
				Config:      config("content_base64 = \"not base64!\""),
				ExpectError: regexp.MustCompile(`Invalid\s+base64`),
			},
			{
				Config:      config(`source = ""`),
				ExpectError: regexp.MustCompile(`Invalid\s+Attribute\s+Value\s+Length`),
			},
		},
	})

	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("file created despite invalid configuration (stat err = %v)", err)
	}
}

func TestOpenSourceRefusesFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Must fail promptly rather than block waiting for a writer.
	if _, err := openSource(fifo); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("openSource(fifo) error = %v, want not a regular file", err)
	}
	if _, _, err := desiredChecksums(&fileModel{Source: types.StringValue(fifo)}); err == nil {
		t.Fatal("desiredChecksums accepted a FIFO source")
	}
}

func TestDesiredChecksumsMissingSourceIsUnknown(t *testing.T) {
	m := &fileModel{Source: types.StringValue(filepath.Join(t.TempDir(), "missing"))}
	if _, known, err := desiredChecksums(m); err != nil || known {
		t.Fatalf("desiredChecksums(missing) = known %v, err %v; want unknown without error", known, err)
	}
}

func TestCheckWrittenChecksum(t *testing.T) {
	m := &fileModel{Path: types.StringValue("/tmp/x"), Source: types.StringValue("/tmp/src")}
	written := checksums{sha256: sha256Hex([]byte("new"))}

	if d := checkWrittenChecksum(m, types.StringValue(written.sha256), written); d.HasError() {
		t.Errorf("matching checksum reported error: %v", d)
	}
	if d := checkWrittenChecksum(m, types.StringUnknown(), written); d.HasError() {
		t.Errorf("unknown planned checksum reported error: %v", d)
	}
	d := checkWrittenChecksum(m, types.StringValue(sha256Hex([]byte("old"))), written)
	if !d.HasError() || !strings.Contains(d[0].Detail(), "modified between plan and apply") {
		t.Errorf("mismatched checksum: diagnostics = %v", d)
	}
}
