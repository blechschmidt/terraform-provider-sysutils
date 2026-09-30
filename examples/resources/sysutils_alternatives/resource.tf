# Make vim the system editor. The alternative is registered by the vim
# package; the selection is pinned in manual mode.
resource "sysutils_alternatives" "editor" {
  name = "editor"
  path = "/usr/bin/vim.basic"
}

# Register a JDK unpacked outside the package manager as an alternative of
# java, select it, and unregister it again on destroy.
resource "sysutils_alternatives" "java" {
  name              = "java"
  path              = "/opt/jdk-21/bin/java"
  link              = "/usr/bin/java"
  priority          = 2100
  remove_on_destroy = true
}

output "editor_mode" {
  value = sysutils_alternatives.editor.mode
}
