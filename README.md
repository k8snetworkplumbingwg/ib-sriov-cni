[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0)
[![Go Report Card](https://goreportcard.com/badge/github.com/Mellanox/ib-sriov-cni)](https://goreportcard.com/report/github.com/Mellanox/ib-sriov-cni)
[![Build Status](https://travis-ci.com/Mellanox/ib-sriov-cni.svg?branch=master)](https://travis-ci.com/Mellanox/ib-sriov-cni)
[![Coverage Status](https://coveralls.io/repos/github/Mellanox/ib-sriov-cni/badge.svg)](https://coveralls.io/github/Mellanox/ib-sriov-cni)

   * [InfiniBand SR-IOV CNI plugin](#infiniband-sr-iov-cni-plugin)
      * [Build](#build)
      * [Enable SR-IOV](#enable-sr-iov)
         * [Using Upstream Mstflint](#using-upstream-mstflint)
         * [Using Mellanox OFED](#using-mellanox-ofed)
      * [Configuration reference](#configuration-reference)
      * [Usage](#usage)
      * [Limitations](limitations)

# InfiniBand SR-IOV CNI plugin
NIC with [SR-IOV](http://blog.scottlowe.org/2009/12/02/what-is-sr-iov/) capabilities work by introducing the idea of physical functions (PFs) and virtual functions (VFs). 

A PF is used by host and VF configurations are applied through the PF. Each VF can be treated as a separate physical NIC and assigned to one container.

## Build

To build the plugin binary:

```
# make
```

Upon successful build the plugin binary will be available in `build/ib-sriov`.

## Enable SR-IOV

IB-SRIOV-CNI support Mellanox ConnectX®-4/ConnectX®-5/ConnectX®-6 adapter cards.

### Using Upstream Mstflint

To enable SR-IOV functionality using upstream mstflint, the following steps are required:

Install Mstflint package.
```
# yum install -y mstflint

```

Enable SR-IOV
```
# lspci | grep Mellanox
02:00.0 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]
02:00.1 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]

# mstconfig -d 0000:02:00.0 set SRIOV_EN=1 NUM_OF_VFS=8

Device #1:
----------

Device type:    ConnectX5       
Name:           MCX556A-ECA_Ax  
Description:    ConnectX-5 VPI adapter card; EDR IB (100Gb/s) and 100GbE; dual-port QSFP28; PCIe3.0 x16; tall bracket; ROHS R6
Device:         0000:02:00.0    

Configurations:                              Next Boot       New
         SRIOV_EN                            False(0)       True(1)         
         NUM_OF_VFS                          0              8               

 Apply new Configuration? (y/n) [n] : y
Applying... Done!
-I- Please reboot machine to load new configurations.

```

Reboot the machine
```
# reboot
```

Create SR-IOV VFs

```
# echo 4 > /sys/class/net/ib0/device/sriov_numvfs

# lspci | grep Mellanox
02:00.0 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]
02:00.1 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]
02:00.2 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.3 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.4 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.5 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]

# ip link show
...
ib2: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband c6:6d:7d:dd:2a:d5 brd ff:ff:ff:ff:ff:ff
ib3: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband 42:3e:07:68:da:fb brd ff:ff:ff:ff:ff:ff
ib4: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband 42:68:f2:aa:c2:27 brd ff:ff:ff:ff:ff:ff
ib5: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
...
```

To change the number of VFs reset the number to 0 then set the needed number

```
echo 0 > /sys/class/net/ib0/device/sriov_numvfs
echo 8 > /sys/class/net/ib0/device/sriov_numvfs
```

### Using Mellanox OFED

To enable SR-IOV functionality using Mellnaox's OFED, the following steps are required:

1- Enable SR-IOV in the NIC's Firmware.

> Installing Mellanox Management Tools (MFT) or mstflint is a pre-requisite, MFT can be downloaded from [here](http://www.mellanox.com/page/management_tools), mstflint package available in the various distros and can be downloaded from [here](https://github.com/Mellanox/mstflint).

Use Mellanox Firmware Tools package to enable and configure SR-IOV in firmware

```
# mst start
Starting MST (Mellanox Software Tools) driver set
Loading MST PCI module - Success
Loading MST PCI configuration module - Success
Create devices
```

Locate the HCA device on the desired PCI slot

```
# mst status
MST modules:
------------
    MST PCI module loaded
    MST PCI configuration module loaded
MST devices:
------------
/dev/mst/mt4115_pciconf0         - PCI configuration cycles access.
...
```

Enable SR-IOV

```
# mlxconfig -d /dev/mst/mt4115_pciconf0 set SRIOV_EN=1 NUM_OF_VFS=8
...
Apply new Configuration? ? (y/n) [n] : y
Applying... Done!
-I- Please reboot machine to load new configurations.
```

Reboot the machine
```
# reboot
```

2- Enable SR-IOV in the NIC's Driver.

```
# ibdev2netdev
mlx5_0 port 1 ==> ib0 (Up)
mlx5_1 port 1 ==> ib1 (Down)

# echo 4 > /sys/class/net/ib0/device/sriov_numvfs
# ibdev2netdev -v
0000:02:00.0 mlx5_0 (MT4115 - MT1523X04353) CX456A - ConnectX-4 QSFP fw 12.23.1020 port 1 (ACTIVE) ==> ib0 (Up)
0000:02:00.1 mlx5_1 (MT4115 - MT1523X04353) CX456A - ConnectX-4 QSFP fw 12.23.1020 port 1 (ACTIVE) ==> ib1 (Down)
0000:02:00.5 mlx5_2 (MT4116 - NA)  fw 12.23.1020 port 1 (DOWN  ) ==> ib2 (Down)
0000:02:00.6 mlx5_3 (MT4116 - NA)  fw 12.23.1020 port 1 (DOWN  ) ==> ib3 (Down)
0000:02:00.7 mlx5_4 (MT4116 - NA)  fw 12.23.1020 port 1 (DOWN  ) ==> ib4 (Down)
0000:02:00.2 mlx5_5 (MT4116 - NA)  fw 12.23.1020 port 1 (DOWN  ) ==> ib5 (Down)

# lspci | grep Mellanox
02:00.0 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]
02:00.1 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4]
02:00.2 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.3 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.4 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]
02:00.5 Infiniband controller: Mellanox Technologies MT27700 Family [ConnectX-4 Virtual Function]

# ip link show
...
ib2: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband c6:6d:7d:dd:2a:d5 brd ff:ff:ff:ff:ff:ff
ib3: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband 42:3e:07:68:da:fb brd ff:ff:ff:ff:ff:ff
ib4: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
    link/infiniband 42:68:f2:aa:c2:27 brd ff:ff:ff:ff:ff:ff
ib5: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000
...
```

To change the number of VFs reset the number to 0 then set the needed number

```
echo 0 > /sys/class/net/ib0/device/sriov_numvfs
echo 8 > /sys/class/net/ib0/device/sriov_numvfs
```

## Configuration reference

* `name` (string, required): the name of the network
* `type` (string, required): "ib-sriov"
* `deviceID` (string, required): A valid pci address of an InfiniBand SR-IOV NIC's VF. e.g. "0000:03:02.3"
* `guid` (string, optional): InfiniBand Guid for VF.
* `pkey` (string, optional): InfiniBand pkey for VF, this field is used by [ib-kubernetes](https://www.github.com/Mellanox/ib-kubernetes) to add pkey with guid to InfiniBand subnet manager client e.g. [Mellanox UFM](https://www.mellanox.com/products/management-software/ufm), [OpenSM](https://docs.mellanox.com/display/MLNXOFEDv461000/OpenSM).
* `ipam` (dictionary, optional): IPAM configuration to be used for this network, `dhcp` is not supported.
* `link_state` (string, optional): Enforces link state for the VF. Allowed values: auto, enable, disable.
* `rdmaIsolation` (boolean, optional): Enable RDMA network namespace isolation for RDMA workloads. More information
about the system requirements to support this mode of operation can be found [here](https://github.com/Mellanox/rdma-cni)
* `ibKubernetesEnabled` (bool, optional): Enforces ib-sriov-cni to work with [ib-kubernetes](https://www.github.com/Mellanox/ib-kubernetes).

> *__Note__*: If `rdmaIsolation` is set to _true_, [`rdma-cni`](https://github.com/Mellanox/rdma-cni) should not be used.

### Supported Capabilities / Runtime configurations

ib-sriov supports the following [CNI's Capabilities / Runtime Configuration](https://github.com/containernetworking/cni/blob/master/CONVENTIONS.md#dynamic-plugin-specific-fields-capabilities--runtime-configuration):

* `infinibandGUID` (string): Dynamically assign Infiniband GUID to network interface (VF).

## Usage

```
# cat > /etc/cni/net.d/10-ib-sriov.conf <<EOF
{
    "cniVersion": "0.3.1",
    "name": "mynet",
    "type": "ib-sriov",
    "deviceID": "0000:03:02.0",
    "link_state": "enable",
    "rdmaIsolation": true,
    "ibKubernetesEnabled": false,
    "ipam": {
                "type": "host-local",
                "subnet": "10.56.217.0/24",
                "rangeStart": "10.56.217.171",
                "rangeEnd": "10.56.217.181",
                "routes": [
                        { "dst": "0.0.0.0/0" }
                ],
                "gateway": "10.56.217.1"
        }
}

EOF
```
