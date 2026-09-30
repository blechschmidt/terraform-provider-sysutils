package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
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

// fakeSwapManager is an in-memory list of active swap areas for unit
// testing sysutils_swap without privileges. mkswap writes a real swap
// header, so that the resource's own header checks run unchanged.
type fakeSwapManager struct {
	mu         sync.Mutex
	active     []swapEntry
	calls      []string
	nextPri    int64
	failMkswap bool
}

func (f *fakeSwapManager) swaps() ([]swapEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.active), nil
}

func (f *fakeSwapManager) mkswap(_ context.Context, p string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("mkswap force=%v", force))
	if f.failMkswap {
		return errors.New("mkswap: failed")
	}
	return writeSwapHeaderLikeMkswap(p)
}

func (f *fakeSwapManager) swapon(_ context.Context, p string, priority *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "swapon "+p)
	h, err := readSwapHeader(p)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("swapon: %s: read swap header failed", p)
	}
	if findSwap(f.active, p) != nil {
		return fmt.Errorf("swapon: %s: swapon failed: Device or resource busy", p)
	}
	pri := f.nextPri
	if priority != nil {
		pri = *priority
	} else {
		if f.nextPri == 0 {
			f.nextPri = -2
			pri = -2
		}
		f.nextPri--
	}
	f.active = append(f.active, swapEntry{filename: p, kind: "file", sizeKiB: (h.bytes - int64(os.Getpagesize())) >> 10, priority: pri})
	return nil
}

func (f *fakeSwapManager) swapoff(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "swapoff "+p)
	return f.swapoffLocked(p)
}

func (f *fakeSwapManager) swapoffLocked(p string) error {
	for i, e := range f.active {
		if e.filename == p {
			f.active = slices.Delete(f.active, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("swapoff: %s: swapoff failed: Invalid argument", p)
}

func (f *fakeSwapManager) probe(_ context.Context, p string, device bool) (string, error) {
	data, err := readProbeData(p, device)
	if err != nil {
		return "", err
	}
	return probeSignatures(data), nil
}

// swapoffOutOfBand and swaponOutOfBand simulate changes made outside
// Terraform.
func (f *fakeSwapManager) swapoffOutOfBand(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.swapoffLocked(p)
}

func (f *fakeSwapManager) swaponOutOfBand(p string, priority int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = append(f.active, swapEntry{filename: p, kind: "file", sizeKiB: 1020, priority: priority})
}

func (f *fakeSwapManager) entry(p string) *swapEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := findSwap(f.active, p); e != nil {
		c := *e
		return &c
	}
	return nil
}

func (f *fakeSwapManager) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// swapProviderFactories returns provider factories whose sysutils_swap
// uses fstab (unless empty) and m (unless nil).
func swapProviderFactories(fstab string, m swapManager) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			swap:    &swapConfig{fstabPath: fstab, manager: m},
		}),
	}
}

// swapTestEnv is a directory for swap files and an fstab with unrelated
// content.
type swapTestEnv struct {
	dir, swapfile, fstab string
}

func newSwapTestEnv(t *testing.T) swapTestEnv {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := swapTestEnv{dir: dir, swapfile: filepath.Join(dir, "swapfile"), fstab: filepath.Join(dir, "fstab")}
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude), 0o644); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e swapTestEnv) checkFstab(want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(e.fstab)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("fstab is\n%s\nwant\n%s", data, want)
		}
		return nil
	}
}

func swapHCL(p string, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_swap" "test" {
  path = %q
%s
}
`, p, extra)
}

const testSwapResource = "sysutils_swap.test"

// checkSwapFile checks that p is a swap file of size bytes with mode 0600,
// or, with size 0, that it does not exist.
func checkSwapFile(p string, size int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if size == 0 {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s still exists (%v)", p, err)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if info.Size() != size || info.Mode() != swapFileMode {
			return fmt.Errorf("%s: size %d, mode %v; want %d, %v", p, info.Size(), info.Mode(), size, swapFileMode)
		}
		h, err := readSwapHeader(p)
		if err != nil || h == nil || h.bytes != size {
			return fmt.Errorf("%s: swap header %+v, %v", p, h, err)
		}
		return nil
	}
}

func checkFakeSwap(f *fakeSwapManager, p string, active bool, priority *int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e := f.entry(p)
		switch {
		case !active && e != nil:
			return fmt.Errorf("%s is still active", p)
		case !active:
			return nil
		case e == nil:
			return fmt.Errorf("%s is not active", p)
		case priority != nil && e.priority != *priority:
			return fmt.Errorf("%s has priority %d, want %d", p, e.priority, *priority)
		}
		return nil
	}
}

func TestSwapResource_lifecycle(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	var uuid string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkSwapFile(env.swapfile, 0),
			checkFakeSwap(f, env.swapfile, false, nil),
			env.checkFstab(testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: swapHCL(env.swapfile, "size_mib = 2"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("created"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("enabled"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("force"), knownvalue.Bool(false)),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("priority"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("uuid"), knownvalue.StringRegexp(regexp.MustCompile(`^[0-9a-f-]{36}$`))),
					statecheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("id"), knownvalue.StringExact(env.swapfile)),
				},
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 2*mib),
					checkFakeSwap(f, env.swapfile, true, nil),
					env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw 0 0\n"),
					func(s *terraform.State) error {
						uuid = s.RootModule().Resources[testSwapResource].Primary.Attributes["uuid"]
						return nil
					},
				),
			},
			{
				// A priority can only be set by enabling the area again.
				Config: swapHCL(env.swapfile, "size_mib = 2\npriority = 5"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSwapResource, tfjsonpath.New("uuid"), knownvalue.NotNull()),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeSwap(f, env.swapfile, true, ptr(int64(5))),
					env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw,pri=5 0 0\n"),
					resource.TestCheckResourceAttrPtr(testSwapResource, "uuid", &uuid),
					func(*terraform.State) error {
						if n := f.callCount("mkswap"); n != 1 {
							return fmt.Errorf("mkswap ran %d times, want once", n)
						}
						return nil
					},
				),
			},
			{
				// Resizing replaces the file with a newly formatted one.
				Config: swapHCL(env.swapfile, "size_mib = 3\npriority = 5"),
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 3*mib),
					checkFakeSwap(f, env.swapfile, true, ptr(int64(5))),
					resource.TestCheckResourceAttr(testSwapResource, "created", "true"),
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[testSwapResource].Primary.Attributes["uuid"]; got == uuid {
							return errors.New("uuid did not change after the swap file was replaced")
						}
						return nil
					},
				),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 3\nenabled = false\npersist = false"),
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 3*mib),
					checkFakeSwap(f, env.swapfile, false, nil),
					env.checkFstab(testFstabPrelude),
				),
			},
			{
				ResourceName:      testSwapResource,
				ImportState:       true,
				ImportStateId:     env.swapfile,
				ImportStateVerify: true,
				// The provider did not create an imported swap file.
				ImportStateVerifyIgnore: []string{"created"},
			},
		},
	})
}

func TestSwapResource_drift(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	config := swapHCL(env.swapfile, "size_mib = 2\npriority = 3")
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate)},
	}
	fullCheck := resource.ComposeTestCheckFunc(
		checkSwapFile(env.swapfile, 2*mib),
		checkFakeSwap(f, env.swapfile, true, ptr(int64(3))),
		env.checkFstab(testFstabPrelude+env.swapfile+" none swap sw,pri=3 0 0\n"),
	)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{Config: config, Check: fullCheck},
			{
				PreConfig:        func() { f.swapoffOutOfBand(env.swapfile) },
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            fullCheck,
			},
			{
				// Enabled again with the kernel's default priority.
				PreConfig: func() {
					f.swapoffOutOfBand(env.swapfile)
					f.swaponOutOfBand(env.swapfile, -7)
				},
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            fullCheck,
			},
			{
				PreConfig: func() {
					if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+env.swapfile+" none swap sw 0 0\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            fullCheck,
			},
			{
				PreConfig: func() {
					if err := os.WriteFile(env.fstab, []byte(testFstabPrelude), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            fullCheck,
			},
			{
				// The swap header was overwritten while the area was off.
				PreConfig: func() {
					f.swapoffOutOfBand(env.swapfile)
					if err := os.WriteFile(env.swapfile, make([]byte, 2*mib), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(testSwapResource, tfjsonpath.New("uuid")),
					},
				},
				Check: fullCheck,
			},
			{
				// The swap file was deleted.
				PreConfig: func() {
					f.swapoffOutOfBand(env.swapfile)
					if err := os.Remove(env.swapfile); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeTestCheckFunc(
					fullCheck,
					resource.TestCheckResourceAttr(testSwapResource, "created", "true"),
				),
			},
			{
				// The file grew while the area was off.
				PreConfig: func() {
					f.swapoffOutOfBand(env.swapfile)
					if err := os.Truncate(env.swapfile, 3*mib); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            fullCheck,
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestSwapResource_existingFiles(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	data := []byte(strings.Repeat("precious data\n", 100000))

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			// force replaced it, but the provider did not create it.
			checkSwapFile(env.swapfile, 2*mib),
			checkFakeSwap(f, env.swapfile, false, nil),
			env.checkFstab(testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					if err := os.WriteFile(env.swapfile, data, 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:      swapHCL(env.swapfile, "size_mib = 2"),
				ExpectError: regexp.MustCompile(`already exists\s+and\s+holds\s+no\s+swap\s+area[\s\S]*force\s+=\s+true`),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 2"),
				// Refused again, with the file untouched.
				PreConfig: func() {
					got, err := os.ReadFile(env.swapfile)
					if err != nil || string(got) != string(data) {
						t.Fatalf("existing file was modified (%v)", err)
					}
				},
				ExpectError: regexp.MustCompile(`already exists`),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 2\nforce = true"),
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 2*mib),
					checkFakeSwap(f, env.swapfile, true, nil),
					resource.TestCheckResourceAttr(testSwapResource, "created", "false"),
				),
			},
		},
	})
}

func TestSwapResource_adoptExistingSwapFile(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	if err := allocateSwapFile(context.Background(), f, env.swapfile, 2*mib, false); err != nil {
		t.Fatal(err)
	}
	// Adopted files get the ownership swapon recommends; the mode is fixed
	// as long as it grants no access to others.
	if err := os.Chmod(env.swapfile, 0o400); err != nil {
		t.Fatal(err)
	}
	f.calls = nil

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkSwapFile(env.swapfile, 2*mib),
			env.checkFstab(testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: swapHCL(env.swapfile, "size_mib = 2"),
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 2*mib),
					checkFakeSwap(f, env.swapfile, true, nil),
					resource.TestCheckResourceAttr(testSwapResource, "created", "false"),
					func(*terraform.State) error {
						if n := f.callCount("mkswap"); n != 0 {
							return fmt.Errorf("mkswap ran %d times on an existing swap file", n)
						}
						return nil
					},
				),
			},
			{
				// Resizing a file the provider did not create needs force.
				Config:      swapHCL(env.swapfile, "size_mib = 4"),
				ExpectError: regexp.MustCompile(`is\s+a\s+swap\s+area\s+of\s+2\s+MiB[\s\S]*did\s+not\s+create\s+it`),
			},
		},
	})
}

// inode returns the inode number of p.
func inode(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// TestSwapResource_exposedExistingFile checks that an existing swap file
// that other users could have opened is never enabled as it is: they may
// hold a descriptor through which they read or modify swapped-out memory,
// and chmod does not revoke it. Only force replaces it with a new file.
func TestSwapResource_exposedExistingFile(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	if err := allocateSwapFile(context.Background(), f, env.swapfile, 2*mib, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(env.swapfile, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := inode(t, env.swapfile)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(env.swapfile, "size_mib = 2"),
				ExpectError: regexp.MustCompile(`has\s+mode\s+0644[\s\S]*read\s+or\s+modify\s+swapped-out\s+memory[\s\S]*force\s+=\s+true`),
			},
			{
				Config: swapHCL(env.swapfile, "size_mib = 2\nforce = true"),
				PreConfig: func() {
					if f.entry(env.swapfile) != nil {
						t.Fatal("the exposed file was enabled")
					}
					if info, err := os.Lstat(env.swapfile); err != nil || info.Mode().Perm() != 0o644 {
						t.Fatalf("the refused file was changed: %v, %v", info, err)
					}
				},
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 2*mib),
					checkFakeSwap(f, env.swapfile, true, nil),
					func(*terraform.State) error {
						if inode(t, env.swapfile) == orig {
							return errors.New("the exposed file was enabled instead of being replaced")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestSwapResource_ownedFileExposed checks that a swap file the provider
// created and that was later opened up to other users is replaced, with
// the new UUID planned rather than reported as an inconsistent result.
func TestSwapResource_ownedFileExposed(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	var orig uint64
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		CheckDestroy:             checkSwapFile(env.swapfile, 0),
		Steps: []resource.TestStep{
			{
				Config: swapHCL(env.swapfile, "size_mib = 2"),
				Check: func(*terraform.State) error {
					orig = inode(t, env.swapfile)
					return nil
				},
			},
			{
				PreConfig: func() {
					if err := os.Chmod(env.swapfile, 0o666); err != nil {
						t.Fatal(err)
					}
				},
				Config: swapHCL(env.swapfile, "size_mib = 2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testSwapResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(env.swapfile, 2*mib),
					checkFakeSwap(f, env.swapfile, true, nil),
					resource.TestCheckResourceAttr(testSwapResource, "created", "true"),
					func(*terraform.State) error {
						if inode(t, env.swapfile) == orig {
							return errors.New("the exposed file was kept")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestSwapResource_foreignOwnedFile checks that a swap file of another
// user is not adopted by chowning it.
func TestSwapResource_foreignOwnedFile(t *testing.T) {
	requireRoot(t)
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	if err := allocateSwapFile(context.Background(), f, env.swapfile, 2*mib, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(env.swapfile, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(env.swapfile, "size_mib = 2"),
				ExpectError: regexp.MustCompile(`belongs\s+to\s+user\s+65534`),
			},
		},
	})
	if f.entry(env.swapfile) != nil {
		t.Error("the file of another user was enabled")
	}
	if info, err := os.Lstat(env.swapfile); err != nil || info.Sys().(*syscall.Stat_t).Uid != 65534 {
		t.Errorf("the file of another user was changed: %v", err)
	}
}

func TestSwapResource_refusesTakeover(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+env.swapfile+" none swap sw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(env.dir, "other")
	if err := allocateSwapFile(context.Background(), f, other, mib, false); err != nil {
		t.Fatal(err)
	}
	f.swaponOutOfBand(other, -2)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config:      swapHCL(env.swapfile, "size_mib = 1"),
				ExpectError: regexp.MustCompile(`already has a swap\s+entry`),
			},
			{
				Config:      swapHCL(other, "size_mib = 1"),
				ExpectError: regexp.MustCompile(`already\s+in\s+use\s+as\s+swap`),
			},
		},
	})
	if _, err := os.Lstat(env.swapfile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a swap file was created although the resource was refused (%v)", err)
	}
}

func TestSwapResource_import(t *testing.T) {
	env := newSwapTestEnv(t)
	f := &fakeSwapManager{}
	if err := allocateSwapFile(context.Background(), f, env.swapfile, 2*mib, false); err != nil {
		t.Fatal(err)
	}
	if err := f.swapon(context.Background(), env.swapfile, ptr(int64(4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+env.swapfile+" none swap sw,pri=4 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config:             swapHCL(env.swapfile, "size_mib = 2\npriority = 4"),
				ResourceName:       testSwapResource,
				ImportState:        true,
				ImportStateId:      env.swapfile,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					for k, want := range map[string]string{"size_mib": "2", "priority": "4", "enabled": "true", "persist": "true", "created": "false", "force": "false"} {
						if a[k] != want {
							return fmt.Errorf("%s = %q, want %q", k, a[k], want)
						}
					}
					return nil
				},
			},
			{
				Config:   swapHCL(env.swapfile, "size_mib = 2\npriority = 4"),
				PlanOnly: true,
			},
			{
				ResourceName:  testSwapResource,
				ImportState:   true,
				ImportStateId: filepath.Join(env.dir, "missing"),
				ExpectError:   regexp.MustCompile(`does not exist, is not\s+an\s+active\s+swap\s+area`),
			},
		},
	})
	// Imported swap files are kept on destroy.
	if _, err := os.Lstat(env.swapfile); err != nil {
		t.Errorf("imported swap file was deleted: %v", err)
	}
}

func TestSwapResource_validation(t *testing.T) {
	env := newSwapTestEnv(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories(env.fstab, &fakeSwapManager{}),
		Steps: []resource.TestStep{
			{Config: swapHCL(env.swapfile, ""), ExpectError: regexp.MustCompile(`size_mib\s+is\s+required`)},
			{Config: swapHCL("/dev/sdz9", "size_mib = 1"), ExpectError: regexp.MustCompile(`is\s+a\s+block\s+device,\s+which\s+is\s+used\s+whole`)},
			{Config: swapHCL(env.swapfile, "size_mib = 0"), ExpectError: regexp.MustCompile(`size_mib`)},
			{Config: swapHCL(env.swapfile, "size_mib = 1\npriority = -1"), ExpectError: regexp.MustCompile(`priority`)},
			{Config: swapHCL("relative/swap", "size_mib = 1"), ExpectError: regexp.MustCompile(`absolute`)},
			{Config: swapHCL("/dev/sdz9", ""), ExpectError: regexp.MustCompile(`(?i)block device "/dev/sdz9" does not\s+exist`)},
			{Config: swapHCL("/dev/null", ""), ExpectError: regexp.MustCompile(`is\s+not\s+a\s+block\s+device`)},
		},
	})
}

func TestSwapResource_rootDir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte(testFstabPrelude), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeSwapManager{}
	provider := fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", root)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: swapProviderFactories("", f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkSwapFile(filepath.Join(root, "swap.img"), 0),
			checkFileContent(filepath.Join(root, "etc", "fstab"), testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config:      provider + swapHCL("/swap.img", "size_mib = 1"),
				ExpectError: regexp.MustCompile(`Set\s+enabled\s+=\s+false`),
			},
			{
				Config:      provider + swapHCL("/dev/sdb2", "enabled = false"),
				ExpectError: regexp.MustCompile(`Block\s+devices\s+belong\s+to\s+the\s+running\s+host`),
			},
			{
				Config: provider + swapHCL("/swap.img", "size_mib = 1\nenabled = false\npriority = 2"),
				Check: resource.ComposeTestCheckFunc(
					checkSwapFile(filepath.Join(root, "swap.img"), mib),
					checkFileContent(filepath.Join(root, "etc", "fstab"), testFstabPrelude+"/swap.img none swap sw,pri=2 0 0\n"),
					resource.TestCheckResourceAttr(testSwapResource, "id", "/swap.img"),
					func(*terraform.State) error {
						if n := f.callCount("swapon"); n != 0 {
							return fmt.Errorf("swapon ran %d times with root_dir", n)
						}
						return nil
					},
				),
			},
			{
				Config:   provider + swapHCL("/swap.img", "size_mib = 1\nenabled = false\npriority = 2"),
				PlanOnly: true,
			},
		},
	})
	// The host's own fstab was never touched.
	if data, err := os.ReadFile("/etc/fstab"); err == nil && strings.Contains(string(data), root) {
		t.Error("the host's /etc/fstab mentions the root_dir tree")
	}
}
