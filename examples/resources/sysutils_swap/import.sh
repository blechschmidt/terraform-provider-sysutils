# By the path of the swap file or block device. /etc/fstab and /proc/swaps
# provide the other settings. Imported swap files are kept on destroy.
terraform import sysutils_swap.file /swapfile
