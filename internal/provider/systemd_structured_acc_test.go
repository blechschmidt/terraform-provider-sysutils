package provider

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// hostSystemdVersion returns the major version of the host's systemd.
func hostSystemdVersion(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		t.Fatalf("systemctl --version: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		t.Fatalf("unexpected systemctl --version output %q", out)
	}
	v, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("unexpected systemctl --version output %q", out)
	}
	return v
}

// checkUnitProps checks properties from "systemctl show" of a unit.
func checkUnitProps(name string, want map[string]string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		args := []string{"show"}
		for p := range want {
			args = append(args, "--property="+p)
		}
		out, err := exec.Command("systemctl", append(args, "--", name)...).Output()
		if err != nil {
			return fmt.Errorf("systemctl show %s: %v", name, err)
		}
		got := parseShowOutput(string(out))
		for p, w := range want {
			if got[p] != w {
				return fmt.Errorf("%s: %s=%q, want %q", name, p, got[p], w)
			}
		}
		return nil
	}
}

func escapeUnitPath(t *testing.T, p, suffix string) string {
	t.Helper()
	out, err := exec.Command("systemd-escape", "--path", "--suffix="+suffix, p).Output()
	if err != nil {
		t.Fatalf("systemd-escape: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// Every unit type that has unit files, configured through attributes, is
// loaded by systemd with the configured properties, started, and removed
// again on destroy.
func TestAccSystemdUnit_structuredAllTypes(t *testing.T) {
	requireSystemd(t)
	id := randomID()[:10]
	app := "tfacc-" + id + "-app.service"
	sock := "tfacc-" + id + "-sock.socket"
	sockSvc := "tfacc-" + id + "-sock.service"
	timer := "tfacc-" + id + ".timer"
	pathUnit := "tfacc-" + id + ".path"
	target := "tfacc-" + id + ".target"
	slice := "tfacc" + id + ".slice"
	device := "dev-tfacc" + id + ".device"
	mountDir := "/run/tfacc" + id
	mount := escapeUnitPath(t, mountDir, "mount")
	autoDir := "/run/tfaccauto" + id
	autoMount := escapeUnitPath(t, autoDir, "mount")
	automount := escapeUnitPath(t, autoDir, "automount")
	port := freeLocalPort(t)
	all := []string{app, sock, sockSvc, timer, pathUnit, target, slice, device, mount, autoMount, automount}

	config := fmt.Sprintf(`
resource "sysutils_systemd_unit" "app" {
  name  = %[1]q
  state = "running"
  unit = {
    description   = "tfacc app %%n"
    documentation = ["man:systemd.service(5)"]
  }
  service = {
    type              = "oneshot"
    remain_after_exit = true
    exec_start        = ["/bin/true first", "/bin/true second"]
    environment       = ["A=1", "\"B=two words\""]
    slice             = %[7]q
    memory_max        = "100M"
  }
  depends_on = [sysutils_systemd_unit.slice]
}

resource "sysutils_systemd_unit" "sock_svc" {
  name = %[3]q
  service = {
    exec_start = ["/bin/sleep infinity"]
  }
}

resource "sysutils_systemd_unit" "sock" {
  name  = %[2]q
  state = "running"
  socket = {
    listen_stream = ["127.0.0.1:%[13]d"]
    service       = %[3]q
    backlog       = "16"
  }
  depends_on = [sysutils_systemd_unit.sock_svc]
}

resource "sysutils_systemd_unit" "timer" {
  name  = %[4]q
  state = "running"
  timer = {
    on_calendar         = ["*-*-* 03:00:00", "Sat *-*-* 04:00:00"]
    randomized_delay_sec = "1m"
    unit                = %[1]q
  }
  install = {
    wanted_by = ["timers.target"]
  }
  depends_on = [sysutils_systemd_unit.app]
}

resource "sysutils_systemd_unit" "path" {
  name  = %[5]q
  state = "running"
  path_section = {
    path_exists = ["/run/tfacc-%[14]s-trigger"]
    unit        = %[1]q
  }
  depends_on = [sysutils_systemd_unit.app]
}

resource "sysutils_systemd_unit" "target" {
  name  = %[6]q
  state = "running"
  unit = {
    description = "tfacc target"
    wants       = [%[1]q]
  }
  depends_on = [sysutils_systemd_unit.app]
}

resource "sysutils_systemd_unit" "slice" {
  name  = %[7]q
  state = "running"
  slice = {
    memory_max = "64M"
    cpu_weight = "50"
  }
}

resource "sysutils_systemd_unit" "device" {
  name = %[8]q
  unit = {
    description = "tfacc device"
  }
}

resource "sysutils_systemd_unit" "mount" {
  name  = %[9]q
  state = "running"
  mount = {
    what    = "tmpfs"
    where   = %[10]q
    type    = "tmpfs"
    options = "size=1m,mode=0700"
  }
}

resource "sysutils_systemd_unit" "auto_mount" {
  name = %[11]q
  mount = {
    what  = "tmpfs"
    where = %[15]q
    type  = "tmpfs"
  }
}

resource "sysutils_systemd_unit" "automount" {
  name  = %[12]q
  state = "running"
  automount = {
    where            = %[15]q
    timeout_idle_sec = "10min"
  }
  depends_on = [sysutils_systemd_unit.auto_mount]
}
`, app, sock, sockSvc, timer, pathUnit, target, slice, device, mount, mountDir, autoMount, automount, port, id, autoDir)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			for _, u := range all {
				if u == slice || u == device {
					// Systemd knows any slice, and devices of the kernel,
					// without a unit file.
					if err := checkUnitProps(u, map[string]string{"FragmentPath": ""})(s); err != nil {
						return err
					}
					continue
				}
				if err := checkRealUnitRemoved(u)(s); err != nil {
					return err
				}
			}
			if mounted(mountDir) {
				return fmt.Errorf("%s is still mounted", mountDir)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkUnitProps(app, map[string]string{
						"LoadState": "loaded", "ActiveState": "active", "Type": "oneshot", "RemainAfterExit": "yes",
						"Description": "tfacc app " + app, "Environment": `A=1 "B=two words"`, "Slice": slice,
						"MemoryMax": "104857600", "Documentation": `"man:systemd.service(5)"`,
					}),
					func(*terraform.State) error {
						out := systemctlOutput(t, "show", "--property=ExecStart", "--", app)
						if strings.Count(out, "argv[]=/bin/true") != 2 {
							return fmt.Errorf("ExecStart of %s = %q, want two commands", app, out)
						}
						return nil
					},
					checkUnitProps(sock, map[string]string{"ActiveState": "active", "Listen": fmt.Sprintf("127.0.0.1:%d (Stream)", port), "Backlog": "16"}),
					checkUnitProps(timer, map[string]string{"ActiveState": "active", "Unit": app, "RandomizedDelayUSec": "1min", "UnitFileState": "disabled"}),
					func(*terraform.State) error {
						out := systemctlOutput(t, "show", "--property=TimersCalendar", "--", timer)
						if strings.Count(out, "OnCalendar=") != 2 {
							return fmt.Errorf("TimersCalendar of %s = %q, want two calendars", timer, out)
						}
						return nil
					},
					checkUnitProps(pathUnit, map[string]string{"ActiveState": "active", "Unit": app, "Paths": fmt.Sprintf("/run/tfacc-%s-trigger (PathExists)", id)}),
					checkUnitProps(target, map[string]string{"ActiveState": "active", "Description": "tfacc target"}),
					checkUnitProps(slice, map[string]string{"ActiveState": "active", "MemoryMax": "67108864", "CPUWeight": "50"}),
					checkUnitProps(device, map[string]string{"LoadState": "loaded", "Description": "tfacc device"}),
					checkUnitProps(mount, map[string]string{"ActiveState": "active", "What": "tmpfs", "Where": mountDir}),
					func(*terraform.State) error {
						if !mounted(mountDir) {
							return fmt.Errorf("%s is not mounted", mountDir)
						}
						return nil
					},
					checkUnitProps(automount, map[string]string{"ActiveState": "active", "Where": autoDir, "TimeoutIdleUSec": "10min"}),
					resource.TestCheckResourceAttr("sysutils_systemd_unit.app", "service.exec_start.#", "2"),
					resource.TestCheckResourceAttr("sysutils_systemd_unit.timer", "timer.on_calendar.1", "Sat *-*-* 04:00:00"),
					resource.TestCheckNoResourceAttr("sysutils_systemd_unit.timer", "service.%"),
				),
			},
			{
				// Out-of-band edits of a property are drift.
				PreConfig: func() {
					p := filepath.Join(defaultSystemdUnitDir, slice)
					data, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, p, strings.Replace(string(data), "MemoryMax=64M", "MemoryMax=32M", 1))
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_systemd_unit.slice", plancheck.ResourceActionUpdate),
					},
				},
				Check: checkUnitProps(slice, map[string]string{"MemoryMax": "67108864"}),
			},
			{
				ResourceName:      "sysutils_systemd_unit.timer",
				ImportState:       true,
				ImportStateId:     timer,
				ImportStateVerify: true,
			},
		},
	})
}

func mounted(dir string) bool {
	data, _ := os.ReadFile("/proc/self/mounts")
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == dir {
			return true
		}
	}
	return false
}

func TestAccSystemdUnit_structuredSwap(t *testing.T) {
	requireSystemd(t)
	if _, err := exec.LookPath("mkswap"); err != nil {
		t.Skip("mkswap not found")
	}
	file := filepath.Join("/var/tmp", "tfaccswap"+randomID()[:10])
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+file, "bs=1M", "count=16").CombinedOutput(); err != nil {
		t.Fatalf("dd: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(file) })
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkswap", file).CombinedOutput(); err != nil {
		t.Fatalf("mkswap: %v: %s", err, out)
	}
	if out, err := exec.Command("swapon", file).CombinedOutput(); err != nil {
		t.Skipf("swap cannot be enabled here: %v: %s", err, out)
	}
	_ = exec.Command("swapoff", file).Run()
	name := escapeUnitPath(t, file, "swap")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if swapActive(file) {
				return fmt.Errorf("%s is still in use as swap", file)
			}
			return checkRealUnitRemoved(name)(s)
		},
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name  = %q
  state = "running"
  swap = {
    what     = %q
    priority = "7"
  }
}`, name, file),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkUnitProps(name, map[string]string{"ActiveState": "active", "Priority": "7"}),
				func(*terraform.State) error {
					if !swapActive(file) {
						return fmt.Errorf("%s is not in use as swap", file)
					}
					return nil
				},
			),
		}},
	})
}

func swapActive(file string) bool {
	data, _ := os.ReadFile("/proc/swaps")
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == file {
			return true
		}
	}
	return false
}

// A template with a default instance can be enabled; changing it restarts
// its running instances, and destroying it stops them.
func TestAccSystemdUnit_template(t *testing.T) {
	requireSystemd(t)
	prefix := "tfacc-" + randomID()[:10]
	name := prefix + "@.service"
	inst := prefix + "@one.service"
	config := func(v string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  enabled = true
  timeout = "30s"
  unit = {
    description = "%s %%i"
  }
  service = {
    exec_start = ["/bin/sleep infinity"]
  }
  install = {
    wanted_by        = ["multi-user.target"]
    default_instance = "main"
  }
}`, name, v)
	}
	var firstInvocation string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, name)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("template file still exists: %v", err)
			}
			if got := systemctlOutput(t, "is-active", "--", inst); got != "inactive" {
				return fmt.Errorf("instance %s is still %q", inst, got)
			}
			if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, "multi-user.target.wants", prefix+"@main.service")); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("enablement symlink left behind: %v", err)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config("v1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "state"),
					func(*terraform.State) error {
						if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, "multi-user.target.wants", prefix+"@main.service")); err != nil {
							return fmt.Errorf("default instance not enabled: %v", err)
						}
						if out, err := exec.Command("systemctl", "start", "--", inst).CombinedOutput(); err != nil {
							return fmt.Errorf("starting %s: %v: %s", inst, err, out)
						}
						firstInvocation = systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", inst)
						return nil
					},
					checkUnitProps(inst, map[string]string{"Description": "v1 one"}),
				),
			},
			{
				Config: config("v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkUnitProps(inst, map[string]string{"Description": "v2 one", "ActiveState": "active"}),
					func(*terraform.State) error {
						if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", inst); got == firstInvocation {
							return errors.New("the instance was not restarted after its template changed")
						}
						return nil
					},
				),
			},
			{
				ResourceName:            testSystemdUnitResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeout"},
			},
		},
	})
}

// Drop-ins change units, including the template instances and scope units
// that have no unit file of their own.
func TestAccSystemdDropIn(t *testing.T) {
	requireSystemd(t)
	id := randomID()[:10]
	base := "tfacc-" + id + ".service"
	scope := "tfacc-" + id + ".scope"
	dropIn := filepath.Join(defaultSystemdUnitDir, base+".d", "50-env.conf")
	config := func(mode string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "base" {
  name    = %[1]q
  state   = "running"
  timeout = "30s"
  service = {
    exec_start = ["/bin/sleep infinity"]
  }
}

resource "sysutils_systemd_dropin" "env" {
  unit_name = sysutils_systemd_unit.base.name
  name      = "50-env"
  timeout   = "30s"
  service = {
    environment = ["MODE=%[3]s"]
  }
}

resource "sysutils_systemd_dropin" "scope" {
  unit_name = %[2]q
  name      = "limits"
  unit = {
    description = "tfacc scope"
  }
  scope = {
    runtime_max_sec = "3h"
  }
}
`, base, scope, mode)
	}
	var firstInvocation string
	var scopeCmd *exec.Cmd
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if _, err := os.Lstat(filepath.Dir(dropIn)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("drop-in directory left behind: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, scope+".d")); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("scope drop-in directory left behind: %v", err)
			}
			return checkRealUnitRemoved(base)(s)
		},
		Steps: []resource.TestStep{
			{
				Config: config("a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(dropIn, "[Service]\nEnvironment=MODE=a\n"),
					checkUnitProps(base, map[string]string{"Environment": "MODE=a", "ActiveState": "active", "DropInPaths": dropIn}),
					func(*terraform.State) error {
						firstInvocation = systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", base)
						// A scope started now picks up its drop-in.
						scopeCmd = exec.Command("systemd-run", "--scope", "--unit="+scope, "sleep", "300")
						if err := scopeCmd.Start(); err != nil {
							return err
						}
						t.Cleanup(func() { _ = exec.Command("systemctl", "stop", "--", scope).Run(); _ = scopeCmd.Wait() })
						deadline := time.Now().Add(10 * time.Second)
						for systemctlOutput(t, "is-active", "--", scope) != "active" && time.Now().Before(deadline) {
							time.Sleep(100 * time.Millisecond)
						}
						return nil
					},
					checkUnitProps(scope, map[string]string{"Description": "tfacc scope", "RuntimeMaxUSec": "3h", "ActiveState": "active"}),
				),
			},
			{
				Config: config("b"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkUnitProps(base, map[string]string{"Environment": "MODE=b", "ActiveState": "active"}),
					func(*terraform.State) error {
						if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", base); got == firstInvocation {
							return errors.New("the unit was not restarted after its drop-in changed")
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "sysutils_systemd_dropin.env",
				ImportState:             true,
				ImportStateId:           base + "/50-env",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeout"},
			},
			{
				// Removing the drop-in reloads and restarts the unit without it.
				Config: fmt.Sprintf(`
resource "sysutils_systemd_unit" "base" {
  name    = %q
  state   = "running"
  timeout = "30s"
  service = {
    exec_start = ["/bin/sleep infinity"]
  }
}`, base),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkUnitProps(base, map[string]string{"Environment": "", "ActiveState": "active", "DropInPaths": ""}),
				),
			},
		},
	})
}

// Every directive in the table that the host's systemd version should know
// is known to it: systemd-analyze verify reports no unknown key for a file
// that assigns all of them. This checks the generated table against a real
// systemd. Directives of [Scope] are only checked through the contexts they
// share with [Slice] and [Service], since scope units have no unit files.
func TestAccSystemdDirectives_knownToHost(t *testing.T) {
	requireSystemd(t)
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not found")
	}
	version := hostSystemdVersion(t)
	dir := t.TempDir()
	unknown := regexp.MustCompile(`Unknown key name '([^']+)' in section '([^']+)'`)
	checked := 0
	for _, s := range unitSectionSpecs {
		if s.Type == "" || s.Type == "scope" {
			continue
		}
		var b strings.Builder
		for _, sec := range []string{"Unit", s.Name, "Install"} {
			fmt.Fprintf(&b, "[%s]\n", sec)
			for _, d := range systemdDirectives[sec] {
				if d.Since > version || (d.Until != 0 && d.Until <= version) {
					continue
				}
				fmt.Fprintf(&b, "%s=\n", d.Key)
				checked++
			}
		}
		file := filepath.Join(dir, "tfacc-verify."+s.Type)
		writeTestFile(t, file, b.String())
		out, _ := exec.Command("systemd-analyze", "verify", "--man=no", file).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, file+":") {
				if m := unknown.FindStringSubmatch(line); m != nil {
					t.Errorf("systemd %d does not know [%s] %s=, which the table says it does", version, m[2], m[1])
				}
			}
		}
	}
	t.Logf("checked %d directive assignments against systemd %d", checked, version)
}
