package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// accSysctlName is the parameter the acceptance tests change. The time the
// kernel waits for a lease holder to release a lease is harmless to change
// for a moment, is not namespaced, and has a "-" in its name.
const accSysctlName = "fs.lease-break-time"

// requireWritableSysctl skips the test unless name can be set in the real
// /proc/sys, which is read-only in unprivileged containers, and returns its
// current value, which is restored when the test ends.
func requireWritableSysctl(t *testing.T, name string) string {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	orig, err := readSysctl(defaultProcSys, name)
	if err != nil {
		t.Skipf("cannot read %s: %v", name, err)
	}
	// Writing the current value back changes nothing, but shows whether
	// the kernel lets this process set the parameter.
	if err := writeSysctl(defaultProcSys, name, orig); err != nil {
		if errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			t.Skipf("the kernel does not allow setting %s here, as in an unprivileged container: %v", name, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writeSysctl(defaultProcSys, name, orig); err != nil {
			t.Errorf("restoring %s = %s: %v", name, orig, err)
		}
	})
	return orig
}

func checkRealSysctl(name, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := readSysctl(defaultProcSys, name)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s = %q, want %q", name, got, want)
		}
		return nil
	}
}

func TestAccSysctl_lifecycle(t *testing.T) {
	orig := requireWritableSysctl(t, accSysctlName)
	n, err := strconv.Atoi(orig)
	if err != nil {
		t.Fatalf("%s = %q is not a number", accSysctlName, orig)
	}
	first, second := strconv.Itoa(n+7), strconv.Itoa(n+11)
	// The real /etc/sysctl.d is never touched.
	file := filepath.Join(t.TempDir(), "sysctl.d", "99-terraform.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkFileText(file, ""),
			checkRealSysctl(accSysctlName, second),
		),
		Steps: []resource.TestStep{
			{
				Config: sysctlHCL(accSysctlName, first, file, ""),
				Check: resource.ComposeTestCheckFunc(
					checkRealSysctl(accSysctlName, first),
					checkFileText(file, accSysctlName+" = "+first+"\n"),
				),
			},
			// A change made with sysctl -w is detected and reverted.
			{
				PreConfig: func() {
					if err := writeSysctl(defaultProcSys, accSysctlName, orig); err != nil {
						t.Fatal(err)
					}
				},
				Config: sysctlHCL(accSysctlName, first, file, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSysctlResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkRealSysctl(accSysctlName, first),
			},
			{
				Config: sysctlHCL(accSysctlName, second, file, ""),
				Check: resource.ComposeTestCheckFunc(
					checkRealSysctl(accSysctlName, second),
					checkFileText(file, accSysctlName+" = "+second+"\n"),
				),
			},
			{
				ResourceName:      testSysctlResource,
				ImportState:       true,
				ImportStateId:     accSysctlName + ":" + file,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccSysctl_invalidValue checks that a value the kernel rejects fails
// the apply and is not persisted.
func TestAccSysctl_invalidValue(t *testing.T) {
	orig := requireWritableSysctl(t, accSysctlName)
	file := filepath.Join(t.TempDir(), "99-terraform.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      sysctlHCL(accSysctlName, "not-a-number", file, ""),
				ExpectError: regexp.MustCompile(`rejected\s+the\s+value\s+as\s+invalid`),
			},
		},
	})
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("rejected value was persisted: %v", err)
	}
	if got, _ := readSysctl(defaultProcSys, accSysctlName); got != orig {
		t.Errorf("%s = %q after a rejected write, want %q", accSysctlName, got, orig)
	}
}
