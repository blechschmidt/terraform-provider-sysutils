package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"golang.org/x/sys/unix"
)

// fwNetns is a network namespace of its own for a firewall acceptance test.
// The provider under test runs nft and iptables through nsenter inside it,
// so the host's firewall is never touched.
type fwNetns struct {
	t *testing.T
	// path is the namespace's /proc/<pid>/ns/net.
	path string
}

// newFirewallNetns creates a network namespace, held open by a sleep
// process, and skips the test if that is not possible: when not running as
// root, without unshare or nsenter, or in a container without the
// privileges. It also skips if the tools of the given backends do not work
// inside the namespace.
func newFirewallNetns(t *testing.T, tools ...string) *fwNetns {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skipf("acceptance test; set %s=1", resource.EnvTfAcc)
	}
	if os.Geteuid() != 0 {
		t.Skip("firewall acceptance tests require root")
	}
	for _, tool := range []string{"unshare", "nsenter"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	cmd := exec.Command("unshare", "--net", "--", "sleep", "infinity")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot run unshare: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Skipf("cannot read the network namespace: %v", err)
	}
	ns := &fwNetns{t: t, path: fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid)}
	// unshare creates the namespace and then executes sleep in the same
	// process; wait until it has.
	for deadline := time.Now().Add(10 * time.Second); ; {
		select {
		case err := <-exited:
			exited <- err
			t.Skipf("cannot create a network namespace (unshare --net): %v", err)
		default:
		}
		if cur, err := os.Readlink(ns.path); err == nil && cur != self {
			// comm, not exe: on Alpine, sleep is a symlink to busybox.
			if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", cmd.Process.Pid)); err == nil && strings.TrimSpace(string(comm)) == "sleep" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("unshare did not create a network namespace in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, tool := range tools {
		var check []string
		switch tool {
		case "nft":
			check = []string{"nft", "list", "tables"}
		case "ip":
			check = []string{"ip", "link", "show", "lo"}
		default:
			check = []string{tool, "-w", "-S"}
		}
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
		if out, err := ns.exec(check...); err != nil {
			t.Skipf("%s does not work in a network namespace: %v: %s", tool, err, out)
		}
	}
	// The provider must never have touched the host's firewall.
	t.Cleanup(func() { checkHostFirewallUntouched(t) })
	return ns
}

// runner returns a commandRunner that runs commands inside the namespace.
func (ns *fwNetns) runner() commandRunner {
	return func(ctx context.Context, spec execSpec) (*execResult, error) {
		spec.Argv = append([]string{"nsenter", "--net=" + ns.path, "--"}, spec.Argv...)
		return runCommand(ctx, spec)
	}
}

// exec runs argv inside the namespace and returns its combined output.
func (ns *fwNetns) exec(argv ...string) (string, error) {
	out, err := exec.Command("nsenter", append([]string{"--net=" + ns.path, "--"}, argv...)...).CombinedOutput()
	return string(out), err
}

func (ns *fwNetns) mustExec(argv ...string) string {
	ns.t.Helper()
	out, err := ns.exec(argv...)
	if err != nil {
		ns.t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}
	return out
}

// nftScript runs an nft script inside the namespace.
func (ns *fwNetns) nftScript(script string) error {
	cmd := exec.Command("nsenter", "--net="+ns.path, "--", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft -f: %v: %s", err, out)
	}
	return nil
}

// nftTableText lists the provider's table, or returns "" if it does not
// exist.
func (ns *fwNetns) nftTableText() string {
	out, err := ns.exec("nft", "-a", "list", "table", "inet", nftTable)
	if err != nil {
		if strings.Contains(out, "No such file or directory") {
			return ""
		}
		ns.t.Fatalf("nft list table: %v: %s", err, out)
	}
	return out
}

// nftHandle returns the handle of the rule tagged with name.
func (ns *fwNetns) nftHandle(name string) string {
	ns.t.Helper()
	m := regexp.MustCompile(`comment "` + regexp.QuoteMeta(firewallTagPrefix+name) + `[^"]*" # handle (\d+)`).FindStringSubmatch(ns.nftTableText())
	if m == nil {
		ns.t.Fatalf("no nft rule tagged %s", name)
	}
	return m[1]
}

// providerFactories serves the provider with nft and iptables confined to
// the namespace.
func (ns *fwNetns) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", firewall: &firewallConfig{run: ns.runner()}}),
	}
}

// check returns a TestCheckFunc that fails unless fn reports no error.
func (ns *fwNetns) check(fn func() error) resource.TestCheckFunc {
	return func(*terraform.State) error { return fn() }
}

func (ns *fwNetns) expectNft(patterns ...string) resource.TestCheckFunc {
	return ns.check(func() error {
		text := ns.nftTableText()
		for _, p := range patterns {
			if !regexp.MustCompile(p).MatchString(text) {
				return fmt.Errorf("nft table does not match %s:\n%s", p, text)
			}
		}
		return nil
	})
}

func (ns *fwNetns) expectNoNftTable() resource.TestCheckFunc {
	return ns.check(func() error {
		if text := ns.nftTableText(); text != "" {
			return fmt.Errorf("the provider's table still exists:\n%s", text)
		}
		return nil
	})
}

// expectIptables checks the rules of cmd (iptables or ip6tables) that
// carry the provider's tag: exactly the given -S lines.
func (ns *fwNetns) expectIptables(cmd string, lines ...string) resource.TestCheckFunc {
	return ns.check(func() error {
		out, err := ns.exec(cmd, "-w", "-S")
		if err != nil {
			return fmt.Errorf("%s -S: %v: %s", cmd, err, out)
		}
		var got []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, firewallTagPrefix) {
				got = append(got, l)
			}
		}
		if strings.Join(got, "\n") != strings.Join(lines, "\n") {
			return fmt.Errorf("%s rules:\n%s\nwant:\n%s", cmd, strings.Join(got, "\n"), strings.Join(lines, "\n"))
		}
		return nil
	})
}

// checkHostFirewallUntouched fails the test if a rule of the provider shows
// up in the host's own firewall.
func checkHostFirewallUntouched(t *testing.T) {
	t.Helper()
	for _, argv := range [][]string{{"nft", "list", "ruleset"}, {"iptables", "-w", "-S"}, {"ip6tables", "-w", "-S"}} {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			continue
		}
		if strings.Contains(string(out), firewallTagPrefix) || strings.Contains(string(out), nftTable) {
			t.Errorf("the host's firewall (%s) contains a rule of the provider:\n%s", strings.Join(argv, " "), out)
		}
	}
}

func TestAccFirewallRule_nftablesLifecycle(t *testing.T) {
	ns := newFirewallNetns(t, "nft")
	const addr = "sysutils_firewall_rule.ssh"
	config := func(ports, action, extra string) string {
		return fmt.Sprintf(`
resource "sysutils_firewall_rule" "ssh" {
  name              = "ssh"
  backend           = "nftables"
  family            = "ipv4"
  chain             = "input"
  protocol          = "tcp"
  source            = "10.0.0.0/8"
  destination_ports = [%s]
  in_interface      = "eth0"
  action            = %q
  comment           = "allow ssh"
  %s
}
`, ports, action, extra)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: ns.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config: config(`"22", "8000-8100"`, "accept", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", "ssh"),
					resource.TestCheckResourceAttr(addr, "active_backend", "nftables"),
					ns.expectNft(
						`type filter hook input priority filter; policy accept;`,
						`iifname "eth0" ip saddr 10\.0\.0\.0/8 tcp dport \{ 22, 8000-8100 \} accept comment "tf-sysutils:ssh allow ssh"`,
					),
				),
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     "nftables:ssh",
				ImportStateVerify: true,
			},
			{
				// Import with backend "auto" searches every backend.
				ResourceName:            addr,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"backend"},
			},
			{
				// Updates replace the rule in place, keeping its handle.
				PreConfig: func() { t.Setenv("FW_HANDLE", ns.nftHandle("ssh")) },
				Config:    config(`"2222"`, "drop", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					ns.expectNft(`tcp dport 2222 drop comment "tf-sysutils:ssh allow ssh"`),
					ns.check(func() error {
						if h := ns.nftHandle("ssh"); h != os.Getenv("FW_HANDLE") {
							return fmt.Errorf("handle changed from %s to %s", os.Getenv("FW_HANDLE"), h)
						}
						return nil
					}),
				),
			},
			{
				// A change made by hand shows up as drift of the attribute.
				PreConfig: func() {
					if err := ns.nftScript(fmt.Sprintf("replace rule inet %s input handle %s %s\n", nftTable, ns.nftHandle("ssh"),
						`meta nfproto ipv4 iifname "eth0" ip saddr 10.0.0.0/8 tcp dport 2223 drop comment "tf-sysutils:ssh allow ssh"`)); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "destination_ports.#", "1"),
					resource.TestCheckTypeSetElemAttr(addr, "destination_ports.*", "2223"),
				),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config(`"2222"`, "drop", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				Check: ns.expectNft(`tcp dport 2222 drop`),
			},
			{
				// A rule deleted by hand is added again.
				PreConfig: func() {
					if err := ns.nftScript(fmt.Sprintf("delete rule inet %s input handle %s\n", nftTable, ns.nftHandle("ssh"))); err != nil {
						t.Fatal(err)
					}
				},
				Config: config(`"2222"`, "drop", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionCreate)},
				},
				Check: ns.expectNft(`tcp dport 2222 drop`),
			},
			{
				// A change the attributes cannot express, here a counter, is
				// replaced by the next apply.
				PreConfig: func() {
					if err := ns.nftScript(fmt.Sprintf("replace rule inet %s input handle %s %s\n", nftTable, ns.nftHandle("ssh"),
						`meta nfproto ipv4 iifname "eth0" ip saddr 10.0.0.0/8 tcp dport 2222 counter drop comment "tf-sysutils:ssh allow ssh"`)); err != nil {
						t.Fatal(err)
					}
				},
				Config: config(`"2222"`, "drop", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				Check: ns.check(func() error {
					if text := ns.nftTableText(); strings.Contains(text, "counter") {
						return fmt.Errorf("counter still there:\n%s", text)
					}
					return nil
				}),
			},
			{
				// Moving the rule to another chain.
				Config: strings.Replace(strings.Replace(config(`"2222"`, "drop", ""), `"input"`, `"forward"`, 1), `in_interface`, `out_interface`, 1),
				Check: ns.expectNft(
					`chain forward \{`,
					`oifname "eth0" ip saddr 10\.0\.0\.0/8 tcp dport 2222 drop comment "tf-sysutils:ssh allow ssh"`,
				),
			},
		},
		// Destroying the last rule removes the table.
		CheckDestroy: func(*terraform.State) error {
			if text := ns.nftTableText(); text != "" {
				return fmt.Errorf("table left behind:\n%s", text)
			}
			return nil
		},
	})
}

func TestAccFirewallRule_iptablesLifecycle(t *testing.T) {
	ns := newFirewallNetns(t, "iptables", "ip6tables")
	const addr = "sysutils_firewall_rule.web"
	config := func(family, extra string) string {
		return fmt.Sprintf(`
resource "sysutils_firewall_rule" "web" {
  name              = "web"
  backend           = "iptables"
  family            = %q
  chain             = "input"
  protocol          = "tcp"
  destination_ports = ["80", "443"]
  action            = "accept"
  %s
}
`, family, extra)
	}
	const v4 = `-A INPUT -p tcp -m multiport --dports 80,443 -m comment --comment "tf-sysutils:web" -j ACCEPT`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: ns.providerFactories(),
		Steps: []resource.TestStep{
			{
				// inet adds one rule with iptables and one with ip6tables.
				Config: config("inet", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "active_backend", "iptables"),
					ns.expectIptables("iptables", v4),
					ns.expectIptables("ip6tables", v4),
				),
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     "iptables:web",
				ImportStateVerify: true,
			},
			{
				// Removing the IPv6 half by hand is drift: the pair no longer
				// forms an inet rule, so the rule reads as family ipv4.
				PreConfig: func() {
					ns.mustExec("ip6tables", "-w", "-D", "INPUT", "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-m", "comment", "--comment", "tf-sysutils:web", "-j", "ACCEPT")
				},
				Config: config("inet", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					ns.expectIptables("iptables", v4),
					ns.expectIptables("ip6tables", v4),
				),
			},
			{
				// A copy added by hand is removed.
				PreConfig: func() {
					ns.mustExec("iptables", "-w", "-A", "INPUT", "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-m", "comment", "--comment", "tf-sysutils:web", "-j", "ACCEPT")
				},
				Config: config("inet", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				Check: ns.expectIptables("iptables", v4),
			},
			{
				// Narrowing to IPv4 with a source removes the IPv6 rule.
				Config: config("ipv4", `source = "192.0.2.1"
  in_interface = "lo"
  comment = "web from the proxy"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "source", "192.0.2.1"),
					ns.expectIptables("iptables", `-A INPUT -s 192.0.2.1/32 -i lo -p tcp -m multiport --dports 80,443 -m comment --comment "tf-sysutils:web web from the proxy" -j ACCEPT`),
					ns.expectIptables("ip6tables"),
				),
			},
			{
				// The /32 that iptables prints is not drift.
				Config:   config("ipv4", "source = \"192.0.2.1\"\n  in_interface = \"lo\"\n  comment = \"web from the proxy\""),
				PlanOnly: true,
			},
			{
				// A change by hand is drift of the attribute.
				PreConfig: func() {
					ns.mustExec("iptables", "-w", "-R", "INPUT", "1", "-s", "192.0.2.2/32", "-i", "lo", "-p", "tcp", "-m", "multiport", "--dports", "80,443", "-m", "comment", "--comment", "tf-sysutils:web web from the proxy", "-j", "ACCEPT")
					// A rule of someone else after it.
					ns.mustExec("iptables", "-w", "-A", "INPUT", "-p", "udp", "-j", "DROP")
				},
				RefreshState:       true,
				Check:              resource.TestCheckResourceAttr(addr, "source", "192.0.2.2"),
				ExpectNonEmptyPlan: true,
			},
			{
				// The update keeps the rule's position in the chain.
				Config: config("ipv4", "source = \"192.0.2.1\"\n  in_interface = \"lo\"\n  comment = \"web from the proxy\""),
				Check: ns.check(func() error {
					out := ns.mustExec("iptables", "-w", "-S", "INPUT")
					want := "-P INPUT ACCEPT\n" +
						`-A INPUT -s 192.0.2.1/32 -i lo -p tcp -m multiport --dports 80,443 -m comment --comment "tf-sysutils:web web from the proxy" -j ACCEPT` + "\n" +
						"-A INPUT -p udp -j DROP\n"
					if out != want {
						return fmt.Errorf("INPUT chain:\n%s\nwant:\n%s", out, want)
					}
					return nil
				}),
			},
			{
				Config: strings.Replace(config("ipv6", `destination = "2001:db8::/32"`), `"accept"`, `"reject"`, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					ns.expectIptables("iptables"),
					ns.expectIptables("ip6tables", `-A INPUT -d 2001:db8::/32 -p tcp -m multiport --dports 80,443 -m comment --comment "tf-sysutils:web" -j REJECT --reject-with icmp6-port-unreachable`),
				),
			},
		},
		CheckDestroy: func(s *terraform.State) error {
			if err := ns.expectIptables("iptables")(s); err != nil {
				return err
			}
			return ns.expectIptables("ip6tables")(s)
		},
	})
}

// TestAccFirewallRule_backends covers backend "auto", which picks nftables
// where nft works, and moving a rule from one backend to the other.
func TestAccFirewallRule_backends(t *testing.T) {
	ns := newFirewallNetns(t, "nft", "iptables")
	const addr = "sysutils_firewall_rule.icmp"
	config := func(backend string) string {
		return fmt.Sprintf(`
resource "sysutils_firewall_rule" "icmp" {
  name     = "no-ping"
  backend  = %q
  family   = "ipv4"
  chain    = "input"
  protocol = "icmp"
  action   = "drop"
}
`, backend)
	}
	const line = `-A INPUT -p icmp -m comment --comment "tf-sysutils:no-ping" -j DROP`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: ns.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config: config("auto"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "active_backend", "nftables"),
					ns.expectNft(`meta l4proto icmp drop comment "tf-sysutils:no-ping"`),
				),
			},
			{
				Config: config("iptables"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "active_backend", "iptables"),
					ns.expectNoNftTable(),
					ns.expectIptables("iptables", line),
				),
			},
			{
				// "auto" keeps the rule where it is.
				Config: config("auto"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "active_backend", "iptables"),
					ns.expectIptables("iptables", line),
					ns.expectNoNftTable(),
				),
			},
		},
	})
}

// TestAccFirewallRule_existing checks that a rule with the name of a new
// resource is not taken over, and that rules of other resources and rules
// added by hand survive a destroy.
func TestAccFirewallRule_existing(t *testing.T) {
	ns := newFirewallNetns(t, "nft")
	if err := ns.nftScript(fmt.Sprintf("add table inet %[1]s\n%[2]s\nadd rule inet %[1]s input udp dport 53 accept comment \"tf-sysutils:dns\"\nadd rule inet %[1]s input tcp dport 9 drop\n",
		nftTable, nftChainDecl("input"))); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: ns.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_firewall_rule" "dns" {
  name              = "dns"
  backend           = "nftables"
  chain             = "input"
  protocol          = "udp"
  destination_ports = ["53"]
  action            = "accept"
}
`,
				ExpectError: regexp.MustCompile(`already\s+has\s+a\s+rule\s+named\s+"dns"`),
			},
			{
				Config: `
resource "sysutils_firewall_rule" "other" {
  name    = "other"
  backend = "nftables"
  chain   = "output"
  action  = "accept"
}
`,
				Check: ns.expectNft(`chain output`, `accept comment "tf-sysutils:other"`),
			},
		},
		CheckDestroy: func(*terraform.State) error {
			text := ns.nftTableText()
			if strings.Contains(text, "tf-sysutils:other") || !strings.Contains(text, "tf-sysutils:dns") || !strings.Contains(text, "tcp dport 9 drop") {
				return fmt.Errorf("unexpected table after destroy:\n%s", text)
			}
			return nil
		},
	})
}

// inNetns runs fn on an OS thread that has joined the namespace, so that
// sockets fn creates belong to it.
func (ns *fwNetns) inNetns(fn func()) {
	ns.t.Helper()
	runtime.LockOSThread()
	orig, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		runtime.UnlockOSThread()
		ns.t.Fatal(err)
	}
	defer func() { _ = orig.Close() }()
	target, err := os.Open(ns.path)
	if err != nil {
		runtime.UnlockOSThread()
		ns.t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		ns.t.Fatal(err)
	}
	defer func() {
		// If the thread cannot return to its namespace, it stays locked and
		// is terminated when the goroutine exits.
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err == nil {
			runtime.UnlockOSThread()
		}
	}()
	fn()
}

// TestAccFirewallRule_traffic checks that the rules take effect: a reject
// rule makes connections to a listening port fail, for both backends.
func TestAccFirewallRule_traffic(t *testing.T) {
	for _, backend := range []string{firewallBackendNftables, firewallBackendIptables} {
		t.Run(backend, func(t *testing.T) {
			tool := "nft"
			if backend == firewallBackendIptables {
				tool = "iptables"
			}
			ns := newFirewallNetns(t, tool, "ip")
			ns.mustExec("ip", "link", "set", "lo", "up")
			var ln net.Listener
			ns.inNetns(func() {
				var err error
				if ln, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
					t.Fatal(err)
				}
			})
			defer func() { _ = ln.Close() }()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
			port := ln.Addr().(*net.TCPAddr).Port
			dial := func() error {
				var err error
				ns.inNetns(func() {
					var c net.Conn
					if c, err = net.DialTimeout("tcp4", ln.Addr().String(), 2*time.Second); err == nil {
						_ = c.Close()
					}
				})
				return err
			}
			if err := dial(); err != nil {
				t.Fatalf("connecting without a rule: %v", err)
			}
			config := func(action string) string {
				return fmt.Sprintf(`
resource "sysutils_firewall_rule" "block" {
  name              = "block"
  backend           = %q
  family            = "ipv4"
  chain             = "input"
  protocol          = "tcp"
  destination       = "127.0.0.1"
  destination_ports = ["%d"]
  in_interface      = "lo"
  action            = %q
}
`, backend, port, action)
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: ns.providerFactories(),
				Steps: []resource.TestStep{
					{
						Config: config("reject"),
						Check: ns.check(func() error {
							err := dial()
							if !errors.Is(err, syscall.ECONNREFUSED) {
								return fmt.Errorf("connecting through the reject rule: got %v, want connection refused", err)
							}
							return nil
						}),
					},
					{
						Config: config("accept"),
						Check:  ns.check(dial),
					},
				},
				CheckDestroy: func(*terraform.State) error { return dial() },
			})
		})
	}
}

// TestFirewallRule_invalidConfig checks the validation of attribute
// combinations at plan time. It needs neither root nor a firewall.
func TestFirewallRule_invalidConfig(t *testing.T) {
	cases := map[string]struct{ attrs, err string }{
		"source with inet":       {`source = "10.0.0.0/8"`, `requires\s+family\s+"ipv4"\s+or\s+"ipv6"`},
		"address family":         {"family = \"ipv6\"\n source = \"10.0.0.0/8\"", `is\s+not\s+an\s+address\s+of\s+family\s+"ipv6"`},
		"host bits":              {"family = \"ipv4\"\n source = \"10.0.0.1/8\"", `has\s+host\s+bits\s+set`},
		"icmp with inet":         {`protocol = "icmp"`, `requires\s+family\s+"ipv4"`},
		"ports without protocol": {`destination_ports = ["22"]`, `Ports\s+require\s+protocol`},
		"overlapping ports":      {"protocol = \"tcp\"\n destination_ports = [\"20-30\", \"25\"]", `overlap`},
		"empty ports":            {"protocol = \"tcp\"\n destination_ports = []", `at\s+least\s+1`},
		"bad port":               {"protocol = \"tcp\"\n source_ports = [\"http\"]", `must\s+be\s+a\s+number`},
		"out_interface on input": {`out_interface = "eth0"`, `so\s+out_interface\s+cannot\s+be\s+used`},
		"interface injection":    {`in_interface = "x\" accept #"`, `Invalid\s+interface\s+name`},
		"comment injection":      {`comment = "x\" ; flush ruleset ; \""`, `Invalid\s+comment`},
		"bad name":               {``, `Invalid\s+rule\s+name`},
		"bad backend":            {`backend = "ufw"`, `value\s+must\s+be\s+one\s+of`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ruleName := "r"
			if name == "bad name" {
				ruleName = "-r"
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config: fmt.Sprintf(`
resource "sysutils_firewall_rule" "r" {
  name   = %q
  chain  = "input"
  action = "accept"
  %s
}
`, ruleName, tc.attrs),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}

// TestAccFirewallRule_parallel applies many rules at once, which Terraform
// does in parallel; the firewall lock serialises their changes.
func TestAccFirewallRule_parallel(t *testing.T) {
	for _, backend := range []string{firewallBackendNftables, firewallBackendIptables} {
		t.Run(backend, func(t *testing.T) {
			tool := "nft"
			if backend == firewallBackendIptables {
				tool = "iptables"
			}
			ns := newFirewallNetns(t, tool)
			const n = 12
			count := func() error {
				var out string
				if backend == firewallBackendNftables {
					out = ns.nftTableText()
				} else {
					out = ns.mustExec("iptables", "-w", "-S")
				}
				if got := strings.Count(out, firewallTagPrefix+"port-"); got != n {
					return fmt.Errorf("%d rules, want %d:\n%s", got, n, out)
				}
				return nil
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: ns.providerFactories(),
				Steps: []resource.TestStep{{
					Config: fmt.Sprintf(`
resource "sysutils_firewall_rule" "port" {
  count             = %d
  name              = "port-${count.index}"
  backend           = %q
  family            = "ipv4"
  chain             = "input"
  protocol          = "udp"
  destination_ports = [tostring(10000 + count.index)]
  action            = "drop"
}
`, n, backend),
					Check: ns.check(count),
				}},
				CheckDestroy: func(*terraform.State) error {
					if backend == firewallBackendNftables {
						return ns.expectNoNftTable()(nil)
					}
					return ns.expectIptables("iptables")(nil)
				},
			})
		})
	}
}
