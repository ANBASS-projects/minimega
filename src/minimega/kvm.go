// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"bridge"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	log "minilog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"qemu"
	"qmp"
	"ron"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"vnc"
)

const (
	DefaultKVMCPU                    = "host"
	DefaultKVMDriver                 = "e1000"
	DefaultKVMDiskInterface          = "ide"
	DefaultKVMDiskCacheSnapshotTrue  = "unsafe"
	DefaultKVMDiskCacheSnapshotFalse = "writeback"

	DEV_PER_BUS    = 32
	DEV_PER_VIRTIO = 30 // Max of 30 virtio ports/device (0 and 32 are reserved)

	QMP_CONNECT_RETRY = 50
	QMP_CONNECT_DELAY = 100
)

type KVMConfig struct {
	// Set the QEMU binary name to invoke. Relative paths are ok.
	//
	// Note: this configuration only applies to KVM-based VMs.
	//
	// Default: "kvm"
	QemuPath string

	// Attach a kernel image to a VM. If set, QEMU will boot from this image
	// instead of any disk image.
	//
	// Note: this configuration only applies to KVM-based VMs.
	KernelPath string

	// Attach an initrd image to a VM. Passed along with the kernel image at
	// boot time.
	//
	// Note: this configuration only applies to KVM-based VMs.
	InitrdPath string

	// Attach a cdrom to a VM. When using a cdrom, it will automatically be set
	// to be the boot device.
	//
	// Note: this configuration only applies to KVM-based VMs.
	CdromPath string

	// Assign a migration image, generated by a previously saved VM to boot
	// with. By default, images are read from the files directory as specified
	// with -filepath. This can be overriden by using an absolute path.
	// Migration images should be booted with a kernel/initrd, disk, or cdrom.
	// Use 'vm migrate' to generate migration images from running VMs.
	//
	// Note: this configuration only applies to KVM-based VMs.
	MigratePath string

	// Set the virtual CPU architecture.
	//
	// By default, set to 'host' which matches the host CPU. See 'qemu -cpu
	// help' for a list of supported CPUs.
	//
	// The accepted values for this configuration depend on the QEMU binary
	// name specified by 'vm config qemu'.
	//
	// Note: this configuration only applies to KVM-based VMs.
	//
	// Default: "host"
	CPU string `validate:"validCPU" suggest:"wrapSuggest(suggestCPU)"`

	// Set the number of CPU sockets. If unspecified, QEMU will calculate
	// missing values based on vCPUs, cores, and threads.
	Sockets uint64

	// Set the number of CPU cores per socket. If unspecified, QEMU will
	// calculate missing values based on vCPUs, sockets, and threads.
	Cores uint64

	// Set the number of CPU threads per core. If unspecified, QEMU will
	// calculate missing values based on vCPUs, sockets, and cores.
	Threads uint64

	// Specify the machine type. See 'qemu -M help' for a list supported
	// machine types.
	//
	// The accepted values for this configuration depend on the QEMU binary
	// name specified by 'vm config qemu'.
	//
	// Note: this configuration only applies to KVM-based VMs.
	Machine string `validate:"validMachine" suggest:"wrapSuggest(suggestMachine)"`

	// Specify the serial ports that will be created for the VM to use. Serial
	// ports specified will be mapped to the VM's /dev/ttySX device, where X
	// refers to the connected unix socket on the host at
	// $minimega_runtime/<vm_id>/serialX.
	//
	// Examples:
	//
	// To display current serial ports:
	//   vm config serial-ports
	//
	// To create three serial ports:
	//   vm config serial-ports 3
	//
	// Note: Whereas modern versions of Windows support up to 256 COM ports,
	// Linux typically only supports up to four serial devices. To use more,
	// make sure to pass "8250.n_uarts = 4" to the guest Linux kernel at boot.
	// Replace 4 with another number.
	SerialPorts uint64

	// Specify the virtio-serial ports that will be created for the VM to use.
	// Virtio-serial ports specified will be mapped to the VM's
	// /dev/virtio-port/<portname> device, where <portname> refers to the
	// connected unix socket on the host at
	// $minimega_runtime/<vm_id>/virtio-serialX.
	//
	// Examples:
	//
	// To display current virtio-serial ports:
	//   vm config virtio-ports
	//
	// To create three virtio-serial ports:
	//   vm config virtio-ports 3
	//
	// To explicitly name the virtio-ports, pass a comma-separated list of names:
	//
	//   vm config virtio-ports foo,bar
	//
	// The ports (on the guest) will then be mapped to /dev/virtio-port/foo and
	// /dev/virtio-port/bar.
	VirtioPorts string

	// Specify the graphics card to emulate. "cirrus" or "std" should work with
	// most operating systems.
	//
	// Default: "std"
	Vga string

	// Add an append string to a kernel set with vm kernel. Setting vm append
	// without using vm kernel will result in an error.
	//
	// For example, to set a static IP for a linux VM:
	//
	// 	vm config append ip=10.0.0.5 gateway=10.0.0.1 netmask=255.255.255.0 dns=10.10.10.10
	//
	// Note: this configuration only applies to KVM-based VMs.
	Append []string

	// Attach one or more disks to a vm. Any disk image supported by QEMU is a
	// valid parameter. Disk images launched in snapshot mode may safely be
	// used for multiple VMs since minimega snapshots the disk image when the
	// VM launches, creating a back qcow2 in the VM's instance directory.
	//
	// Note: this configuration only applies to KVM-based VMs.
	Disks DiskConfigs

	// Add additional arguments to be passed to the QEMU instance. For example:
	//
	// 	vm config qemu-append -serial tcp:localhost:4001
	//
	// Note: this configuration only applies to KVM-based VMs.
	QemuAppend []string

	// QemuOverride for the VM, handler is not generated by vmconfiger.
	QemuOverride QemuOverrides

	// hugepagesMountPath is copied from ns.hugepagesMountPath when the VM is
	// launched. Not set by "vm config" APIs.
	hugepagesMountPath string
}

type qemuOverride struct {
	Match string
	Repl  string
}

type QemuOverrides []qemuOverride

type vmHotplug struct {
	Disk    string
	Version string
}

type KvmVM struct {
	*BaseVM   // embed
	KVMConfig // embed

	// Internal variables
	hotplug map[int]vmHotplug

	q qmp.Conn // qmp connection for this vm

	vncShim net.Listener // shim for VNC connections
	VNCPort int
}

// Ensure that KvmVM implements the VM interface
var _ VM = (*KvmVM)(nil)

// Copy makes a deep copy and returns reference to the new struct.
func (old KVMConfig) Copy() KVMConfig {
	// Copy all fields
	res := old

	// Make deep copy of slices
	res.Disks = make([]DiskConfig, len(old.Disks))
	copy(res.Disks, old.Disks)
	res.QemuAppend = make([]string, len(old.QemuAppend))
	copy(res.QemuAppend, old.QemuAppend)

	return res
}

func NewKVM(name, namespace string, config VMConfig) (*KvmVM, error) {
	vm := new(KvmVM)

	vm.BaseVM = NewBaseVM(name, namespace, config)
	vm.Type = KVM

	vm.KVMConfig = config.KVMConfig.Copy() // deep-copy configured fields

	vm.hotplug = make(map[int]vmHotplug)

	return vm, nil
}

func (vm *KvmVM) Copy() VM {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	vm2 := new(KvmVM)

	// Make shallow copies of all fields
	*vm2 = *vm

	// Make deep copies
	vm2.BaseVM = vm.BaseVM.copy()
	vm2.KVMConfig = vm.KVMConfig.Copy()

	vm2.hotplug = make(map[int]vmHotplug)
	for k, v := range vm.hotplug {
		vm2.hotplug[k] = v
	}

	return vm2
}

// Launch a new KVM VM.
func (vm *KvmVM) Launch() error {
	defer vm.lock.Unlock()

	return vm.launch()
}

// Flush cleans up all resources allocated to the VM which includes all the
// network taps.
func (vm *KvmVM) Flush() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	for _, net := range vm.Networks {
		// Handle already disconnected taps differently since they aren't
		// assigned to any bridges.
		if net.VLAN == DisconnectedVLAN {
			if err := bridge.DestroyTap(net.Tap); err != nil {
				log.Error("leaked tap %v: %v", net.Tap, err)
			}

			continue
		}

		br, err := getBridge(net.Bridge)
		if err != nil {
			return err
		}

		if err := br.DestroyTap(net.Tap); err != nil {
			log.Error("leaked tap %v: %v", net.Tap, err)
		}
	}

	return vm.BaseVM.Flush()
}

func (vm *KvmVM) Config() *BaseConfig {
	return &vm.BaseConfig
}

func (vm *KvmVM) Start() (err error) {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.State&VM_RUNNING != 0 {
		return nil
	}

	if vm.State == VM_QUIT || vm.State == VM_ERROR {
		log.Info("relaunching VM: %v", vm.ID)

		// Create a new channel since we closed the other one to indicate that
		// the VM should quit.
		vm.kill = make(chan bool)

		// Launch handles setting the VM to error state
		if err := vm.launch(); err != nil {
			return err
		}
	}

	log.Info("starting VM: %v", vm.ID)
	if err := vm.q.Start(); err != nil {
		return vm.setErrorf("unable to start: %v", err)
	}

	vm.setState(VM_RUNNING)

	return nil
}

func (vm *KvmVM) Stop() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.Name == "vince" {
		return errors.New("vince is unstoppable")
	}

	if vm.State != VM_RUNNING {
		return vmNotRunning(strconv.Itoa(vm.ID))
	}

	log.Info("stopping VM: %v", vm.ID)
	if err := vm.q.Stop(); err != nil {
		return vm.setErrorf("unstoppable: %v", vm.ID)
	}

	vm.setState(VM_PAUSED)

	return nil
}

func (vm *KvmVM) String() string {
	return fmt.Sprintf("%s:%d:kvm", hostname, vm.ID)
}

func (vm *KvmVM) Info(field string) (string, error) {
	// If the field is handled by BaseVM, return it
	if v, err := vm.BaseVM.Info(field); err == nil {
		return v, nil
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	switch field {
	case "vnc_port":
		return strconv.Itoa(vm.VNCPort), nil
	case "pid":
		return strconv.Itoa(vm.Pid), nil
	}

	return vm.KVMConfig.Info(field)
}

func (vm *KvmVM) Conflicts(vm2 VM) error {
	switch vm2 := vm2.(type) {
	case *KvmVM:
		return vm.ConflictsKVM(vm2)
	case *ContainerVM:
		return vm.BaseVM.conflicts(vm2.BaseVM)
	}

	return errors.New("unknown VM type")
}

// ConflictsKVM tests whether vm and vm2 share a disk and returns an
// error if one of them is not running in snapshot mode. Also checks
// whether the BaseVMs conflict.
func (vm *KvmVM) ConflictsKVM(vm2 *KvmVM) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	for _, d := range vm.Disks {
		for _, d2 := range vm2.Disks {
			if d.Path == d2.Path && (!vm.Snapshot || !vm2.Snapshot) {
				return fmt.Errorf("disk conflict with vm %v: %v", vm.Name, d)
			}
		}
	}

	return vm.BaseVM.conflicts(vm2.BaseVM)
}

func (vm *KVMConfig) String() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "KVM configuration:")
	fmt.Fprintf(w, "Migrate Path:\t%v\n", vm.MigratePath)
	fmt.Fprintf(w, "Disks:\t%v\n", vm.DiskString(namespace))
	fmt.Fprintf(w, "CDROM Path:\t%v\n", vm.CdromPath)
	fmt.Fprintf(w, "Kernel Path:\t%v\n", vm.KernelPath)
	fmt.Fprintf(w, "Initrd Path:\t%v\n", vm.InitrdPath)
	fmt.Fprintf(w, "Kernel Append:\t%v\n", vm.Append)
	fmt.Fprintf(w, "QEMU Path:\t%v\n", vm.QemuPath)
	fmt.Fprintf(w, "QEMU Append:\t%v\n", vm.QemuAppend)
	fmt.Fprintf(w, "Serial Ports:\t%v\n", vm.SerialPorts)
	fmt.Fprintf(w, "Virtio-Serial Ports:\t%v\n", vm.VirtioPorts)
	fmt.Fprintf(w, "Machine:\t%v\n", vm.Machine)
	fmt.Fprintf(w, "CPU:\t%v\n", vm.CPU)
	fmt.Fprintf(w, "Cores:\t%v\n", vm.Cores)
	fmt.Fprintf(w, "Threads:\t%v\n", vm.Threads)
	fmt.Fprintf(w, "Sockets:\t%v\n", vm.Sockets)
	fmt.Fprintf(w, "VGA:\t%v\n", vm.Vga)
	w.Flush()
	fmt.Fprintln(&o)
	return o.String()
}

func (vm *KVMConfig) DiskString(namespace string) string {
	return fmt.Sprintf("[%s]", vm.Disks.String())
}

func (vm *KvmVM) QMPRaw(input string) (string, error) {
	return vm.q.Raw(input)
}

func (vm *KvmVM) Migrate(filename string) error {
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(*f_iomBase, filename)
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	// migrating the VM will pause it
	vm.setState(VM_PAUSED)

	return vm.q.MigrateDisk(filename)
}

func (vm *KvmVM) QueryMigrate() (string, float64, error) {
	var status string
	var completed float64

	r, err := vm.q.QueryMigrate()
	if err != nil {
		return "", 0.0, err
	}

	// find the status
	if s, ok := r["status"]; ok {
		status = s.(string)
	} else {
		// if there is no status, it means that there is no active migration
		return "", 0.0, nil
	}

	var ram map[string]interface{}
	switch status {
	case "completed":
		completed = 100.0
		return status, completed, nil
	case "failed":
		return status, completed, nil
	case "active":
		if e, ok := r["ram"]; !ok {
			return status, completed, fmt.Errorf("could not decode ram segment: %v", e)
		} else {
			switch e.(type) {
			case map[string]interface{}:
				ram = e.(map[string]interface{})
			default:
				return status, completed, fmt.Errorf("invalid ram type: %v", e)
			}
		}
	}

	total := ram["total"].(float64)
	transferred := ram["transferred"].(float64)

	if total == 0.0 {
		return status, completed, fmt.Errorf("zero total ram!")
	}

	completed = transferred / total * 100

	return status, completed, nil
}

func (vm *KvmVM) Screenshot(size int) ([]byte, error) {
	if vm.GetState()&VM_RUNNING == 0 {
		return nil, vmNotRunning(strconv.Itoa(vm.ID))
	}

	suffix := rand.New(rand.NewSource(time.Now().UnixNano())).Int31()
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("minimega_screenshot_%v", suffix))

	// We have to write this out to a file, because QMP
	err := vm.q.Screendump(tmp)
	if err != nil {
		return nil, err
	}

	ppmFile, err := ioutil.ReadFile(tmp)
	if err != nil {
		return nil, err
	}

	pngResult, err := ppmToPng(ppmFile, size)
	if err != nil {
		return nil, err
	}

	err = os.Remove(tmp)
	if err != nil {
		return nil, err
	}

	return pngResult, nil
}

func (vm *KvmVM) connectQMP() (err error) {
	delay := QMP_CONNECT_DELAY * time.Millisecond

	for count := 0; count < QMP_CONNECT_RETRY; count++ {
		vm.q, err = qmp.Dial(vm.path("qmp"))
		if err == nil {
			log.Debug("qmp dial to %v successful", vm.ID)
			return
		}

		log.Debug("qmp dial to %v : %v, redialing in %v", vm.ID, err, delay)
		time.Sleep(delay)
	}

	// Never connected successfully
	return errors.New("qmp timeout")
}

func (vm *KvmVM) connectVNC() error {
	l, err := net.Listen("tcp", "")
	if err != nil {
		return err
	}

	// Keep track of shim so that we can close it later
	vm.vncShim = l
	vm.VNCPort = l.Addr().(*net.TCPAddr).Port

	go func() {
		defer l.Close()

		// should never create...
		ns := GetOrCreateNamespace(vm.Namespace)

		for {
			// Sit waiting for new connections
			remote, err := l.Accept()
			if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
				return
			} else if err != nil {
				log.Errorln(err)
				return
			}

			log.Info("vnc shim connect: %v -> %v", remote.RemoteAddr(), vm.Name)

			go func() {
				defer remote.Close()

				// Dial domain socket
				local, err := net.Dial("unix", vm.path("vnc"))
				if err != nil {
					log.Error("unable to dial vm vnc: %v", err)
					return
				}
				defer local.Close()

				// copy local -> remote
				go io.Copy(remote, local)

				// Reads will implicitly copy from remote -> local
				tee := io.TeeReader(remote, local)
				for {
					msg, err := vnc.ReadClientMessage(tee)
					if err == nil {
						ns.Recorder.Route(vm.GetName(), msg)
						continue
					}

					// shim is no longer connected
					if err == io.EOF || strings.Contains(err.Error(), "broken pipe") {
						log.Info("vnc shim quit: %v", vm.Name)
						break
					}

					// ignore these
					if strings.Contains(err.Error(), "unknown client-to-server message") {
						continue
					}

					// unknown error
					log.Warnln(err)
				}
			}()
		}
	}()

	return nil
}

// launch is the low-level launch function for KVM VMs. The caller should hold
// the VM's lock.
func (vm *KvmVM) launch() error {
	log.Info("launching vm: %v", vm.ID)

	// If this is the first time launching the VM, do the final configuration
	// check and create directories for it.
	if vm.State == VM_BUILDING {
		// create a directory for the VM at the instance path
		if err := os.MkdirAll(vm.instancePath, os.FileMode(0700)); err != nil {
			return vm.setErrorf("unable to create VM dir: %v", err)
		}

		// Create a snapshot of each disk image
		if vm.Snapshot {
			for i, d := range vm.Disks {
				dst := vm.path(fmt.Sprintf("disk-%v.qcow2", i))
				if err := diskSnapshot(d.Path, dst); err != nil {
					return vm.setErrorf("unable to snapshot %v: %v", d, err)
				}

				vm.Disks[i].SnapshotPath = dst
			}
		}

		if err := vm.createInstancePathAlias(); err != nil {
			return vm.setErrorf("createInstancePathAlias: %v", err)
		}
	}

	mustWrite(vm.path("name"), vm.Name)

	// create and add taps if we are associated with any networks
	for i := range vm.Networks {
		nic := &vm.Networks[i]
		if nic.Tap != "" {
			// tap has already been created, don't need to do again
			continue
		}

		br, err := getBridge(nic.Bridge)
		if err != nil {
			return vm.setErrorf("unable to get bridge %v: %v", nic.Bridge, err)
		}

		tap, err := br.CreateTap(nic.MAC, nic.VLAN)
		if err != nil {
			return vm.setErrorf("unable to create tap %v: %v", i, err)
		}

		nic.Tap = tap
	}

	if len(vm.Networks) > 0 {
		if err := vm.writeTaps(); err != nil {
			return vm.setErrorf("unable to write taps: %v", err)
		}
	}

	var sOut bytes.Buffer
	var sErr bytes.Buffer

	vmConfig := VMConfig{BaseConfig: vm.BaseConfig, KVMConfig: vm.KVMConfig}
	args := vmConfig.qemuArgs(vm.ID, vm.instancePath)
	args = vmConfig.applyQemuOverrides(args)
	log.Debug("final qemu args: %#v", args)

	// if the QemuPath is not absolute, try a lookup based on $PATH
	qemu := vm.QemuPath
	if !filepath.IsAbs(qemu) {
		v, err := process(qemu)
		if err != nil {
			return vm.setErrorf("unable to launch VM: %v", err)
		}
		qemu = v
	}

	cmd := &exec.Cmd{
		Path:   qemu,
		Args:   append([]string{qemu}, args...),
		Stdout: &sOut,
		Stderr: &sErr,
	}

	if err := cmd.Start(); err != nil {
		return vm.setErrorf("unable to start qemu: %v %v", err, sErr.String())
	}

	vm.Pid = cmd.Process.Pid
	log.Debug("vm %v has pid %v", vm.ID, vm.Pid)

	// Channel to signal when the process has exited
	var waitChan = make(chan bool)

	// Create goroutine to wait for process to exit
	go func() {
		defer close(waitChan)
		err := cmd.Wait()

		vm.lock.Lock()
		defer vm.lock.Unlock()

		// Check if the process quit for some reason other than being killed
		if err != nil && err.Error() != "signal: killed" {
			vm.setErrorf("qemu killed: %v %v", err, sErr.String())
		} else if vm.State != VM_ERROR {
			// Set to QUIT unless we've already been put into the error state
			vm.setState(VM_QUIT)
		}

		// Kill the VNC shim, if it exists
		if vm.vncShim != nil {
			vm.vncShim.Close()
		}
	}()

	if err := vm.connectQMP(); err != nil {
		// Failed to connect to qmp so clean up the process
		cmd.Process.Kill()

		return vm.setErrorf("unable to connect to qmp socket: %v", err)
	}

	go qmpLogger(vm.ID, vm.q)

	if err := vm.connectVNC(); err != nil {
		// Failed to connect to vnc so clean up the process
		cmd.Process.Kill()

		return vm.setErrorf("unable to connect to vnc shim: %v", err)
	}

	// Create goroutine to wait to kill the VM
	go func() {
		defer vm.cond.Signal()

		select {
		case <-waitChan:
			log.Info("VM %v exited", vm.ID)
		case <-vm.kill:
			log.Info("Killing VM %v", vm.ID)
			cmd.Process.Kill()
			<-waitChan
		}
	}()

	return nil
}

func (vm *KvmVM) Connect(cc *ron.Server, reconnect bool) error {
	if !vm.Backchannel {
		return nil
	}

	if !reconnect {
		cc.RegisterVM(vm)
	}

	return cc.DialSerial(vm.path("cc"))
}

func (vm *KvmVM) Disconnect(cc *ron.Server) error {
	if !vm.Backchannel {
		return nil
	}

	cc.UnregisterVM(vm)

	return nil
}

func (vm *KvmVM) AddNIC() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	// We just added this network, so it should be the last one in the list
	pos := len(vm.Networks) - 1
	if pos < 0 {
		// weird...
		return fmt.Errorf("Missing network interface to add...")
	}
	log.Info("Creating network at pos %v", pos)
	nic := &vm.Networks[pos]
	if nic.MAC == "" {
		nic.MAC = randomMac()
	}
	//vm.UpdateNetworks()

	// generate an id by adding 2 to the highest in the list for the
	// nic devices, 0 if it's empty - we add two because we need to
	// generate 2 devices for every nic, a net device (tap) for the
	// nic and the nic itself
	id := 0
	for k := range vm.Networks {
		id = k + 2
	}
	ndid := fmt.Sprintf("nd%v", id)
	tapid := fmt.Sprintf("tap%v", id)
	r, err := vm.q.NetDevAdd(ndid, "tap", tapid)
	if err != nil {
		return err
	}
	log.Debugln("Add Nic: NetDevAdd QMP response:", r)

	nicid := fmt.Sprintf("nic%v", id+1)
	r, err = vm.q.NicAdd(nicid, ndid, "pci.0", nic.Driver, nic.MAC)
	if err != nil {
		return err
	}
	log.Debugln("Add Nic: NicAdd QMP response:", r)

	return nil
}

func (vm *KvmVM) Hotplug(f, version, serial string) error {
	var bus string
	switch version {
	case "", "1.1":
		version = "1.1"
		bus = "usb-bus.0"
	case "2.0":
		bus = "ehci.0"
	default:
		return fmt.Errorf("invalid version: `%v`", version)
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	// generate an id by adding 1 to the highest in the list for the
	// hotplug devices, 0 if it's empty
	id := 0
	for k := range vm.hotplug {
		if k >= id {
			id = k + 1
		}
	}

	hid := fmt.Sprintf("hotplug%v", id)
	log.Debugln("hotplug generated id:", hid)

	r, err := vm.q.DriveAdd(hid, f)
	if err != nil {
		return err
	}
	log.Debugln("hotplug drive_add response:", r)

	r, err = vm.q.USBDeviceAdd(hid, bus, serial)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb device add response:", r)
	vm.hotplug[id] = vmHotplug{f, version}

	return nil
}

func (vm *KvmVM) HotplugRemoveAll() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if len(vm.hotplug) == 0 {
		return errors.New("no hotplug devices to remove")
	}

	for k := range vm.hotplug {
		if err := vm.hotplugRemove(k); err != nil {
			return err
		}
	}

	return nil
}

func (vm *KvmVM) HotplugRemove(id int) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	return vm.hotplugRemove(id)
}

func (vm *KvmVM) hotplugRemove(id int) error {
	hid := fmt.Sprintf("hotplug%v", id)
	log.Debugln("hotplug id:", hid)
	if _, ok := vm.hotplug[id]; !ok {
		return errors.New("no such hotplug device")
	}

	resp, err := vm.q.USBDeviceDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb device del response:", resp)
	resp, err = vm.q.DriveDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb drive del response:", resp)
	delete(vm.hotplug, id)
	return nil
}

// HotplugInfo returns a deep copy of the VM's hotplug info
func (vm *KvmVM) HotplugInfo() map[int]vmHotplug {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	res := map[int]vmHotplug{}

	for k, v := range vm.hotplug {
		res[k] = vmHotplug{v.Disk, v.Version}
	}

	return res
}

func (vm *KvmVM) ChangeCD(f string, force bool) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.CdromPath != "" {
		if err := vm.ejectCD(force); err != nil {
			return err
		}
	}

	err := vm.q.BlockdevChange("ide0-cd0", f)
	if err == nil {
		vm.CdromPath = f
	}

	return err
}

func (vm *KvmVM) EjectCD(force bool) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.CdromPath == "" {
		return errors.New("no cdrom inserted")
	}

	return vm.ejectCD(force)
}

func (vm *KvmVM) ejectCD(force bool) error {
	err := vm.q.BlockdevEject("ide0-cd0", force)
	if err == nil {
		vm.CdromPath = ""
	}

	return err
}

func (vm *KvmVM) ProcStats() (map[int]*ProcStats, error) {
	p, err := GetProcStats(vm.Pid)
	if err != nil {
		return nil, err
	}

	return map[int]*ProcStats{vm.Pid: p}, nil
}

func (vm *KvmVM) WriteConfig(w io.Writer) error {
	if err := vm.BaseConfig.WriteConfig(w); err != nil {
		return err
	}

	return vm.KVMConfig.WriteConfig(w)
}

// qemuArgs build the horribly long qemu argument string
//
// Note: it would be cleaner if this was a method for KvmVM rather than
// VMConfig but we want to be able to show the qemu arg string before and after
// overrides in the `vm config qemu-override` API. We cannot use KVMConfig as
// the receiver either because we need to look at fields from the BaseConfig to
// build the qemu args.
func (vm VMConfig) qemuArgs(id int, vmPath string) []string {
	var args []string

	args = append(args, "-name")
	args = append(args, strconv.Itoa(id))

	if vm.Machine != "" {
		args = append(args, "-M", vm.Machine)
	}

	args = append(args, "-m")
	args = append(args, strconv.FormatUint(vm.Memory, 10))

	args = append(args, "-nographic")

	args = append(args, "-vnc")
	args = append(args, "unix:"+filepath.Join(vmPath, "vnc"))

	args = append(args, "-smp")
	smp := strconv.FormatUint(vm.VCPUs, 10)
	if vm.Cores != 0 {
		smp += ",cores=" + strconv.FormatUint(vm.Cores, 10)
	}
	if vm.Threads != 0 {
		smp += ",threads=" + strconv.FormatUint(vm.Threads, 10)
	}
	if vm.Sockets != 0 {
		smp += ",sockets=" + strconv.FormatUint(vm.Sockets, 10)
	}
	args = append(args, smp)

	args = append(args, "-qmp")
	args = append(args, "unix:"+filepath.Join(vmPath, "qmp")+",server")

	args = append(args, "-vga")
	if vm.Vga == "" {
		args = append(args, "std")
	} else {
		args = append(args, vm.Vga)
	}

	args = append(args, "-rtc")
	args = append(args, "clock=vm,base=utc")

	// for USB 1.0, creates bus named usb-bus.0
	args = append(args, "-usb")
	// for USB 2.0, creates bus named ehci.0
	args = append(args, "-device", "usb-ehci,id=ehci")
	// this allows absolute pointers in vnc, and works great on android vms
	args = append(args, "-device", "usb-tablet,bus=usb-bus.0")

	// this is non-virtio serial ports
	// for virtio-serial, look below near the net code
	for i := uint64(0); i < vm.SerialPorts; i++ {
		args = append(args, "-chardev")
		args = append(args, fmt.Sprintf("socket,id=charserial%v,path=%v%v,server,nowait", i, filepath.Join(vmPath, "serial"), i))

		args = append(args, "-device")
		args = append(args, fmt.Sprintf("isa-serial,chardev=charserial%v,id=serial%v", i, i))
	}

	args = append(args, "-pidfile")
	args = append(args, filepath.Join(vmPath, "qemu.pid"))

	args = append(args, "-k")
	args = append(args, "en-us")

	if vm.CPU != "" {
		args = append(args, "-cpu")
		args = append(args, vm.CPU)
	}

	args = append(args, "-net")
	args = append(args, "none")

	args = append(args, "-S")

	if vm.MigratePath != "" {
		args = append(args, "-incoming")
		args = append(args, fmt.Sprintf("exec:cat %v", vm.MigratePath))
	}

	// put cdrom *before* disks so that it is always connected to ide0 -- this
	// allows us to use a hardcoded block device name in cdrom eject/change.
	if vm.CdromPath != "" {
		args = append(args, "-drive")
		args = append(args, "file="+vm.CdromPath+",media=cdrom")
		args = append(args, "-boot")
		args = append(args, "once=d")
	} else {
		// add an empty cdrom
		args = append(args, "-drive")
		args = append(args, "media=cdrom")
	}

	// disks
	var ahciBusSlot int

	for _, diskConfig := range vm.Disks {
		var driveParams string

		path := diskConfig.Path
		if vm.Snapshot && diskConfig.SnapshotPath != "" {
			path = diskConfig.SnapshotPath
		}

		if diskConfig.Interface == "ahci" {
			if ahciBusSlot == 0 {
				args = append(args, "-device")
				args = append(args, "ahci,id=ahci")
			}

			args = append(args, "-device")
			args = append(args, fmt.Sprintf("ide-drive,drive=ahci-drive-%v,bus=ahci.%v", ahciBusSlot, ahciBusSlot))

			driveParams = fmt.Sprintf("id=ahci-drive-%v,file=%v,media=disk,if=none", ahciBusSlot, path)

			ahciBusSlot++
		} else if diskConfig.Interface != "" {
			driveParams = fmt.Sprintf("file=%v,media=disk,if=%v", path, diskConfig.Interface)
		} else {
			driveParams = fmt.Sprintf("file=%v,media=disk,if=%v", path, DefaultKVMDiskInterface)
		}

		if diskConfig.Cache != "" {
			driveParams = fmt.Sprintf("%v,cache=%v", driveParams, diskConfig.Cache)
		} else {
			if vm.Snapshot {
				driveParams = fmt.Sprintf("%v,cache=%v", driveParams, DefaultKVMDiskCacheSnapshotTrue)
			} else {
				driveParams = fmt.Sprintf("%v,cache=%v", driveParams, DefaultKVMDiskCacheSnapshotFalse)
			}
		}

		args = append(args, "-drive")
		args = append(args, driveParams)
	}

	if vm.KernelPath != "" {
		args = append(args, "-kernel")
		args = append(args, vm.KernelPath)
	}
	if vm.InitrdPath != "" {
		args = append(args, "-initrd")
		args = append(args, vm.InitrdPath)
	}
	if len(vm.Append) > 0 {
		args = append(args, "-append")
		args = append(args, unescapeString(vm.Append))
	}

	// net
	var bus, addr int
	addBus := func() {
		addr = 1 // start at 1 because 0 is reserved
		bus++
		args = append(args, fmt.Sprintf("-device"))
		args = append(args, fmt.Sprintf("pci-bridge,id=pci.%v,chassis_nr=%v", bus, bus))
	}

	addBus()
	for _, net := range vm.Networks {
		args = append(args, "-netdev")
		args = append(args, fmt.Sprintf("tap,id=%v,script=no,ifname=%v", net.Tap, net.Tap))
		args = append(args, "-device")
		args = append(args, fmt.Sprintf("driver=%v,netdev=%v,mac=%v,bus=pci.%v,addr=0x%x", net.Driver, net.Tap, net.MAC, bus, addr))
		addr++
		if addr == DEV_PER_BUS {
			addBus()
		}
	}

	// start at -1 so that the first time we call addVirtioDevice we create port 0
	virtioPort := -1

	addVirtioDevice := func() {
		virtioPort++

		args = append(args, "-device")
		args = append(args, fmt.Sprintf("virtio-serial-pci,id=virtio-serial%v,bus=pci.%v,addr=0x%x", virtioPort, bus, addr))

		addr++
		if addr == DEV_PER_BUS { // check to see if we've run out of addr slots on this bus
			addBus()
		}
	}

	// virtio-serial
	if vm.Backchannel {
		addVirtioDevice()

		args = append(args, "-chardev")
		args = append(args, fmt.Sprintf("socket,id=charvserialCC,path=%v,server,nowait", filepath.Join(vmPath, "cc")))
		args = append(args, "-device")
		args = append(args, fmt.Sprintf("virtserialport,bus=virtio-serial%v.0,chardev=charvserialCC,id=charvserialCC,name=cc", virtioPort))
	}

	if vm.VirtioPorts != "" {
		names := []string{}

		v, err := strconv.ParseUint(vm.VirtioPorts, 10, 64)
		if err == nil {
			// if the VirtioPorts is an int, assume they want automatically generated names
			for i := uint64(0); i < v; i++ {
				names = append(names, "virtio-serial"+strconv.FormatUint(i, 10))
			}
		} else {
			// otherwise, assume they specified a list of names
			names = strings.Split(vm.VirtioPorts, ",")
		}

		for i, name := range names {
			if name == "cc" && vm.Backchannel {
				// TODO: abort?
				log.Warn("virtio-port name conflicts with miniccc's")
			}

			// If we've maxed out the device, create a new one
			if i%DEV_PER_VIRTIO == 0 {
				addVirtioDevice()
			}

			args = append(args, "-chardev")
			args = append(args, fmt.Sprintf("socket,id=charvserial%v,path=%v%v,server,nowait", i, filepath.Join(vmPath, "virtio-serial"), i))
			args = append(args, "-device")
			args = append(args, fmt.Sprintf("virtserialport,bus=virtio-serial%v.0,chardev=charvserial%v,id=charvserial%v,name=%v", virtioPort, i, i, name))
		}
	}

	// hook for hugepage support
	if vm.hugepagesMountPath != "" {
		args = append(args, "-mem-info")
		args = append(args, vm.hugepagesMountPath)
	}

	if len(vm.QemuAppend) > 0 {
		args = append(args, vm.QemuAppend...)
	}

	args = append(args, "-uuid")
	args = append(args, vm.UUID)

	log.Debug("args for vm %v are: %#v", id, args)
	return args
}

func (vm VMConfig) qemuOverrideString() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "id\tmatch\treplacement")
	for i, v := range vm.QemuOverride {
		fmt.Fprintf(&o, "%v\t\"%v\"\t\"%v\"\n", i, v.Match, v.Repl)
	}
	w.Flush()

	args := vm.qemuArgs(0, "") // ID and path don't matter -- just testing
	preArgs := unescapeString(args)
	postArgs := unescapeString(vm.applyQemuOverrides(args))

	r := o.String()
	r += fmt.Sprintf("\nBefore overrides:\n%v\n", preArgs)
	r += fmt.Sprintf("\nAfter overrides:\n%v\n", postArgs)

	return r
}

func (vm VMConfig) applyQemuOverrides(args []string) []string {
	ret := unescapeString(args)
	for _, v := range vm.QemuOverride {
		ret = strings.Replace(ret, v.Match, v.Repl, -1)
	}
	return fieldsQuoteEscape("\"", ret)
}

func (c QemuOverrides) WriteConfig(w io.Writer) error {
	for k, v := range c {
		if _, err := fmt.Fprintf(w, "vm config qemu-override %v %v\n", k, v); err != nil {
			return err
		}
	}

	return nil
}

// log any asynchronous messages, such as vnc connects, to log.Info
func qmpLogger(id int, q qmp.Conn) {
	for v := q.Message(); v != nil; v = q.Message() {
		log.Info("VM %v received asynchronous message: %v", id, v)
	}
}

func validCPU(vmConfig VMConfig, cpu string) error {
	cpus, err := qemu.CPUs(vmConfig.QemuPath, vmConfig.Machine)
	if err != nil {
		return err
	}

	if !cpus[cpu] {
		return fmt.Errorf("invalid QEMU CPU: `%v`, see help", cpu)
	}

	return nil
}

func validMachine(vmConfig VMConfig, machine string) error {
	machines, err := qemu.Machines(vmConfig.QemuPath)
	if err != nil {
		return err
	}

	if !machines[machine] {
		return fmt.Errorf("invalid QEMU machine: `%v`, see help", machine)
	}

	return nil
}

func validNIC(vmConfig VMConfig, nic string) error {
	nics, err := qemu.NICs(vmConfig.QemuPath, vmConfig.Machine)
	if err != nil {
		return err
	}

	if !nics[nic] {
		return fmt.Errorf("invalid QEMU nic: `%v`, see help", nic)
	}

	return nil
}

func qemuSuggest(vals map[string]bool, prefix string) []string {
	var res []string

	for k := range vals {
		if strings.HasPrefix(k, prefix) {
			res = append(res, k)
		}
	}

	return res
}

func suggestCPU(ns *Namespace, val, prefix string) []string {
	cpus, err := qemu.CPUs(ns.vmConfig.QemuPath, ns.vmConfig.Machine)
	if err != nil {
		log.Info("suggest failed: %v", err)
		return nil
	}

	return qemuSuggest(cpus, prefix)
}

func suggestMachine(ns *Namespace, val, prefix string) []string {
	machines, err := qemu.Machines(ns.vmConfig.QemuPath)
	if err != nil {
		log.Info("suggest failed: %v", err)
		return nil
	}

	return qemuSuggest(machines, prefix)
}
