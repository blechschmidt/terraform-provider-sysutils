package provider

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// accRepoName is the repository the acceptance tests add and remove.
const accRepoName = "sysutils-acc-test"

// accRepoCase is a repository the acceptance test adds to the host's real
// package manager, with refresh_cache, so that the package manager itself
// checks the file and the key. contains checks that the package manager
// knows the repository; with enabled false, that it does not use it.
type accRepoCase struct {
	body     string // HCL attributes except name, enabled and refresh_cache
	files    []string
	contains func(t *testing.T, enabled bool) error
}

// osRelease returns the fields of /etc/os-release.
func osRelease(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Skipf("no /etc/os-release: %v", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = strings.Trim(v, `"'`)
		}
	}
	return fields
}

// commandOutput runs argv and returns its combined output.
func commandOutput(argv ...string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, out)
	}
	return string(out), nil
}

// aptAccCase adds the backports suite of the distribution's own archive,
// in a component small enough to download quickly, signed by the archive
// key that is fetched from the distribution's binary keyring file. The
// system's sources use another suite, so apt does not complain about
// conflicting Signed-By values.
func aptAccCase(t *testing.T) accRepoCase {
	rel := osRelease(t)
	codename := rel["VERSION_CODENAME"]
	var uri, keyring, component string
	switch rel["ID"] {
	case "debian":
		uri, keyring, component = "http://deb.debian.org/debian", "/usr/share/keyrings/debian-archive-keyring.gpg", "contrib"
	case "ubuntu":
		uri, keyring, component = "http://archive.ubuntu.com/ubuntu", "/usr/share/keyrings/ubuntu-archive-keyring.gpg", "main"
		if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
			uri = "http://ports.ubuntu.com/ubuntu-ports"
		}
	default:
		t.Skipf("apt acceptance test supports Debian and Ubuntu, not %q", rel["ID"])
	}
	if codename == "" || strings.Contains(rel["VERSION"], "sid") {
		t.Skip("no release codename with a backports suite")
	}
	if _, err := os.Stat(keyring); err != nil {
		t.Skipf("archive keyring: %v", err)
	}
	suite := codename + "-backports"
	return accRepoCase{
		body: fmt.Sprintf(`
  description     = "sysutils acceptance test: %s backports"
  uris            = [%q]
  suites          = [%q]
  components      = [%q]
  signing_key_url = "file://%s"
`, rel["ID"], uri, suite, component, keyring),
		files: []string{"/etc/apt/sources.list.d/" + accRepoName + ".sources", "/etc/apt/keyrings/" + accRepoName + ".asc"},
		contains: func(t *testing.T, enabled bool) error {
			out, err := commandOutput("apt-cache", "policy")
			if err != nil {
				return err
			}
			listed := strings.Contains(out, uri+" "+suite+"/"+component+" ")
			if listed != enabled {
				return fmt.Errorf("apt-cache policy lists %s %s/%s: %v, want %v:\n%s", uri, suite, component, listed, enabled, out)
			}
			return nil
		},
	}
}

// writeEmptyRPMRepo writes the metadata of an empty rpm-md repository to
// dir, which is all dnf makecache needs.
func writeEmptyRPMRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "repodata"), 0o755); err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"primary":   `<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="0"></metadata>`,
		"filelists": `<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="0"></filelists>`,
		"other":     `<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="0"></otherdata>`,
	}
	var repomd strings.Builder
	repomd.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n<revision>1</revision>\n")
	for _, typ := range []string{"primary", "filelists", "other"} {
		open := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + docs[typ] + "\n")
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		_, _ = w.Write(open)
		_ = w.Close()
		name := typ + ".xml.gz"
		if err := os.WriteFile(filepath.Join(dir, "repodata", name), gz.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&repomd, `<data type="%s"><checksum type="sha256">%s</checksum><open-checksum type="sha256">%s</open-checksum>`+
			`<location href="repodata/%s"/><timestamp>1700000000</timestamp><size>%d</size><open-size>%d</open-size></data>`+"\n",
			typ, sha256Hex(gz.Bytes()), sha256Hex(open), name, gz.Len(), len(open))
	}
	repomd.WriteString("</repomd>\n")
	if err := os.WriteFile(filepath.Join(dir, "repodata", "repomd.xml"), []byte(repomd.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rpmAccCase adds a local, empty repository, with the distribution's own
// package signing key given inline if there is one.
func rpmAccCase(t *testing.T, kind string) accRepoCase {
	dir := t.TempDir()
	writeEmptyRPMRepo(t, dir)
	key := ""
	if keys, _ := filepath.Glob("/etc/pki/rpm-gpg/RPM-GPG-KEY-*"); len(keys) > 0 {
		for _, k := range keys {
			data, err := os.ReadFile(k)
			if err == nil && strings.Count(string(data), pgpArmorBegin) == 1 {
				if _, err := normalizeSigningKey(data); err == nil {
					key = fmt.Sprintf("  signing_key = %q\n", string(data))
					break
				}
			}
		}
	}
	files := []string{"/etc/yum.repos.d/" + accRepoName + ".repo"}
	if key != "" {
		files = append(files, "/etc/pki/rpm-gpg/RPM-GPG-KEY-"+accRepoName)
	}
	return accRepoCase{
		body:  fmt.Sprintf("  description = \"sysutils acceptance test\"\n  uris = [\"file://%s\"]\n%s", dir, key),
		files: files,
		contains: func(t *testing.T, enabled bool) error {
			out, err := commandOutput(kind, "repolist", "--enabled")
			if err != nil {
				return err
			}
			if listed := strings.Contains(out, accRepoName); listed != enabled {
				return fmt.Errorf("%s repolist --enabled lists %s: %v, want %v:\n%s", kind, accRepoName, listed, enabled, out)
			}
			return nil
		},
	}
}

// apkAccCase adds the edge/testing repository with a tag, so that it does
// not change which packages untagged installs pick.
func apkAccCase(t *testing.T) accRepoCase {
	const uri = "https://dl-cdn.alpinelinux.org/alpine/edge/testing"
	return accRepoCase{
		body: fmt.Sprintf("  description = \"sysutils acceptance test\"\n  uris = [%q]\n  tag = \"sysutilsacc\"\n", uri),
		contains: func(t *testing.T, enabled bool) error {
			// apk update lists the repositories it uses, with their index
			// versions.
			out, err := commandOutput("apk", "update")
			if err != nil {
				return err
			}
			if listed := strings.Contains(out, "["+uri+"]"); listed != enabled {
				return fmt.Errorf("apk update lists %s: %v, want %v:\n%s", uri, listed, enabled, out)
			}
			return nil
		},
	}
}

// TestAccPackageRepository_real adds a repository to the host's package
// manager with refresh_cache, so that apt-get update, dnf makecache or apk
// update read and verify it, disables it, and removes it. It runs as root
// only, and in CI in the disposable test container.
func TestAccPackageRepository_real(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	mgr, err := (&packageConfig{}).resolve(packageManagerAuto)
	if err != nil {
		t.Skipf("no supported package manager: %v", err)
	}
	var c accRepoCase
	switch mgr.Kind() {
	case packageManagerApt:
		c = aptAccCase(t)
	case packageManagerDnf, packageManagerYum:
		c = rpmAccCase(t, mgr.Kind())
	case packageManagerApk:
		c = apkAccCase(t)
	}
	for _, f := range c.files {
		if _, err := os.Lstat(f); !errors.Is(err, os.ErrNotExist) {
			t.Skipf("%s exists already; not touching it", f)
		}
	}
	apkBefore, _ := os.ReadFile(apkRepositoriesFile)
	t.Cleanup(func() {
		for _, f := range c.files {
			_ = os.Remove(f)
		}
		if mgr.Kind() == packageManagerApk {
			_ = os.WriteFile(apkRepositoriesFile, apkBefore, 0o644)
		}
	})

	config := func(enabled bool) string {
		return repoHCL(fmt.Sprintf("  name = %q\n  manager = %q\n  enabled = %v\n  refresh_cache = true\n%s", accRepoName, mgr.Kind(), enabled, c.body))
	}
	check := func(enabled bool) resource.TestCheckFunc {
		return func(*terraform.State) error {
			for _, f := range c.files {
				info, err := os.Stat(f)
				if err != nil {
					return err
				}
				if info.Mode() != repoFileMode {
					return fmt.Errorf("%s has mode %v, want %v", f, info.Mode(), repoFileMode)
				}
			}
			return c.contains(t, enabled)
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if err := checkRepoGone(c.files...)(s); err != nil {
				return err
			}
			if mgr.Kind() == packageManagerApk {
				if after, _ := os.ReadFile(apkRepositoriesFile); !bytes.Equal(after, apkBefore) {
					return fmt.Errorf("%s =\n%s\nwant\n%s", apkRepositoriesFile, after, apkBefore)
				}
			}
			return c.contains(t, false)
		},
		Steps: []resource.TestStep{
			{Config: config(true), Check: check(true)},
			{Config: config(true), PlanOnly: true},
			{
				ResourceName:            testRepoResource,
				ImportState:             true,
				ImportStateId:           accRepoName,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"refresh_cache", "manager", "signing_key", "signing_key_url"},
			},
			{Config: config(false), Check: check(false)},
		},
	})
}
