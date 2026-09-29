package provider

// Acceptance regression tests for the security review of the ini_value
// resource.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// A file starting with a UTF-8 byte order mark, with CRLF line endings and
// without a final newline, is edited in place of its existing section; the
// mark, the line endings, the rest of the file, its mode and its owner are
// kept. On the old code the section was appended a second time.
func TestAccIniValue_byteOrderMark(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "app.ini")
	const bom = "\xef\xbb\xbf"
	orig := bom + "[Service]\r\n; user\r\nUser=alice\r\nGroup=staff\r\n\r\n[Install]\r\nWantedBy=multi-user.target"
	mustWrite(t, p, orig)
	mustChmod(t, p, 0o640)
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	config := func(value string) string {
		return iniValueConfig(p, fmt.Sprintf(`  section   = "Service"
  key       = "User"
  separator = "="
  value     = %q`, value))
	}

	edited := strings.Replace(orig, "User=alice", "User=bob", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy removes both keys; the blank line that separated the
		// global key from the first section stays.
		CheckDestroy: checkFileContent(p, bom+"\r\n"+strings.Replace(orig, "User=alice\r\n", "", 1)[len(bom):]),
		Steps: []resource.TestStep{
			{
				Config: config("bob"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, edited),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
					checkNoTempFiles(dir),
				),
			},
			{
				// A global key goes before the first section, after the
				// mark.
				Config: config("bob") + fmt.Sprintf(`
resource "sysutils_ini_value" "global" {
  path  = %q
  key   = "version"
  value = "2"
}`, p),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, bom+"version = 2\r\n\r\n"+edited[len(bom):]),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
				),
			},
		},
	})
}
