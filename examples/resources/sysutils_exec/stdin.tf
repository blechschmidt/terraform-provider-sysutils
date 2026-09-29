resource "sysutils_exec" "probe" {
  command         = ["/bin/sh", "-c", "cat; grep -q needle || exit 2"]
  stdin           = "haystack\nhaystack\n"
  fail_on_nonzero = false
}

output "probe_exit" {
  value = sysutils_exec.probe.exit_code
}

output "probe_stderr" {
  value = sysutils_exec.probe.stderr
}
