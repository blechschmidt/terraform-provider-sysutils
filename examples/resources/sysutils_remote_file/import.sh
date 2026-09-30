# The ID is the file's absolute path. url and checksum come from the
# configuration; the next apply records them without downloading the file
# if it matches the checksum.
terraform import sysutils_remote_file.agent /usr/local/bin/agent
