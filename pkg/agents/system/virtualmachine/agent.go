package virtualmachine

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/n0stack/n0stack/n0core/pkg/driver/iproute2"
	"github.com/ophum/humstack/pkg/agents/system/network/utils"
	"github.com/ophum/humstack/pkg/api/system"
	"github.com/ophum/humstack/pkg/client"
)

type VirtualMachineAgent struct {
	client *client.Clients
}

const (
	VirtualMachineV0AnnotationNodeName = "virtualmachinev0/node_name"
)

func NewVirtualMachineAgent(client *client.Clients) *VirtualMachineAgent {
	return &VirtualMachineAgent{
		client: client,
	}
}

func (a *VirtualMachineAgent) Run() {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	nodeName, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case <-ticker.C:
			nsList, err := a.client.CoreV0().Namespace().List()
			if err != nil {
				log.Println(err)
				continue
			}

			for _, ns := range nsList {
				vmList, err := a.client.SystemV0().VirtualMachine().List(ns.ID)
				if err != nil {
					log.Println(err)
					continue
				}

				for _, vm := range vmList {
					oldHash := vm.ResourceHash
					if vm.Annotations[VirtualMachineV0AnnotationNodeName] != nodeName {
						continue
					}
					err = a.syncVirtualMachine(vm)
					if err != nil {
						log.Println(err)
						continue
					}

					if vm.ResourceHash == oldHash {
						log.Printf("vm(`%s`) no update.\n", vm.ID)
						continue
					}

					_, err := a.client.SystemV0().VirtualMachine().Update(vm)
					if err != nil {
						log.Println(err)
						continue
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
		bs, err := a.client.SystemV0().BlockStorage().Get(vm.Namespace, bsID)
		if err != nil {
			return err
		}

		if bs.Status.State != system.BlockStorageStateActive {
			vm.Status.State = system.VirtualMachineStatePending
			return fmt.Errorf("BlockStorage is not active")
		}

		disks = append(disks,
			"-drive",
			fmt.Sprintf("file=./blockstorages/%s/%s,format=qcow2", vm.Namespace, bs.ID),
		)
	}

	nics := []string{}
	tapNames := []string{}
	brNames := []string{}
	for i, nic := range vm.Spec.NICs {
		if nic.MacAddress == "" {
			nic.MacAddress = generateMacAddress(vm.ID + nic.NetworkID)
		}

		net, err := a.client.SystemV0().Network().Get(vm.Namespace, nic.NetworkID)
		if err != nil {
			return err
		}

		if _, ok := net.Annotations["networkv0/bridge_name"]; !ok {
			return fmt.Errorf("network is not active")
		}
		tapName := utils.GenerateName("hum-vm-", net.Annotations["networkv0/bridge_name"])
		tapName = fmt.Sprintf("%s-%02d", tapName[:len(tapName)-3], i)

		tapNames = append(tapNames, tapName)
		brNames = append(brNames, net.Annotations["networkv0/bridge_name"])

		nics = append(nics,
			"-device",
			fmt.Sprintf("virtio-net,netdev=netdev-%s,driver=virtio-net-pci,mac=%s",
				net.Annotations["networkv0/bridge_name"],
				nic.MacAddress),
			"-netdev",
			fmt.Sprintf("tap,script=no,downscript=no,id=netdev-%s,vhost=on,ifname=%s",
				net.Annotations["networkv0/bridge_name"],
				tapName,
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

	vcpus := withUnitToWithoutUnit(vm.Spec.LimitVcpus)
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
		fmt.Sprintf("0.0.0.0:1"),
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

	log.Printf("create vm `%s`", vm.ID)
	log.Println(command, args)

	cmd := exec.Command(command, args...)
	if _, err := cmd.CombinedOutput(); err != nil {
		log.Println(err.Error())
		return err
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
		log.Println(err.Error())
		return err
	}
	vm.Annotations["virtualmachinev0/pid"] = fmt.Sprint(pid)
	vm.Status.State = system.VirtualMachineStateRunning
	return nil
}

func (a *VirtualMachineAgent) syncVirtualMachine(vm *system.VirtualMachine) error {
	switch vm.Spec.ActionState {
	case system.VirtualMachineActionStatePowerOn:
		err := a.powerOnVirtualMachine(vm)
		if err != nil {
			return err
		}
	case system.VirtualMachineActionStatePowerOff:
		err := a.powerOffVirtualMachine(vm)
		if err != nil {
			return err
		}
	}
	return setHash(vm)
}

func getPID(uuid string) (int64, error) {
	command := "sh"
	args := []string{
		"-c",
		fmt.Sprintf(`ps aux | grep qemu | grep -v grep | grep '%s' | awk '{print $2}' | tr -d '\n'`, uuid),
	}

	log.Println(command, args)
	cmd := exec.Command(command, args...)
	out, err := cmd.CombinedOutput()
	if string(out) == "" || err != nil {
		return -1, err
	}

	return strconv.ParseInt(string(out), 10, 64)
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
