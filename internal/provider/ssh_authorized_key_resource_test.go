package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testSSHKeyResource = "sysutils_ssh_authorized_key.test"

// sshKeyProviderFactories returns provider factories whose
// sysutils_ssh_authorized_key maps every user name except "ghost" to the
// user running the tests, with home as home directory.
func sshKeyProviderFactories(home string) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			sshKey: &sshKeyConfig{lookup: func(name string) (*sshAccount, error) {
				if name == "ghost" {
					return nil, sshUserNotFound(name)
				}
				return &sshAccount{name: name, uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), home: home}, nil
			}},
		}),
	}
}

func sshKeyHCL(body string) string {
	return fmt.Sprintf(`
resource "sysutils_ssh_authorized_key" "test" {
  user = "alice"
%s
}
`, body)
}

// checkAuthorizedKeys checks that the authorized_keys file in home
// contains want and has mode 0600, and that ~/.ssh has mode 0700.
func checkAuthorizedKeys(home, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		p := filepath.Join(home, ".ssh", "authorized_keys")
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
		}
		if info, err := os.Lstat(p); err != nil || info.Mode() != 0o600 {
			return fmt.Errorf("%s has mode %v (%v), want 0600", p, info.Mode(), err)
		}
		if info, err := os.Lstat(filepath.Dir(p)); err != nil || info.Mode() != os.ModeDir|0o700 {
			return fmt.Errorf("%s has mode %v (%v), want 0700", filepath.Dir(p), info.Mode(), err)
		}
		return nil
	}
}

func writeAuthorizedKeysFile(t *testing.T, home, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSSHAuthorizedKeyResource_lifecycle(t *testing.T) {
	home := t.TempDir()
	a, b := testSSHKey(t, 0), testSSHKey(t, 1)
	at, bt := testSSHKeyText(a), testSSHKeyText(b)
	other := testSSHKeyText(testSSHKey(t, 2)) + " unmanaged"
	p := filepath.Join(home, ".ssh", "authorized_keys")
	fpA, fpB := sshKeyFingerprint(a), sshKeyFingerprint(b)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sshKeyProviderFactories(home),
		CheckDestroy:             checkAuthorizedKeys(home, "# keep me\n"+other+"\n"),
		Steps: []resource.TestStep{
			{
				// ~/.ssh does not exist yet; the comment comes from the key.
				Config: sshKeyHCL(fmt.Sprintf(`  key = %q`, at+" alice@laptop\n")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("fingerprint"), knownvalue.StringExact(fpA)),
						plancheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("line"), knownvalue.StringExact(at+" alice@laptop")),
						plancheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("id"), knownvalue.StringExact("alice:"+fpA)),
					},
				},
				Check: checkAuthorizedKeys(home, at+" alice@laptop\n"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
					statecheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("comment"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("options"), knownvalue.Null()),
				},
			},
			{
				// Lines around the key are kept; options and comment change in place.
				PreConfig: func() {
					writeAuthorizedKeysFile(t, home, "# keep me\n"+at+" alice@laptop\n"+other+"\n")
				},
				Config: sshKeyHCL(fmt.Sprintf(`
  key     = %q
  comment = "deploy"
  options = ["from=\"10.0.0.0/8,192.168.0.0/16\"", "no-pty", "command=\"echo \\\"hi\\\"\""]
`, at+" alice@laptop")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSSHKeyResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkAuthorizedKeys(home, "# keep me\n"+`from="10.0.0.0/8,192.168.0.0/16",no-pty,command="echo \"hi\"" `+at+" deploy\n"+other+"\n"),
			},
			{
				// Another key takes the old key's place.
				Config: sshKeyHCL(fmt.Sprintf(`
  key     = %q
  options = ["restrict"]
`, bt)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSSHKeyResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSSHKeyResource, tfjsonpath.New("id"), knownvalue.StringExact("alice:"+fpB)),
					},
				},
				Check: checkAuthorizedKeys(home, "# keep me\nrestrict "+bt+"\n"+other+"\n"),
			},
			{
				ResourceName:      testSSHKeyResource,
				ImportState:       true,
				ImportStateId:     "alice:" + fpB,
				ImportStateVerify: true,
			},
		},
	})
}

func TestSSHAuthorizedKeyResource_drift(t *testing.T) {
	home := t.TempDir()
	a := testSSHKey(t, 0)
	at := testSSHKeyText(a)
	other := testSSHKeyText(testSSHKey(t, 1))
	config := sshKeyHCL(fmt.Sprintf(`
  key     = %q
  comment = "alice"
  options = ["no-pty"]
`, at))
	want := "no-pty " + at + " alice"

	step := func(name, mutation string, action plancheck.ResourceActionType) resource.TestStep {
		return resource.TestStep{
			PreConfig: func() {
				if mutation == "delete" {
					if err := os.Remove(filepath.Join(home, ".ssh", "authorized_keys")); err != nil {
						t.Fatal(err)
					}
					return
				}
				writeAuthorizedKeysFile(t, home, mutation)
			},
			Config: config,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSSHKeyResource, action)},
			},
			// The key is back, exactly once, and the other key is kept.
			Check: func(*terraform.State) error {
				data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				var keyLines []string
				for _, l := range strings.Split(string(data), "\n") {
					if strings.Contains(l, at) {
						keyLines = append(keyLines, l)
					}
				}
				if len(keyLines) != 1 || keyLines[0] != want {
					return fmt.Errorf("%s: key lines %q, want [%q]", name, keyLines, want)
				}
				if mutation != "delete" && !strings.Contains(string(data), other) {
					return fmt.Errorf("%s: other key removed:\n%s", name, data)
				}
				return nil
			},
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sshKeyProviderFactories(home),
		Steps: []resource.TestStep{
			{Config: config, Check: checkAuthorizedKeys(home, want+"\n")},
			step("removed", other+"\n", plancheck.ResourceActionCreate),
			step("comment changed", other+"\nno-pty "+at+" someone\n", plancheck.ResourceActionUpdate),
			step("options removed", at+" alice\n"+other+"\n", plancheck.ResourceActionUpdate),
			step("duplicate", want+"\n"+other+"\n"+at+"\n", plancheck.ResourceActionUpdate),
			step("file deleted", "delete", plancheck.ResourceActionCreate),
			{
				// Reading it back plans no change.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A comment change outside Terraform shows the actual value.
				PreConfig:          func() { writeAuthorizedKeysFile(t, home, "no-pty "+at+" changed\n") },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(testSSHKeyResource, "comment", "changed"),
					resource.TestCheckResourceAttr(testSSHKeyResource, "line", "no-pty "+at+" changed"),
				),
			},
		},
	})
}

func TestSSHAuthorizedKeyResource_importWithOptions(t *testing.T) {
	home := t.TempDir()
	a := testSSHKey(t, 3)
	at := testSSHKeyText(a)
	writeAuthorizedKeysFile(t, home, `from="10.1.2.3",no-agent-forwarding `+at+" ops key\n")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sshKeyProviderFactories(home),
		Steps: []resource.TestStep{
			{
				Config: sshKeyHCL(fmt.Sprintf(`
  key     = %q
  options = ["from=\"10.1.2.3\"", "no-agent-forwarding"]
`, at+" ops key")),
				ResourceName:       testSSHKeyResource,
				ImportState:        true,
				ImportStateId:      "alice:" + sshKeyFingerprint(a),
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					attrs := states[0].Attributes
					if attrs["key"] != at+" ops key" || attrs["options.#"] != "2" || attrs["options.0"] != `from="10.1.2.3"` ||
						attrs["user"] != "alice" || attrs["path"] != filepath.Join(home, ".ssh", "authorized_keys") {
						return fmt.Errorf("imported state: %v", attrs)
					}
					if _, ok := attrs["comment"]; ok && attrs["comment"] != "" {
						return fmt.Errorf("comment = %q, want null", attrs["comment"])
					}
					return nil
				},
			},
			{
				Config: sshKeyHCL(fmt.Sprintf(`
  key     = %q
  options = ["from=\"10.1.2.3\"", "no-agent-forwarding"]
`, at+" ops key")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestSSHAuthorizedKeyResource_invalid(t *testing.T) {
	home := t.TempDir()
	at := testSSHKeyText(testSSHKey(t, 0))
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{"not a key", `key = "ssh-ed25519 not-base64!"`, `not a valid OpenSSH public key`},
		{"options in key", fmt.Sprintf(`key = %q`, "no-pty "+at), `not with options`},
		{"two keys", fmt.Sprintf(`key = %q`, at+"\n"+at), `single public key`},
		{"bad option", fmt.Sprintf("key = %q\noptions = [\"no-pty,no-X11-forwarding\"]", at), `Invalid authorized_keys option`},
		{"bad comment", fmt.Sprintf("key = %q\ncomment = \" padded\"", at), `must not start or end with\s+white space`},
		{"bad user", fmt.Sprintf("key = %q\nuser = \"-oops\"", at), `Invalid name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			config := sshKeyHCL(body)
			if strings.Contains(body, "user =") {
				config = fmt.Sprintf("resource \"sysutils_ssh_authorized_key\" \"test\" {\n%s\n}\n", body)
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: sshKeyProviderFactories(home),
				Steps: []resource.TestStep{{
					Config:      config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestSSHAuthorizedKeyResource_errors(t *testing.T) {
	at := testSSHKeyText(testSSHKey(t, 0))

	t.Run("missing user", func(t *testing.T) {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: sshKeyProviderFactories(t.TempDir()),
			Steps: []resource.TestStep{{
				Config:      fmt.Sprintf("resource \"sysutils_ssh_authorized_key\" \"test\" {\n  user = \"ghost\"\n  key = %q\n}\n", at),
				ExpectError: regexp.MustCompile(`User "ghost" does not exist`),
			}},
		})
	})

	t.Run("symlinked .ssh", func(t *testing.T) {
		home, victim := t.TempDir(), t.TempDir()
		if err := os.Symlink(victim, filepath.Join(home, ".ssh")); err != nil {
			t.Fatal(err)
		}
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: sshKeyProviderFactories(home),
			Steps: []resource.TestStep{{
				Config:      sshKeyHCL(fmt.Sprintf("key = %q", at)),
				ExpectError: regexp.MustCompile(`is a symbolic\s+link`),
			}},
		})
		if entries, _ := os.ReadDir(victim); len(entries) != 0 {
			t.Errorf("symlink target was written: %v", entries)
		}
	})

	t.Run("symlinked authorized_keys", func(t *testing.T) {
		home := t.TempDir()
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("do not touch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(home, ".ssh", "authorized_keys")); err != nil {
			t.Fatal(err)
		}
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: sshKeyProviderFactories(home),
			Steps: []resource.TestStep{{
				Config:      sshKeyHCL(fmt.Sprintf("key = %q", at)),
				ExpectError: regexp.MustCompile(`is a symbolic\s+link`),
			}},
		})
		if data, _ := os.ReadFile(victim); string(data) != "do not touch\n" {
			t.Errorf("symlink target changed to %q", data)
		}
	})

	t.Run("root_dir", func(t *testing.T) {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: sshKeyProviderFactories(t.TempDir()),
			Steps: []resource.TestStep{{
				Config: fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", t.TempDir()) +
					sshKeyHCL(fmt.Sprintf("key = %q", at)),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Not supported with root_dir`),
			}},
		})
	})

	t.Run("bad import ID", func(t *testing.T) {
		for _, id := range []string{"alice", "alice:MD5:aa", "-x:" + sshKeyFingerprint(testSSHKey(t, 0))} {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: sshKeyProviderFactories(t.TempDir()),
				Steps: []resource.TestStep{{
					Config:        sshKeyHCL(fmt.Sprintf("key = %q", at)),
					ResourceName:  testSSHKeyResource,
					ImportState:   true,
					ImportStateId: id,
					ExpectError:   regexp.MustCompile(`Invalid import ID`),
				}},
			})
		}
	})

	t.Run("import missing key", func(t *testing.T) {
		home := t.TempDir()
		writeAuthorizedKeysFile(t, home, testSSHKeyText(testSSHKey(t, 5))+"\n")
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: sshKeyProviderFactories(home),
			Steps: []resource.TestStep{{
				Config:        sshKeyHCL(fmt.Sprintf("key = %q", at)),
				ResourceName:  testSSHKeyResource,
				ImportState:   true,
				ImportStateId: "alice:" + sshKeyFingerprint(testSSHKey(t, 0)),
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			}},
		})
	})
}
