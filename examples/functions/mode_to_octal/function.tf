# Accept a mode in any notation and pass the octal form on.
variable "upload_dir_mode" {
  type    = string
  default = "u=rwx,g=rwxs,o="
}

resource "sysutils_directory" "uploads" {
  path = "/srv/app/uploads"
  mode = provider::sysutils::mode_to_octal(var.upload_dir_mode) # "2770"
}

data "sysutils_file" "shadow" {
  path = "/etc/shadow"
}

# Compare modes written in different notations.
check "shadow_not_world_readable" {
  assert {
    condition     = data.sysutils_file.shadow.mode != provider::sysutils::mode_to_octal("rw-r--r--")
    error_message = "/etc/shadow is world-readable."
  }
}
