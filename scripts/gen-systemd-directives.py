#!/usr/bin/env python3
"""Generates internal/provider/systemd_directives.go.

It reads the unit file directive tables (src/core/load-fragment-gperf.gperf.*)
of every systemd release from FIRST to LAST and records, for each section and
directive, whether the directive may be repeated (accumulates values), the
first release that knew it, and the release that retired it.

Usage: scripts/gen-systemd-directives.py [CACHE_DIR] > internal/provider/systemd_directives.go
Fetched tables are cached in CACHE_DIR (default: a temporary directory).
"""
import collections
import os
import re
import sys
import tempfile
import urllib.request

FIRST, LAST = 239, 262
URL = "https://raw.githubusercontent.com/systemd/systemd/v{v}/src/core/load-fragment-gperf.gperf.{ext}"

# Parsers whose directive may be given several times, each assignment adding
# to the previous ones (an empty assignment usually resets the list). All
# other directives are single-valued: a later assignment replaces an earlier
# one.
LIST_PARSERS = {
    "config_parse_unit_deps", "config_parse_documentation",
    "config_parse_unit_mounts_for", "config_parse_unit_requires_mounts_for",
    "config_parse_unit_condition_path", "config_parse_unit_condition_string",
    "config_parse_unit_condition_null",
    "config_parse_exec", "config_parse_environ", "config_parse_unit_env_file",
    "config_parse_pass_environ", "config_parse_unset_environ",
    "config_parse_exec_directories", "config_parse_namespace_path_strv",
    "config_parse_bind_paths", "config_parse_temporary_filesystems",
    "config_parse_capability_set", "config_parse_syscall_filter",
    "config_parse_syscall_archs", "config_parse_syscall_log",
    "config_parse_address_families", "config_parse_restrict_filesystems",
    "config_parse_restrict_network_interfaces", "config_parse_socket_listen",
    "config_parse_path_spec", "config_parse_timer", "config_parse_device_allow",
    "config_parse_io_device_weight", "config_parse_io_device_latency",
    "config_parse_io_limit", "config_parse_blockio_device_weight",
    "config_parse_blockio_bandwidth", "config_parse_in_addr_prefixes",
    "config_parse_ip_address_access", "config_parse_ip_filter_bpf_progs",
    "config_parse_bpf_foreign_program", "config_parse_cgroup_socket_bind",
    "config_parse_cgroup_nft_set", "config_parse_set_credential",
    "config_parse_load_credential", "config_parse_import_credential",
    "config_parse_log_extra_fields", "config_parse_log_filter_patterns",
    "config_parse_extension_images", "config_parse_mount_images",
    "config_parse_root_image_options", "config_parse_user_group_strv_compat",
    "config_parse_user_group_strv", "config_parse_set_status",
    "config_parse_unit_path_strv_printf", "config_parse_service_sockets",
    "config_parse_open_file", "config_parse_disable_controllers",
    "config_parse_delegate", "config_parse_namespace_flags",
    "config_parse_restrict_namespaces", "config_parse_exec_cpu_affinity",
    "config_parse_xattr", "config_parse_colon_separated_paths",
    "config_parse_luo_sessions", "config_parse_managed_oom_rules",
    "config_parse_exec_input_text", "config_parse_exec_input_data",
    "config_parse_strv", "config_parse_socket_bind_allow_deny",
}

# The [Install] section is parsed by src/shared/install.c, not the table.
INSTALL = [("Alias", True), ("WantedBy", True), ("RequiredBy", True),
           ("UpheldBy", True), ("Also", True), ("DefaultInstance", False)]

CONTEXTS = {
    "EXEC_CONTEXT_CONFIG_ITEMS": "exec",
    "KILL_CONTEXT_CONFIG_ITEMS": "kill",
    "CGROUP_CONTEXT_CONFIG_ITEMS": "resource-control",
}

ITEM = re.compile(r"^[`']?(\$1|\{\{type\}\}|[A-Z][A-Za-z]*)\.([A-Za-z0-9]+),\s*(\w+),")


def fetch(v, cache):
    for ext in ("in", "m4"):
        p = os.path.join(cache, f"v{v}.{ext}")
        if not os.path.exists(p):
            try:
                with urllib.request.urlopen(URL.format(v=v, ext=ext)) as r:
                    data = r.read()
            except Exception:
                continue
            with open(p, "wb") as f:
                f.write(data)
        return open(p).read()
    raise SystemExit(f"no directive table found for v{v}")


def parse(text):
    """Returns {(section, key): (parser, context)} for one release."""
    macros, cur, out = {}, None, []
    for line in text.splitlines():
        m = re.match(r"\{%- macro (\w+)\(type\) -%\}|m4_define\(`(\w+)',", line)
        if m:
            cur = m.group(1) or m.group(2)
            macros[cur] = []
        if cur and (line.startswith("{%- endmacro") or line.startswith(")m4_dnl")):
            cur = None
            continue
        m = re.match(r"\{\{ (\w+)\('(\w+)'\) \}\}|(\w+_CONFIG_ITEMS)\((\w+)\)m4_dnl", line)
        if m and cur is None:
            name, typ = (m.group(1), m.group(2)) if m.group(1) else (m.group(3), m.group(4))
            out += [(typ, k, p, CONTEXTS[name]) for k, p in macros[name]]
            continue
        m = ITEM.match(line)
        if not m:
            continue
        if cur:
            macros[cur].append((m.group(2), m.group(3)))
        else:
            out.append((m.group(1), m.group(2), m.group(3), None))
    res = {}
    for sec, key, parser, ctx in out:
        if (sec, key) in res and parser == "config_parse_warn_compat":
            continue  # The #else branch of a feature that is compiled in.
        if (sec, key) in res and res[(sec, key)][0] != "config_parse_warn_compat":
            continue
        res[(sec, key)] = (parser, ctx)
    return res


def snake(key):
    special = {"IPv4": "Ipv4", "IPv6": "Ipv6", "ZSwap": "Zswap", "VHangup": "Vhangup",
               "VTDisallocate": "VtDisallocate", "NSec": "Nsec", "USec": "Usec",
               "BPF": "Bpf", "NFT": "Nft", "LUO": "Luo", "TCP": "Tcp", "OOM": "Oom",
               "IO": "Io", "TTY": "Tty", "CPU": "Cpu", "NUMA": "Numa", "IP": "Ip",
               "PAM": "Pam", "UMask": "Umask", "SELinux": "Selinux", "AppArmor": "Apparmor",
               "XAttr": "Xattr", "UID": "Uid", "GID": "Gid", "FD": "Fd", "TOS": "Tos",
               "TTL": "Ttl", "UTMP": "Utmp", "PID": "Pid", "MPTCP": "Mptcp",
               "IPC": "Ipc", "SIGPIPE": "Sigpipe", "SUIDSGID": "SuidSgid", "MStack": "Mstack",
               "IOPS": "Iops", "RTPRIO": "Rtprio", "PIDFD": "Pidfd"}
    for k in sorted(special, key=len, reverse=True):
        key = key.replace(k, special[k])
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", key)
    s = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1_\2", s)
    return s.lower()


def main():
    cache = sys.argv[1] if len(sys.argv) > 1 else tempfile.mkdtemp()
    os.makedirs(cache, exist_ok=True)
    tables = {v: parse(fetch(v, cache)) for v in range(FIRST, LAST + 1)}
    real = lambda t, k: k in t and t[k][0] not in ("config_parse_warn_compat", "NULL")
    keys = []
    for v in range(FIRST, LAST + 1):
        for k in tables[v]:
            if k not in keys and real(tables[v], k) and k[0] != "Install":
                keys.append(k)
    info = collections.OrderedDict()
    for k in keys:
        versions = [v for v in range(FIRST, LAST + 1) if real(tables[v], k)]
        parser, ctx = tables[versions[-1]][k]
        if parser in ("config_parse_obsolete_unit_deps",):
            continue
        since = versions[0] if versions[0] > FIRST else 0
        until = versions[-1] + 1 if versions[-1] < LAST else 0
        info[k] = dict(list=parser in LIST_PARSERS, since=since, until=until, ctx=ctx)
    for key, lst in INSTALL:
        info[("Install", key)] = dict(list=lst, since=0, until=0, ctx=None)
    if any("$" in k[1] for k in info):
        raise SystemExit("unexpanded macro")
    sections = ["Unit", "Install", "Service", "Socket", "Mount", "Automount", "Swap",
                "Timer", "Path", "Slice", "Scope"]
    extra = sorted({s for s, _ in info} - set(sections))
    if extra:
        raise SystemExit(f"unknown sections {extra}")

    w = sys.stdout.write
    w("// Code generated by scripts/gen-systemd-directives.py; DO NOT EDIT.\n\n")
    w("package provider\n\n")
    w(f"// systemdDirectives lists the unit file directives of systemd v{FIRST} to v{LAST}\n")
    w("// per section, in systemd's own order.\n")
    w("var systemdDirectives = map[string][]unitDirective{\n")
    for sec in sections:
        w(f"\t{sec!r}: {{\n".replace("'", '"'))
        seen = set()
        for (s, key), d in info.items():
            if s != sec:
                continue
            attr = snake(key)
            if attr in seen:
                raise SystemExit(f"duplicate attribute {sec}.{attr}")
            seen.add(attr)
            ctx = f'"{d["ctx"]}"' if d["ctx"] else '""'
            w(f'\t\t{{Key: "{key}", Attr: "{attr}", List: {str(d["list"]).lower()}, Since: {d["since"]}, Until: {d["until"]}, Context: {ctx}}},\n')
        w("\t},\n")
    w("}\n")


main()
