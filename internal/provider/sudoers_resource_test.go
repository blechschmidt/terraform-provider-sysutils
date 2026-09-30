package provider

import (
	"fmt"
	"os"
	"os/exec"
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

const testSudoersResource = "sysutils_sudoers.test"

// newSudoersTestConfig returns a sudoers configuration that writes to a
// sudoers.d directory in a temporary directory, which does not exist yet,
// with files owned by the user running the tests and checked by a fake
// visudo.
func newSudoersTestConfig(t *testing.T) (*sudoersConfig, *fakeVisudo) {
	t.Helper()
	f := &fakeVisudo{t: t}
	return &sudoersConfig{
		dir:      filepath.Join(t.TempDir(), "sudoers.d"),
		uid:      uint32(os.Getuid()), //nolint:gosec // Test IDs fit.
		gid:      uint32(os.Getgid()), //nolint:gosec // Test IDs fit.
		run:      f.run,
		lookPath: func(string) (string, error) { return fakeVisudoPath, nil },
	}, f
}

func sudoersProviderFactories(cfg *sudoersConfig) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", sudoers: cfg}),
	}
}

func sudoersHCL(name, body string) string {
	return fmt.Sprintf(`
resource "sysutils_sudoers" "test" {
  name = %q
%s
}
`, name, body)
}

// checkSudoersFile checks that the drop-in p contains want, with mode 0440.
func checkSudoersDropIn(p, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode() != sudoersFileMode {
			return fmt.Errorf("%s has mode %v, want %v", p, info.Mode(), sudoersFileMode)
		}
		return nil
	}
}

func checkSudoersGone(p string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("%s still exists (err = %v)", p, err)
		}
		return nil
	}
}

// writeSudoersFixture writes a drop-in by hand, bypassing the resource.
func writeSudoersFixture(t *testing.T, p, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o440); err != nil {
		t.Fatal(err)
	}
}

// assertSudoersFile fails the test unless p contains want.
func assertSudoersFile(t *testing.T, p, want string) {
	t.Helper()
	if err := checkSudoersDropIn(p, want)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestSudoersResource_lifecycle(t *testing.T) {
	cfg, f := newSudoersTestConfig(t)
	p := filepath.Join(cfg.dir, "90-deploy")
	owner, group := uidName(uint32(os.Getuid())), gidName(uint32(os.Getgid())) //nolint:gosec // Test IDs fit.
	content := "deploy ALL = (root) NOPASSWD: /usr/bin/systemctl restart app\n"
	rendered := sudoersFileHeader + "\n" +
		"%deploy, alice ALL = (ALL:ALL) NOPASSWD: SETENV: /usr/bin/systemctl restart app, /usr/bin/kill -s TERM\\, 42\n" +
		"bob web1 = /usr/bin/journalctl\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{
				Config: sudoersHCL("90-deploy", fmt.Sprintf("  content = %q", content)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("mode"), knownvalue.StringExact("0440")),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkSudoersDropIn(p, content),
					func(*terraform.State) error {
						if f.count() != 1 {
							return fmt.Errorf("visudo checked %d files, want 1", f.count())
						}
						return nil
					},
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("id"), knownvalue.StringExact("90-deploy")),
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("validate"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("owner"), knownvalue.StringExact(owner)),
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("group"), knownvalue.StringExact(group)),
				},
			},
			{
				// Switching to rules renders them, one line each.
				Config: sudoersHCL("90-deploy", `
  rules = [
    {
      users    = ["%deploy", "alice"]
      runas    = "ALL:ALL"
      nopasswd = true
      setenv   = true
      commands = ["/usr/bin/systemctl restart app", "/usr/bin/kill -s TERM\\, 42"]
    },
    {
      users    = ["bob"]
      hosts    = ["web1"]
      commands = ["/usr/bin/journalctl"]
    },
  ]
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("content"), knownvalue.StringExact(rendered)),
					},
				},
				Check: checkSudoersDropIn(p, rendered),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("rules").AtSliceIndex(0).AtMapKey("hosts"),
						knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("ALL")})),
				},
			},
			{
				// Renaming replaces the file.
				Config: sudoersHCL("91-deploy", fmt.Sprintf("  content = %q", content)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkSudoersGone(p),
					checkSudoersDropIn(filepath.Join(cfg.dir, "91-deploy"), content),
				),
			},
		},
	})
}

// TestSudoersResource_invalidSyntax checks that a file visudo rejects is
// never installed: the previous file stays in place, and so does the state.
func TestSudoersResource_invalidSyntax(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	p := filepath.Join(cfg.dir, "ops")
	good := "%ops ALL = ALL\n"
	goodConfig := sudoersHCL("ops", fmt.Sprintf("  content = %q", good))
	badConfig := sudoersHCL("ops", `  content = "%ops ALL = SYNTAX ERROR\n"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{Config: goodConfig, Check: checkSudoersDropIn(p, good)},
			{
				Config: badConfig,
				// visudo's output is the diagnostic, naming the drop-in
				// rather than the temporary file.
				ExpectError: regexp.MustCompile(`Invalid\s+sudoers\s+file[\s\S]*file\s+was\s+not\s+changed[\s\S]*` + regexp.QuoteMeta(p) + `:1:7:\s+syntax\s+error`),
			},
			{
				PreConfig: func() {
					assertSudoersFile(t, p, good)
					entries, err := os.ReadDir(cfg.dir)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 1 {
						t.Fatalf("%s has %d entries after the rejected apply, want 1", cfg.dir, len(entries))
					}
				},
				// The state still describes the good file.
				Config: goodConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// With validate = false, the check is skipped.
				Config: sudoersHCL("ops", `
  content  = "%ops ALL = SYNTAX ERROR\n"
  validate = false
`),
				Check: checkSudoersDropIn(p, "%ops ALL = SYNTAX ERROR\n"),
			},
		},
	})

	// A rejected new drop-in is not created.
	q := filepath.Join(cfg.dir, "new")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		Steps: []resource.TestStep{{
			Config:      sudoersHCL("new", `  content = "SYNTAX ERROR\n"`),
			ExpectError: regexp.MustCompile(`Invalid\s+sudoers\s+file`),
		}},
	})
	if _, err := os.Lstat(q); !os.IsNotExist(err) {
		t.Errorf("%s exists after a rejected create (err = %v)", q, err)
	}
}

func TestSudoersResource_visudoMissing(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	cfg.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	p := filepath.Join(cfg.dir, "ops")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{
				Config:      sudoersHCL("ops", `  content = "%ops ALL = ALL\n"`),
				ExpectError: regexp.MustCompile(`visudo\s+not\s+found[\s\S]*validate\s+=\s+false`),
			},
			{
				PreConfig: func() {
					if _, err := os.Lstat(p); !os.IsNotExist(err) {
						t.Fatalf("%s exists after the failed apply (err = %v)", p, err)
					}
				},
				Config: sudoersHCL("ops", `
  content  = "%ops ALL = ALL\n"
  validate = false
`),
				Check: checkSudoersDropIn(p, "%ops ALL = ALL\n"),
			},
		},
	})
}

func TestSudoersResource_nameValidation(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	cases := []struct {
		name, body, want string
	}{
		{"admins.conf", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name[\s\S]*sudo\s+ignores[\s\S]*"\."`},
		{"admins~", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name[\s\S]*end\s+with\s+"~"`},
		{"a/b", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name[\s\S]*"/"`},
		{"../../etc/shadow", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name`},
		{"", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name`},
		{"with space", `content = "x"`, `Invalid\s+sudoers\s+drop-in\s+name[\s\S]*white\s+space`},
		{"ok", ``, `Missing\s+Attribute\s+Configuration`},
		{"ok", `content = "x"` + "\n" + `rules = [{ users = ["a"], commands = ["ALL"] }]`, `Invalid\s+Attribute\s+Combination`},
		{"ok", `rules = []`, `rules\s+list\s+must\s+contain\s+at\s+least\s+1`},
		{"ok", `rules = [{ users = ["a,b"], commands = ["ALL"] }]`, `Invalid\s+sudoers\s+user`},
		{"ok", `rules = [{ users = ["a"], commands = ["/bin/ls, ALL"] }]`, `Invalid\s+sudoers\s+command[\s\S]*unescaped`},
		{"ok", `rules = [{ users = ["a"], runas = "root) ALL", commands = ["ALL"] }]`, `Invalid\s+sudoers\s+runas`},
		{"ok", `rules = [{ users = ["a"], hosts = ["ALL=ALL"], commands = ["ALL"] }]`, `Invalid\s+sudoers\s+host`},
		{"ok", `content = "a\u0000b"`, `Invalid\s+sudoers\s+content`},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{
			Config:      sudoersHCL(c.name, c.body),
			ExpectError: regexp.MustCompile(c.want),
		})
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		Steps:                    steps,
	})
	if _, err := os.Stat(cfg.dir); !os.IsNotExist(err) {
		t.Errorf("validation errors created %s (err = %v)", cfg.dir, err)
	}
}

func TestSudoersResource_import(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	p := filepath.Join(cfg.dir, "handmade")
	content := "Defaults:backup !requiretty\nbackup ALL = NOPASSWD: /usr/bin/rsync\n"
	config := sudoersHCL("handmade", fmt.Sprintf("  content = %q", content))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{
				PreConfig:          func() { writeSudoersFixture(t, p, content) },
				Config:             config,
				ResourceName:       testSudoersResource,
				ImportState:        true,
				ImportStateId:      "handmade",
				ImportStatePersist: true,
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 {
						return fmt.Errorf("got %d instances", len(s))
					}
					a := s[0].Attributes
					for k, v := range map[string]string{"name": "handmade", "content": content, "path": p, "mode": "0440", "validate": "true"} {
						if a[k] != v {
							return fmt.Errorf("%s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			{
				// The configuration matches the imported file.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config:            config,
				ResourceName:      testSudoersResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				ResourceName:  testSudoersResource,
				ImportState:   true,
				ImportStateId: "missing",
				ExpectError:   regexp.MustCompile(`missing\s+does\s+not\s+exist`),
			},
			{
				ResourceName:  testSudoersResource,
				ImportState:   true,
				ImportStateId: "../sudoers",
				ExpectError:   regexp.MustCompile(`Invalid\s+import\s+ID`),
			},
			{
				ResourceName:  testSudoersResource,
				ImportState:   true,
				ImportStateId: "README.md",
				ExpectError:   regexp.MustCompile(`Invalid\s+import\s+ID[\s\S]*sudo\s+ignores`),
			},
		},
	})
}

func TestSudoersResource_existingFile(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	p := filepath.Join(cfg.dir, "taken")
	config := sudoersHCL("taken", `  content = "a ALL = ALL\n"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { writeSudoersFixture(t, p, "keep ALL = ALL\n") },
				Config:      config,
				ExpectError: regexp.MustCompile(`Sudoers\s+drop-in\s+already\s+exists[\s\S]*terraform\s+import`),
			},
			{
				PreConfig:          func() { assertSudoersFile(t, p, "keep ALL = ALL\n") },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestSudoersResource_drift(t *testing.T) {
	cfg, _ := newSudoersTestConfig(t)
	p := filepath.Join(cfg.dir, "drift")
	config := sudoersHCL("drift", `
  rules = [{ users = ["alice"], commands = ["ALL"] }]
`)
	want := sudoersFileHeader + "\nalice ALL = ALL\n"
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionUpdate)},
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{Config: config, Check: checkSudoersDropIn(p, want)},
			{
				// A line added by hand is drift that apply reverts.
				PreConfig:        func() { writeSudoersFixture(t, p, want+"mallory ALL = NOPASSWD: ALL\n") },
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkSudoersDropIn(p, want),
			},
			{
				// sudo ignores a file writable by others; so does the
				// resource not accept it.
				PreConfig: func() {
					if err := os.Chmod(p, 0o666); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("mode"), knownvalue.StringExact("0440")),
					},
				},
				Check: checkSudoersDropIn(p, want),
			},
			{
				// A deleted file is recreated.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionCreate)},
				},
				Check: checkSudoersDropIn(p, want),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Refresh refuses to follow a symlink planted at the path.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("/etc/passwd", p); err != nil {
						t.Fatal(err)
					}
				},
				Config:      config,
				ExpectError: regexp.MustCompile(`symbolic\s+link;\s+refusing\s+to\s+follow\s+it`),
			},
			{
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
			},
		},
	})
}

// TestSudoersResource_rootDir writes drop-ins below root_dir. The tree's
// visudo is never run: the host's checks the file if the tree has sudo,
// and the check is skipped with a warning if it has not.
func TestSudoersResource_rootDir(t *testing.T) {
	cfg, f := newSudoersTestConfig(t)
	cfg.dir = ""
	root := t.TempDir()
	p := filepath.Join(root, "etc/sudoers.d/image")
	config := func(content string) string {
		return rootedProvider(root) + sudoersHCL("image", fmt.Sprintf("  content = %q", content))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sudoersProviderFactories(cfg),
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{
				// No visudo in the tree: installed unchecked.
				Config: config("SYNTAX ERROR\n"),
				Check: resource.ComposeTestCheckFunc(
					checkSudoersDropIn(p, "SYNTAX ERROR\n"),
					resource.TestCheckResourceAttr(testSudoersResource, "path", "/etc/sudoers.d/image"),
					func(*terraform.State) error {
						if f.count() != 0 {
							return fmt.Errorf("visudo checked %d files, want none", f.count())
						}
						info, err := os.Stat(filepath.Dir(p))
						if err != nil {
							return err
						}
						if info.Mode().Perm() != sudoersDirMode {
							return fmt.Errorf("sudoers.d created with mode %v, want %v", info.Mode().Perm(), sudoersDirMode)
						}
						return nil
					},
				),
			},
			{
				// With sudo in the tree, the host's visudo checks the file.
				PreConfig: func() {
					bin := filepath.Join(root, "usr/sbin/visudo")
					if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				},
				Config:      config("SYNTAX ERROR\nstill wrong\n"),
				ExpectError: regexp.MustCompile(`/etc/sudoers\.d/image:1:7:\s+syntax\s+error`),
			},
			{
				PreConfig: func() { assertSudoersFile(t, p, "SYNTAX ERROR\n") },
				Config:    config("root ALL = ALL\n"),
				Check: resource.ComposeTestCheckFunc(
					checkSudoersDropIn(p, "root ALL = ALL\n"),
					func(*terraform.State) error {
						for _, c := range f.checked {
							if !strings.HasPrefix(c, filepath.Dir(p)+"/.image.") {
								return fmt.Errorf("visudo checked %s, want a temporary file next to %s", c, p)
							}
						}
						return nil
					},
				),
			},
		},
	})
}
