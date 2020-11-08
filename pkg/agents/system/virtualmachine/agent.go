package virtualmachine

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/n0stack/n0stack/n0core/pkg/driver/iproute2"
	"github.com/ophum/humstack/pkg/agents/system/nodenetwork/utils"
	"github.com/ophum/humstack/pkg/api/meta"
	"github.com/ophum/humstack/pkg/api/system"
	"github.com/ophum/humstack/pkg/client"
	"github.com/ophum/humstack/pkg/utils/cloudinit"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type VirtualMachineAgent struct {
	client        *client.Clients
	logger        *zap.Logger
	nodeName      string
	vncDisplayMap map[int32]bool
}

const (
	VirtualMachineV0AnnotationNodeName = "virtualmachinev0/node_name"
)

func NewVirtualMachineAgent(client *client.Clients, logger *zap.Logger) *VirtualMachineAgent {

	nodeName, err := os.Hostname()
	if err != nil {
		log.Fatal(err.Error())
	}
	return &VirtualMachineAgent{
		client:        client,
		logger:        logger,
		nodeName:      nodeName,
		vncDisplayMap: map[int32]bool{},
	}
}

func (a *VirtualMachineAgent) Run() {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			grList, err := a.client.CoreV0().Group().List()
			if err != nil {
				a.logger.Error(
					"get group list",
					zap.String("msg", err.Error()),
					zap.Time("time", time.Now()),
				)
				continue
			}

			for _, group := range grList {
				nsList, err := a.client.CoreV0().Namespace().List(group.ID)
				if err != nil {
					a.logger.Error(
						"get namespace list",
						zap.String("msg", err.Error()),
						zap.Time("time", time.Now()),
					)
					continue
				}

				for _, ns := range nsList {
					vmList, err := a.client.SystemV0().VirtualMachine().List(group.ID, ns.ID)
					if err != nil {
						a.logger.Error(
							"get virtualmachine list",
							zap.String("msg", err.Error()),
							zap.Time("time", time.Now()),
						)
						continue
					}

					for _, vm := range vmList {
						oldHash := vm.ResourceHash
						if vm.Annotations[VirtualMachineV0AnnotationNodeName] != a.nodeName {
							continue
						}

						// agentは無視する
						if ignore, ok := vm.Annotations["virtualmachinev0/ignore"]; ok && ignore == "true" {
							continue
						}
						err = a.syncVirtualMachine(vm)
						if err != nil {
							a.logger.Error(
								"sync virtualmachine",
								zap.String("msg", err.Error()),
								zap.Time("time", time.Now()),
							)
							continue
						}

						if vm.ResourceHash == oldHash {
							continue
						}

						_, err := a.client.SystemV0().VirtualMachine().Update(vm)
						if err != nil {
							a.logger.Error(
								"update virtualmachine",
								zap.String("msg", err.Error()),
								zap.Time("time", time.Now()),
							)
							continue
						}
					}
				}
			}
		}
	}
}

func (a *VirtualMachineAgent) powerOffVirtualMachine(vm *system.VirtualMachine) error {
	pid, err := getPID(vm.Spec.UUID)
	if err != nil {
		return err
	}
	if pid == -1 {
		if vm.Status.State != system.VirtualMachineStateStopped {
			vm.Status.State = system.VirtualMachineStateStopped
			_, err := a.client.SystemV0().VirtualMachine().Update(vm)
			if err != nil {
				return err
			}
		}
		return nil
	}

	vm.Status.State = system.VirtualMachineStateStopping
	_, err = a.client.SystemV0().VirtualMachine().Update(vm)
	if err != nil {
		return err
	}

	p, err := os.FindProcess(int(pid))
	if err != nil {
		return err
	}

	err = p.Kill()
	if err != nil {
		return err
	}

	displayNumberString := vm.Annotations["virtualmachinev0/vnc_display_number"]
	displayNumber, err := strconv.ParseInt(displayNumberString, 10, 64)
	delete(a.vncDisplayMap, int32(displayNumber))
	vm.Status.State = system.VirtualMachineStateStopped
	_, err = a.client.SystemV0().VirtualMachine().Update(vm)
	return err
}

func (a *VirtualMachineAgent) powerOnVirtualMachine(vm *system.VirtualMachine) error {
	pid, err := getPID(vm.Spec.UUID)
	if err != nil {
		return err
	}

	if pid != -1 && vm.Status.State == system.VirtualMachineStateRunning {
		return nil
	}

	vm.Status.State = system.VirtualMachineStatePending
	if _, err = a.client.SystemV0().VirtualMachine().Update(vm); err != nil {
		return err
	}

	disks := []string{}
	for _, bsID := range vm.Spec.BlockStorageIDs {
		bs, err := a.client.SystemV0().BlockStorage().Get(vm.Group, vm.Namespace, bsID)
		if err != nil {
			return err
		}

		if bs.Status.State != system.BlockStorageStateActive {
			vm.Status.State = system.VirtualMachineStatePending
			return fmt.Errorf("BlockStorage is not active")
		}

		disks = append(disks,
			"-drive",
			fmt.Sprintf("file=./blockstorages/%s/%s/%s,format=qcow2", vm.Group, vm.Namespace, bs.ID),
		)
	}

	vcpus := withUnitToWithoutUnit(vm.Spec.LimitVcpus)
	vcpusInt, err := strconv.ParseInt(vcpus, 10, 64)
	if err != nil {
		return err
	}
	nics := []string{}
	tapNames := []string{}
	brNames := []string{}
	for i, nic := range vm.Spec.NICs {
		if nic.MacAddress == "" {
			nic.MacAddress = generateMacAddress(vm.ID + nic.NetworkID)
		}

		n, err := a.client.CoreV0().Network().Get(vm.Group, vm.Namespace, nic.NetworkID)
		if err != nil {
			return err
		}
		net, err := a.getNodeNetwork(vm.Group, vm.Namespace, n.ID, a.nodeName)
		if err != nil {
			return err
		}

		if _, ok := net.Annotations["nodenetworkv0/bridge_name"]; !ok {
			return fmt.Errorf("network is not active")
		}
		tapName := utils.GenerateName("hum-vm-", net.Annotations["nodenetworkv0/bridge_name"]+vm.ID)
		tapName = fmt.Sprintf("%s-%02d", tapName[:len(tapName)-3], i)

		tapNames = append(tapNames, tapName)
		brNames = append(brNames, net.Annotations["nodenetworkv0/bridge_name"])

		nics = append(nics,
			"-device",
			fmt.Sprintf("virtio-net,netdev=netdev-%s,driver=virtio-net-pci,mac=%s,mq=on,rx_queue_size=1024,tx_queue_size=1024,vectors=%d",
				net.Annotations["nodenetworkv0/bridge_name"],
				nic.MacAddress,
				vcpusInt*2+2,
			),
			"-netdev",
			fmt.Sprintf("tap,script=no,downscript=no,id=netdev-%s,vhost=on,ifname=%s,queues=%d",
				net.Annotations["nodenetworkv0/bridge_name"],
				tapName,
				vcpusInt,
			),
		)
	}

	_, err = uuid.FromBytes([]byte(vm.Spec.UUID))
	if err != nil {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}

		vm.Spec.UUID = id.String()
	}

	metaData := cloudinit.MetaData{
		InstanceID:    vm.Spec.UUID,
		LocalHostName: vm.Name,
	}

	userDataUsers := []cloudinit.UserDataUser{}
	for _, user := range vm.Spec.LoginUsers {
		userDataUsers = append(userDataUsers, cloudinit.UserDataUser{
			Name:              user.Username,
			SSHAuthorizedKeys: user.SSHAuthorizedKeys,
			Groups:            "sudo",
			Shell:             "/bin/bash",
			Sudo: []string{
				"ALL=(ALL) NOPASSWD:ALL",
			},
		})
	}
	userData := cloudinit.UserData{
		Users: userDataUsers,
	}

	networkConfigConfigs := []cloudinit.NetworkConfigConfig{}
	for i, nic := range vm.Spec.NICs {
		// 上のやつと統合するべき
		n, err := a.client.CoreV0().Network().Get(vm.Group, vm.Namespace, nic.NetworkID)
		if err != nil {
			return err
		}
		_, ipnet, err := net.ParseCIDR(n.Spec.Template.Spec.IPv4CIDR)

		networkConfigConfigs = append(networkConfigConfigs, cloudinit.NetworkConfigConfig{
			Type:       cloudinit.NetworkConfigConfigTypePhysical,
			Name:       fmt.Sprintf("eth%d", i),
			MacAddress: nic.MacAddress,
			Subnets: []cloudinit.NetworkConfigConfigSubnet{
				{
					Type:    cloudinit.NetworkConfigConfigSubnetTypeStatic,
					Address: nic.IPv4Address,
					Netmask: fmt.Sprintf("%d.%d.%d.%d",
						ipnet.Mask[0],
						ipnet.Mask[1],
						ipnet.Mask[2],
						ipnet.Mask[3],
					),
					Nameservers: nic.Nameservers,
					Gateway:     nic.DefaultGateway,
				},
			},
		})
	}
	networkConfig := cloudinit.NetworkConfig{
		Version: 1,
		Config:  networkConfigConfigs,
	}

	ci := cloudinit.NewCloudInit(metaData, userData, networkConfig)
	ci.Output(filepath.Join("./virtualmachines", vm.Group, vm.Namespace))
	disks = append(disks,
		"-drive",
		fmt.Sprintf("file=./virtualmachines/%s/%s/%s/cloudinit.img,format=raw", vm.Group, vm.Namespace, vm.Spec.UUID),
	)

	displayNumber := int32(0)
	for ; displayNumber < 1000; displayNumber++ {
		if is, ok := a.vncDisplayMap[displayNumber]; ok && is {
			continue
		}

		a.vncDisplayMap[displayNumber] = true
		break
	}

	command := "qemu-system-x86_64"
	args := []string{
		"-enable-kvm",
		"-uuid",
		vm.Spec.UUID,
		"-name",
		fmt.Sprintf("guest=%s/%s,debug-threads=on", vm.Namespace, vm.ID),
		"-daemonize",
		"-nodefaults",
		"-vnc",
		// とりあえず6900以降をWebSocketに使う
		fmt.Sprintf("0.0.0.0:%d,websocket=%d", displayNumber, 6900+displayNumber),
		"-smp",
		fmt.Sprintf("%s,sockets=1,cores=%s,threads=1", vcpus, vcpus),
		"-cpu",
		"host",
		"-m",
		vm.Spec.LimitMemory,
		"-device",
		"VGA,id=video0,bus=pci.0",
	}

	args = append(args, disks...)
	args = append(args, nics...)

	cmd := exec.Command(command, args...)
	if _, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrap(err, fmt.Sprint(command, args))
	}

	for i, tapName := range tapNames {
		br, err := iproute2.NewBridge(brNames[i])
		if err != nil {
			return err
		}

		tap, err := iproute2.NewTap(tapName)
		if err != nil {
			return err
		}
		err = tap.Up()
		if err != nil {
			return err
		}

		err = tap.SetMaster(br)
		if err != nil {
			return err
		}
	}

	pid, err = getPID(vm.Spec.UUID)
	if err != nil {
		return errors.Wrap(err, "get pid")
	}

	node, err := a.client.SystemV0().Node().Get(a.nodeName)
	if err != nil {
		return errors.Wrap(err, "get node")
	}

	vm.Annotations["virtualmachinev0/pid"] = fmt.Sprint(pid)
	vm.Annotations["virtualmachinev0/vnc_display_number"] = fmt.Sprint(displayNumber)
	vm.Annotations["virtualmachinev0/vnc_websocket_host"] = fmt.Sprintf("%s:%d", node.Spec.Address, displayNumber+6900)
	vm.Status.State = system.VirtualMachineStateRunning
	return nil
}

func (a *VirtualMachineAgent) syncVirtualMachine(vm *system.VirtualMachine) error {
	if vm.DeleteState == meta.DeleteStateDelete {
		err := a.powerOffVirtualMachine(vm)
		if err != nil {
			return errors.Wrap(err, "poweroff vm")
		}

		path := fmt.Sprintf("./virtualmachines/%s/%s/%s/", vm.Group, vm.Namespace, vm.Spec.UUID)
		if err := os.RemoveAll(path); err != nil {
			return errors.Wrap(err, "delete cloudinit data")
		}

		err = a.client.SystemV0().VirtualMachine().Delete(vm.Group, vm.Namespace, vm.ID)
		if err != nil {
			return errors.Wrap(err, "delete client vm")
		}
		return nil
	}
	switch vm.Spec.ActionState {
	case system.VirtualMachineActionStatePowerOn:
		err := a.powerOnVirtualMachine(vm)
		if err != nil {
			return errors.Wrap(err, "poweron vm")
		}
	case system.VirtualMachineActionStatePowerOff:
		err := a.powerOffVirtualMachine(vm)
		if err != nil {
			return errors.Wrap(err, "poweroff vm")
		}
	}
	return setHash(vm)
}

func getPID(uuid string) (int64, error) {
	if uuid == "" {
		return -1, nil
	}

	command := "sh"
	args := []string{
		"-c",
		fmt.Sprintf(`ps aux | grep qemu | grep -v grep | grep '%s' | awk '{print $2}' | tr -d '\n'`, uuid),
	}

	cmd := exec.Command(command, args...)
	out, err := cmd.CombinedOutput()
	if string(out) == "" || err != nil {
		return -1, err
	}

	return strconv.ParseInt(string(out), 10, 64)
}

func (a *VirtualMachineAgent) getNodeNetwork(groupID, namespaceID, networkID, nodeID string) (*system.NodeNetwork, error) {
	nodeNetList, err := a.client.SystemV0().NodeNetwork().List(groupID, namespaceID)
	if err != nil {
		return nil, err
	}

	for _, nodeNet := range nodeNetList {
		isOwned := false
		for _, owner := range nodeNet.OwnerReferences {
			if owner.Meta.ID == networkID {
				isOwned = true
				break
			}
		}
		if isOwned {
			if node, ok := nodeNet.Annotations["nodenetworkv0/node_name"]; ok && node == nodeID {
				return nodeNet, nil
			}
		}
	}

	return nil, fmt.Errorf("NodeNetwork not found")
}

func setHash(vm *system.VirtualMachine) error {
	vm.ResourceHash = ""
	resourceJSON, err := json.Marshal(vm)
	if err != nil {
		return err
	}

	hash := md5.Sum(resourceJSON)
	vm.ResourceHash = fmt.Sprintf("%x", hash)
	return nil
}

const (
	UnitGigabyte = 'G'
	UnitMegabyte = 'M'
	UnitKilobyte = 'K'
	UnitMilli    = 'm'
)

func withUnitToWithoutUnit(numberWithUnit string) string {
	length := len(numberWithUnit)
	if numberWithUnit[length-1] >= '0' && numberWithUnit[length-1] <= '9' {
		return numberWithUnit
	}

	number, err := strconv.ParseInt(numberWithUnit[:length-1], 10, 64)
	if err != nil {
		return "0"
	}

	switch numberWithUnit[length-1] {
	case UnitGigabyte:
		return fmt.Sprintf("%d", number*1024*1024*1024)
	case UnitMegabyte:
		return fmt.Sprintf("%d", number*1024*1024)
	case UnitKilobyte:
		return fmt.Sprintf("%d", number*1024)
	case UnitMilli:
		return fmt.Sprintf("%d", number/1000)
	}
	return "0"
}

func generateMacAddress(id string) string {
	addr := "52:54"
	for i := 0; i < 4; i++ {
		addr = fmt.Sprintf("%s:%02x", addr, random(0, 255))
	}
	return addr
}

func random(min, max int) int {
	rand.Seed(time.Now().UnixNano())
	return rand.Intn(max-min) + min
}
