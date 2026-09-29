resource "sysutils_exec" "wait_for_db" {
  # Poll until the database accepts connections, but give up after 2 minutes.
  # On timeout the whole process group (the shell and every pg_isready it
  # started) is killed and the apply fails.
  command = ["/bin/sh", "-c", "until pg_isready -h 127.0.0.1; do sleep 2; done"]
  timeout = "2m"
}
