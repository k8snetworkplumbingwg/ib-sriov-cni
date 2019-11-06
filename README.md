   * [InfiniBand SR-IOV CNI plugin](#infiniband-sr-iov-cni-plugin)
      * [Build](#build)
      * [Enable SR-IOV](#enable-sr-iov)
      * [Configuration reference](#configuration-reference)
      * [Usage](#usage)

# InfiniBand SR-IOV CNI plugin
NIC with [SR-IOV](http://blog.scottlowe.org/2009/12/02/what-is-sr-iov/) capabilities work by introducing the idea of physical functions (PFs) and virtual functions (VFs). 

A PF is used by host and VF configurations are applied through the PF. Each VF can be treated as a separate physical NIC and assigned to one container.

## Build

To build the plugin binary:

```
# make
```

Upon successful build the plugin binary will be available in `build/ib-sriov-cni`.

## Enable SR-IOV

IB-SRIOV-CNI support Mellanox ConnectX®-4/ConnectX®-5/ConnectX®-6 adapter cards
To enable SR-IOV functionality the following steps are required:

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
# mlxconfig -d /dev/mst/mt4115_pciconf0 q set SRIOV_EN=1 NUM_OF_VFS=8
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

# echo 4 > /sys/class/net/enp2s0f0/device/sriov_numvfs
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
* `type` (string, required): "ib-sriov-cni"
* `deviceID` (string, required): A valid pci address of an InfiniBand SR-IOV NIC's VF. e.g. "0000:03:02.3"
* `guid` (string, optional): InfiniBand Guid for VF.
* `ipam` (dictionary, optional): IPAM configuration to be used for this network, `dhcp` is not supported.


## Usage

```
# cat > /etc/cni/net.d/10-ib-sriov-cni.conf <<EOF
{
    "cniVersion": "0.3.1",
    "name": "mynet",
    "type": "ib-sriov-cni",
        "deviceID": "0000:03:02.0",
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
