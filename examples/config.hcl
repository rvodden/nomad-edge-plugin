# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

config {
  chipset = "cv22"
  operating_system = "ambalink"
  rtos_device = "/dev/ttyACM0"
  linux_device = "/dev/ttyACM1"
  nfs_server_ip = "10.0.0.1"
  nfs_server_path = "/mnt/nfs_share"
  nfs_device_path = "/tmp/mnt"
}
