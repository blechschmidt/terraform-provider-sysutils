package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testTimezoneResource = "sysutils_timezone.test"

func timezoneResourceConfig(root, attrs string) string {
	p := ""
	if root != "" {
		p = rootedProvider(root)
	}
	return p + fmt.Sprintf(`
resource "sysutils_timezone" "test" {
%s
}
`, attrs)
}

func setSymlink(t *testing.T, target, link string) func() {
	return func() {
		t.Helper()
		_ = os.Remove(link)
		mustSymlink(t, target, link)
	}
}

func TestAccTimezone_rootDir(t *testing.T) {
	root := testTimezoneRoot(t, "etc/debian_version")
	lt := filepath.Join(root, "etc", "localtime")
	tz := filepath.Join(root, "etc", "timezone")
	mustSymlink(t, "/usr/share/zoneinfo/Etc/UTC", lt)
	mustWrite(t, tz, "Etc/UTC\n")
	config := func(zone string) string {
		return timezoneResourceConfig(root, fmt.Sprintf("  timezone           = %q\n  restore_on_destroy = true", zone))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy puts back exactly what was there before.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkSymlinkTarget(lt, "/usr/share/zoneinfo/Etc/UTC"),
			checkFileContent(tz, "Etc/UTC\n"),
		),
		Steps: []resource.TestStep{
			{
				Config: config("Europe/Berlin"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(lt, "../usr/share/zoneinfo/Europe/Berlin"),
					checkFileContent(tz, "Europe/Berlin\n"),
					checkNoTempFiles(filepath.Join(root, "etc")),
					resource.TestCheckResourceAttr(testTimezoneResource, "id", "system"),
					resource.TestCheckResourceAttr(testTimezoneResource, "timezone", "Europe/Berlin"),
					resource.TestCheckResourceAttr(testTimezoneResource, "restore_on_destroy", "true"),
				),
			},
			{
				Config: config("Europe/Berlin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// The symlink was changed outside Terraform.
				PreConfig: setSymlink(t, "/usr/share/zoneinfo/America/New_York", lt),
				Config:    config("Europe/Berlin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testTimezoneResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkSymlinkTarget(lt, "../usr/share/zoneinfo/Europe/Berlin"),
			},
			{
				// /etc/timezone was changed outside Terraform.
				PreConfig: setFile(t, tz, "America/New_York\n"),
				Config:    config("Europe/Berlin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testTimezoneResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFileContent(tz, "Europe/Berlin\n"),
			},
			{
				// /etc/localtime was deleted.
				PreConfig: func() { _ = os.Remove(lt) },
				Config:    config("Europe/Berlin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testTimezoneResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkSymlinkTarget(lt, "../usr/share/zoneinfo/Europe/Berlin"),
			},
			{
				// Zones reached through a symlink in the zoneinfo tree.
				Config: config("UTC"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(lt, "../usr/share/zoneinfo/UTC"),
					checkFileContent(tz, "UTC\n"),
				),
			},
			{
				ResourceName:            testTimezoneResource,
				ImportState:             true,
				ImportStateId:           "system",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"restore_on_destroy"},
			},
		},
	})
}

// TestAccTimezone_destroyKeepsZone checks the default: destroy leaves the
// zone in place, and a distribution without /etc/timezone gets none.
func TestAccTimezone_destroyKeepsZone(t *testing.T) {
	root := testTimezoneRoot(t, "etc/fedora-release")
	lt := filepath.Join(root, "etc", "localtime")
	tz := filepath.Join(root, "etc", "timezone")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkSymlinkTarget(lt, "../usr/share/zoneinfo/Etc/GMT+5"),
			checkNotExist(tz),
		),
		Steps: []resource.TestStep{
			{
				// /etc/localtime does not exist yet.
				Config: timezoneResourceConfig(root, `  timezone = "Etc/GMT+5"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(lt, "../usr/share/zoneinfo/Etc/GMT+5"),
					checkNotExist(tz),
					resource.TestCheckResourceAttr(testTimezoneResource, "restore_on_destroy", "false"),
				),
			},
		},
	})
}

// TestAccTimezone_restoreAfterImport warns rather than guessing what to
// restore for an imported resource.
func TestAccTimezone_restoreAfterImport(t *testing.T) {
	root := testTimezoneRoot(t, "")
	lt := filepath.Join(root, "etc", "localtime")
	mustSymlink(t, "../usr/share/zoneinfo/America/New_York", lt)
	config := timezoneResourceConfig(root, "  timezone           = \"America/New_York\"\n  restore_on_destroy = true")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkSymlinkTarget(lt, "../usr/share/zoneinfo/America/New_York"),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testTimezoneResource,
				ImportState:        true,
				ImportStateId:      "system",
				ImportStatePersist: true,
			},
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr(testTimezoneResource, "timezone", "America/New_York"),
			},
		},
	})
}

func TestAccTimezone_errors(t *testing.T) {
	root := testTimezoneRoot(t, "etc/debian_version")
	lt := filepath.Join(root, "etc", "localtime")
	mustSymlink(t, "/opt/zoneinfo/Europe/Berlin", lt)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      timezoneResourceConfig(root, `  timezone = "../../etc/shadow"`),
				ExpectError: regexp.MustCompile(`must\s+be\s+an\s+IANA\s+time\s+zone\s+name`),
			},
			{
				Config:      timezoneResourceConfig(root, `  timezone = "Mars/Olympus_Mons"`),
				ExpectError: regexp.MustCompile(`(?is)unknown\s+time\s+zone.*does\s+not\s+exist`),
			},
			{
				Config:      timezoneResourceConfig(root, `  timezone = "leapseconds"`),
				ExpectError: regexp.MustCompile(`not\s+a\s+compiled\s+zone\s+file`),
			},
			{
				// /etc/localtime points outside the zoneinfo directory.
				Config:        timezoneResourceConfig(root, `  timezone = "UTC"`),
				ResourceName:  testTimezoneResource,
				ImportState:   true,
				ImportStateId: "system",
				ExpectError:   regexp.MustCompile(`(?s)Cannot\s+import\s+time\s+zone.*/opt/zoneinfo/Europe/Berlin`),
			},
			{
				Config:        timezoneResourceConfig(root, `  timezone = "UTC"`),
				ResourceName:  testTimezoneResource,
				ImportState:   true,
				ImportStateId: "UTC",
				ExpectError:   regexp.MustCompile(`import\s+ID\s+of\s+sysutils_timezone\s+must\s+be\s+"system"`),
			},
		},
	})
	// Nothing was changed by the failed applies.
	if got := readLinkOrFail(t, lt); got != "/opt/zoneinfo/Europe/Berlin" {
		t.Errorf("localtime -> %q", got)
	}
}

// hostTimezoneState is the host's /etc/localtime and /etc/timezone, saved
// by requireHostTimezone and put back when the test ends.
type hostTimezoneState struct {
	localtime localtimeEntry
	tzfile    *string
}

func (s hostTimezoneState) check() resource.TestCheckFunc {
	return func(*terraform.State) error {
		cur, err := readLocaltime(localtimePath)
		if err != nil {
			return err
		}
		if cur.Kind != s.localtime.Kind || cur.Target != s.localtime.Target || !bytes.Equal(cur.Content, s.localtime.Content) {
			return fmt.Errorf("%s is %+v, want %+v", localtimePath, cur, s.localtime)
		}
		tz, err := readTimezoneFile(timezoneFilePath)
		if err != nil {
			return err
		}
		if (tz == nil) != (s.tzfile == nil) || tz != nil && *tz != *s.tzfile {
			return fmt.Errorf("%s changed", timezoneFilePath)
		}
		return nil
	}
}

// requireHostTimezone skips the test unless it may change the time zone of
// the real host: TF_ACC is set, the process is root, and tzdata is
// installed. It saves the host's configuration and restores it when the
// test ends, whatever the test did.
func requireHostTimezone(t *testing.T, zones ...string) hostTimezoneState {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	for _, z := range zones {
		if _, err := readZoneinfo(hostRoot, z); err != nil {
			t.Skipf("tzdata is not installed: %v", err)
		}
	}
	lt, err := readLocaltime(localtimePath)
	if err != nil {
		t.Skipf("cannot read %s: %v", localtimePath, err)
	}
	tz, err := readTimezoneFile(timezoneFilePath)
	if err != nil {
		t.Skipf("cannot read %s: %v", timezoneFilePath, err)
	}
	s := hostTimezoneState{localtime: lt, tzfile: tz}
	t.Cleanup(func() {
		if err := s.check()(nil); err == nil {
			return
		}
		if _, err := restoreTimezone(context.Background(), nil, hostRoot, &timezoneSnapshot{Localtime: lt, TimezoneFile: tz}); err != nil {
			t.Errorf("restoring the host's time zone: %v", err)
		}
	})
	return s
}

// TestAccTimezone_realHost changes the time zone of the machine running the
// test, with timedatectl if systemd is PID 1, and restores it on destroy.
func TestAccTimezone_realHost(t *testing.T) {
	a, b := "America/New_York", "Asia/Tokyo"
	orig := requireHostTimezone(t, a, b)
	if cur, ok := (&timezoneSnapshot{Localtime: orig.localtime}).zone(hostRoot, ""); ok && cur == a {
		a, b = b, a
	}
	config := func(zone string) string {
		return timezoneResourceConfig("", fmt.Sprintf("  timezone           = %q\n  restore_on_destroy = true", zone))
	}
	checkZone := func(zone string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			cur, err := readLocaltime(localtimePath)
			if err != nil {
				return err
			}
			if got, ok := zoneFromLinkTarget(cur.Target); cur.Kind != localtimeSymlink || !ok || got != zone {
				return fmt.Errorf("%s is %+v, want a symlink to %s", localtimePath, cur, zone)
			}
			if tz, err := readTimezoneFile(timezoneFilePath); err != nil {
				return err
			} else if tz != nil && firstLine(*tz) != zone {
				return fmt.Errorf("%s names %q, want %q", timezoneFilePath, *tz, zone)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			orig.check(),
			checkTimedatedZone(t, orig),
		),
		Steps: []resource.TestStep{
			{Config: config(a), Check: checkZone(a)},
			{
				Config: config(a),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{Config: config(b), Check: checkZone(b)},
		},
	})
}

// checkTimedatedZone checks that systemd-timedated, if systemd booted the
// host, reports the zone of orig. It keeps the zone in memory, so a restore
// that only rewrote /etc/localtime would leave it reporting another one.
func checkTimedatedZone(t *testing.T, orig hostTimezoneState) resource.TestCheckFunc {
	return func(*terraform.State) error {
		want, ok := zoneFromLinkTarget(orig.localtime.Target)
		if orig.localtime.Kind != localtimeSymlink || !ok || !(*timezoneConfig)(nil).useTimedatectl(hostRoot) {
			return nil
		}
		out, err := exec.Command("timedatectl", "show", "-p", "Timezone", "--value").Output()
		if err != nil {
			t.Logf("timedatectl show: %v", err)
			return nil
		}
		if got := strings.TrimSpace(string(out)); got != want {
			return fmt.Errorf("systemd-timedated reports %q after destroy, want %q", got, want)
		}
		return nil
	}
}
