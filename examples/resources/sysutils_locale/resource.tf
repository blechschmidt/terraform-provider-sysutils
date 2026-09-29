# US English messages, with British date formats and German paper sizes.
# The file (/etc/default/locale on Debian and Ubuntu, /etc/locale.conf
# elsewhere) is detected automatically, and locales that are not installed
# are compiled.
resource "sysutils_locale" "this" {
  lang = "en_US.UTF-8"
  lc = {
    LC_TIME  = "en_GB.UTF-8"
    LC_PAPER = "de_DE.UTF-8"
  }
  generate = true
}
