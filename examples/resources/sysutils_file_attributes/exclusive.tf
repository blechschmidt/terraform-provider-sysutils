# The listed flags and no others: flags set outside Terraform, or inherited
# from the parent directory, are cleared and show up as drift.
resource "sysutils_file_attributes" "exclusive" {
  path       = "/srv/data/export.csv"
  attributes = ["d"]
  exclusive  = true
}
