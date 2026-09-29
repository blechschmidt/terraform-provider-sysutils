package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// fakeMounter is an in-memory mount table for unit testing sysutils_mount
// without privileges. Mounts stack like in the kernel: unmount removes the
// most recent mount at a mount point.
type fakeMounter struct {
	mu    sync.Mutex
	table []mountEntry
	calls []string
	// busy mount points cannot be unmounted.
	busy map[string]bool
}

func newFakeMounter() *fakeMounter { return &fakeMounter{busy: map[string]bool{}} }

// mountProviderFactories returns provider factories whose sysutils_mount
// uses fstab and m. A nil m selects the real mount(8).
func mountProviderFactories(fstab string, m mounter) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			mount:   &mountConfig{fstabPath: fstab, mounter: m},
		}),
	}
}

func (f *fakeMounter) mounts() ([]mountEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.table), nil
}

func fakeMountOptions(options []string) []string {
	if slices.Contains(options, "ro") {
		return []string{"ro", "relatime"}
	}
	return []string{"rw", "relatime"}
}

func (f *fakeMounter) mount(_ context.Context, device, target, fstype string, options []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("mount %s %s %s %s", fstype, strings.Join(options, ","), device, target))
	if fstype == "bogus" {
		return errors.New("mount: unknown filesystem type 'bogus'")
	}
	f.table = append(f.table, mountEntry{mountPoint: target, root: "/", fstype: fstype, source: device, options: fakeMountOptions(options)})
	return nil
}

// mountOutOfBand simulates a mount made outside Terraform.
func (f *fakeMounter) mountOutOfBand(device, target, fstype string, options ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.table = append(f.table, mountEntry{mountPoint: target, root: "/", fstype: fstype, source: device, options: fakeMountOptions(options)})
}

func (f *fakeMounter) remount(_ context.Context, device, target string, options []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("remount %s %s %s", strings.Join(options, ","), device, target))
	e := topMount(f.table, target)
	if e == nil {
		return fmt.Errorf("mount: %s: not mounted", target)
	}
	e.options = fakeMountOptions(options)
	return nil
}

func (f *fakeMounter) unmount(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "umount "+target)
	return f.unmountLocked(target)
}

func (f *fakeMounter) unmountLocked(target string) error {
	if f.busy[target] {
		return fmt.Errorf("umount: %s: target is busy", target)
	}
	for i := len(f.table) - 1; i >= 0; i-- {
		if f.table[i].mountPoint == target {
			f.table = slices.Delete(f.table, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("umount: %s: not mounted", target)
}

// unmountOutOfBand simulates an unmount made outside Terraform.
func (f *fakeMounter) unmountOutOfBand(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.unmountLocked(target)
}

func (f *fakeMounter) setBusy(target string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[target] = busy
}

// top returns a copy of the topmost mount at target, or nil.
func (f *fakeMounter) top(target string) *mountEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := topMount(f.table, target); e != nil {
		c := *e
		return &c
	}
	return nil
}

// depth returns how many file systems are stacked at target.
func (f *fakeMounter) depth(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.table {
		if e.mountPoint == target {
			n++
		}
	}
	return n
}

func (f *fakeMounter) callCount(prefix string) int {
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

// mountTestEnv is a mount point and an fstab with unrelated content in a
// temporary directory.
type mountTestEnv struct {
	dir, mountPoint, fstab string
}

const testFstabPrelude = `# /etc/fstab: static file system information.
#
# <file system> <mount point> <type> <options> <dump> <pass>
UUID=0a1b2c3d / ext4 errors=remount-ro 0 1

# Swap
/swapfile none swap sw 0 0
`

func newMountTestEnv(t *testing.T) mountTestEnv {
	t.Helper()
	dir := t.TempDir()
	// t.TempDir may be below a symlink, e.g. on macOS; mount points must not.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := mountTestEnv{dir: dir, mountPoint: filepath.Join(dir, "mnt"), fstab: filepath.Join(dir, "fstab")}
	if err := os.Mkdir(env.mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude), 0o644); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e mountTestEnv) readFstab(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(e.fstab)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
