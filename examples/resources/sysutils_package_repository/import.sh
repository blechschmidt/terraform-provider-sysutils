# By name, with the package manager detected as for manager = "auto".
terraform import sysutils_package_repository.docker docker

# With an explicit package manager: <manager>:<name>.
terraform import sysutils_package_repository.alpine_testing apk:testing
