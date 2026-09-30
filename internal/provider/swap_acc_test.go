package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// realSwapManager returns the production swap manager.
func realSwapManager() systemSwapManager {
	return systemSwapManager{run: runCommand, procSwaps: defaultProcSwapsPath, timeout: swapCommandTimeout}
}

// newSwapAccEnv prepares a directory and a temporary fstab for acceptance
// tests that run the real mkswap, swapon and swapoff. The real /etc/fstab
// is never touched. It skips the test unless swap can be enabled on a file
// in that directory: containers usually lack the privilege, and overlay
// file systems cannot hold swap files. Anything a failed test leaves
// enabled below the directory is disabled before it is removed.
func newSwapAccEnv(t *testing.T) swapTestEnv {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	for _, tool := range []string{"mkswap", "swapon", "swapoff"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	env := newSwapTestEnv(t)
	t.Cleanup(func() { disableSwapBelow(t, env.dir) })

	m := realSwapManager()
	probe := filepath.Join(env.dir, "probe")
	if err := allocateSwapFile(context.Background(), m, probe, mib, false); err != nil {
		t.Skipf("cannot create a swap file here: %v", err)
	}
	if err := m.swapon(context.Background(), probe, nil); err != nil {
		t.Skipf("swapon is not permitted here: %v", err)
	}
	if err := m.swapoff(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	return env
}

// disableSwapBelow disables every active swap area below dir.
func disableSwapBelow(t *testing.T, dir string) {
	m := realSwapManager()
	entries, err := m.swaps()
	if err != nil {
		t.Error(err)
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.filename, dir+"/") {
			if err := m.swapoff(context.Background(), e.filename); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}
}

func realSwap(t *testing.T, p string) *swapEntry {
	t.Helper()
	entries, err := realSwapManager().swaps()
	if err != nil {
		t.Fatal(err)
	}
	return findSwap(entries, p)
}

// checkRealSwap checks /proc/swaps for p: absent if sizeMiB is 0, else
// active with a swap area of sizeMiB and, unless nil, priority.
func checkRealSwap(t *testing.T, p string, sizeMiB int64, priority *int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e := realSwap(t, p)
		switch {
		case sizeMiB == 0 && e != nil:
			return fmt.Errorf("%s is still active: %+v", p, e)
		case sizeMiB == 0:
			return nil
		case e == nil:
			return fmt.Errorf("%s is not listed in /proc/swaps", p)
		case priority != nil && e.priority != *priority:
			return fmt.Errorf("%s has priority %d, want %d", p, e.priority, *priority)
		}
		// The kernel leaves out the header page.
		if want := sizeMiB<<10 - int64(os.Getpagesize())>>10; e.sizeKiB != want {
			return fmt.Errorf("%s has %d KiB of swap, want %d", p, e.sizeKiB, want)
		}
		return nil
	}
}

func TestAccSwap_file(t *testing.T) {
	env := newSwapAccEnv(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, nil),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealSwap(t, env.swapfile, 0, nil),
			checkSwapFile(env.swapfile, 0),
			env.checkFstab(testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: swapHCL(env.swapfile, "size_mib = 8\npriority = 7"),
				Check: resource.ComposeTestCheckFunc(
					checkRealSwap(t, env.swapfile, 8, ptr(int64(7))),
					checkSwapFile(env.swapfile, 8*mib),
					env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw,pri=7 0 0\n"),
					resource.TestCheckResourceAttr(testSwapResource, "created", "true"),
					resource.TestMatchResourceAttr(testSwapResource, "uuid", regexp.MustCompile(`^[0-9a-f-]{36}$`)),
				),
			},
			{
				// Disabled outside Terraform.
				PreConfig: func() {
					if err := realSwapManager().swapoff(context.Background(), env.swapfile); err != nil {
						t.Fatal(err)
					}
				},
				Config: swapHCL(env.swapfile, "size_mib = 8\npriority = 7"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRealSwap(t, env.swapfile, 8, ptr(int64(7))),
			},
			{
				// Enabled again outside Terraform with the kernel's priority.
				PreConfig: func() {
					m := realSwapManager()
					if err := m.swapoff(context.Background(), env.swapfile); err != nil {
						t.Fatal(err)
					}
					if err := m.swapon(context.Background(), env.swapfile, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: swapHCL(env.swapfile, "size_mib = 8\npriority = 7"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRealSwap(t, env.swapfile, 8, ptr(int64(7))),
			},
			{
				// Resized while active.
				Config: swapHCL(env.swapfile, "size_mib = 12\npriority = 3"),
				Check: resource.ComposeTestCheckFunc(
					checkRealSwap(t, env.swapfile, 12, ptr(int64(3))),
					checkSwapFile(env.swapfile, 12*mib),
					env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw,pri=3 0 0\n"),
				),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 12\nenabled = false"),
				Check: resource.ComposeTestCheckFunc(
					checkRealSwap(t, env.swapfile, 0, nil),
					checkSwapFile(env.swapfile, 12*mib),
					env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw 0 0\n"),
				),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 12"),
				Check:  checkRealSwap(t, env.swapfile, 12, nil),
			},
			{
				ResourceName:            testSwapResource,
				ImportState:             true,
				ImportStateId:           env.swapfile,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"created"},
			},
		},
	})
}

// newLoopDevice attaches a new loop device to a file of size bytes in dir
// and returns its path. It skips the test if loop devices are unavailable.
func newLoopDevice(t *testing.T, dir string, size int64) string {
	t.Helper()
	backing := filepath.Join(dir, "loop.img")
	if err := os.WriteFile(backing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(backing, size); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("losetup", "--find", "--show", backing).CombinedOutput()
	if err != nil {
		t.Skipf("cannot attach a loop device: %v: %s", err, out)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if e := realSwap(t, dev); e != nil {
			_ = realSwapManager().swapoff(context.Background(), dev)
		}
		if out, err := exec.Command("losetup", "--detach", dev).CombinedOutput(); err != nil {
			t.Errorf("detaching %s: %v: %s", dev, err, out)
		}
	})
	return dev
}

func TestAccSwap_blockDevice(t *testing.T) {
	env := newSwapAccEnv(t)
	dev := newLoopDevice(t, env.dir, 16*mib)
	// Pretend the device holds an ext4 file system.
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("\x53\xef"), 1080); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, nil),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealSwap(t, dev, 0, nil),
			env.checkFstab(testFstabPrelude),
			func(*terraform.State) error {
				// Block devices are never deleted.
				if _, err := os.Stat(dev); err != nil {
					return err
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(dev, ""),
				ExpectError: regexp.MustCompile(`holds\s+a\s+ext2/ext3/ext4\s+signature[\s\S]*force\s+=\s+true`),
			},
			{
				Config: swapHCL(dev, "force = true"),
				Check: resource.ComposeTestCheckFunc(
					checkRealSwap(t, dev, 16, nil),
					env.checkFstab(testFstabPrelude+dev+" none swap sw 0 0\n"),
					resource.TestCheckResourceAttr(testSwapResource, "created", "false"),
					resource.TestCheckNoResourceAttr(testSwapResource, "size_mib"),
				),
			},
			{
				// Already a swap area: no force needed any more, and the
				// device is not formatted again.
				Config: swapHCL(dev, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("uuid"), knownvalue.NotNull()),
					},
				},
				Check: checkRealSwap(t, dev, 16, nil),
			},
			{
				Config: swapHCL(dev, "priority = 9"),
				Check:  checkRealSwap(t, dev, 16, ptr(int64(9))),
			},
		},
	})
}

// TestAccSwap_swapHeaderUnderOtherData checks that a device whose old swap
// header survived another format, here LUKS written without wiping it, is
// not enabled as swap without force: swapon would overwrite the data.
func TestAccSwap_swapHeaderUnderOtherData(t *testing.T) {
	env := newSwapAccEnv(t)
	dev := newLoopDevice(t, env.dir, 8*mib)
	if err := realSwapManager().mkswap(context.Background(), dev, false); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("LUKS\xba\xbe"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if h, err := readSwapHeader(dev); err != nil || h == nil {
		t.Fatalf("the swap header did not survive: %+v, %v", h, err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, nil),
		CheckDestroy:             checkRealSwap(t, dev, 0, nil),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(dev, "persist = false"),
				ExpectError: regexp.MustCompile(`holds\s+a\s+swap\s+header,\s+but\s+also\s+a\s+LUKS\s+signature[\s\S]*force\s+=\s+true`),
			},
			{
				PreConfig: func() {
					if realSwap(t, dev) != nil {
						t.Fatal("the device was enabled although it was refused")
					}
				},
				Config: swapHCL(dev, "persist = false\nforce = true"),
				Check:  checkRealSwap(t, dev, 8, nil),
			},
		},
	})
}

func TestAccSwap_blankBlockDevice(t *testing.T) {
	env := newSwapAccEnv(t)
	dev := newLoopDevice(t, env.dir, 8*mib)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, nil),
		CheckDestroy:             checkRealSwap(t, dev, 0, nil),
		Steps: []resource.TestStep{
			{
				Config: swapHCL(dev, "persist = false"),
				Check: resource.ComposeTestCheckFunc(
					checkRealSwap(t, dev, 8, nil),
					env.checkFstab(testFstabPrelude),
				),
			},
		},
	})
}

// TestAccSwap_mountedDeviceRefused checks that a device in use is never
// formatted, even with force.
func TestAccSwap_mountedDeviceRefused(t *testing.T) {
	env := newSwapAccEnv(t)
	dev := newLoopDevice(t, env.dir, 8*mib)
	// An exclusive opener stands in for a mount.
	holder, err := os.OpenFile(dev, os.O_RDONLY|syscall.O_EXCL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, nil),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(dev, "force = true"),
				ExpectError: regexp.MustCompile(`is\s+in\s+use`),
			},
		},
	})
}
