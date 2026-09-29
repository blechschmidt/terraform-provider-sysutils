# A line: <path>:<line>
terraform import sysutils_file_line.db_host '/etc/hosts:10.0.0.5 db.internal db'

# A block: <path>:<marker>, recognised by the {mark} placeholder
terraform import sysutils_file_line.cluster_hosts '/etc/hosts:# {mark} cluster nodes'
