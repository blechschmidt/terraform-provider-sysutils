# Decrypt a secret kept next to the configuration. The ciphertext is sent
# on standard input; the plaintext exists only in memory.
ephemeral "sysutils_exec" "decrypt" {
  command = ["/usr/bin/age", "--decrypt", "--identity", "/root/.config/age/key.txt"]
  stdin   = file("${path.module}/secrets/db.env.age")
}

resource "sysutils_file" "db_env" {
  path               = "/etc/app/db.env"
  content_wo         = ephemeral.sysutils_exec.decrypt.stdout
  content_wo_version = 1
  mode               = "0600"
}
