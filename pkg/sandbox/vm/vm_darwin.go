// Package vm implements experimental support for Devbox virtual machines on
// macOS.
package vm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

var boot fs.FS

// Default host resources to allocate to new VMs.
var (
	DefaultCPUs   int   = 1
	DefaultMemory int   = 1 << 30 // 1 GiB
	DefaultDisk   int64 = 1 << 32 // 4 GiB
)

// VM is a Devbox virtual machine. The zero value is a temporary VM that deletes
// after stopping.
type VM struct {
	// ID is a Virtualization Framework machine ID.
	ID []byte

	// CPUs is the number of CPU cores to allocate to the VM. For a new VM,
	// it defaults to the system-allowed minimum or DefaultCPUs, whichever
	// is larger. For existing VMs, it defaults to the value from the
	// previous run.
	CPUs int

	// Memory is the amount of memory in bytes to allocate to the VM. For a
	// new VM, it defaults to the system-allowed minimum or DefaultMemory,
	// whichever is larger. For existing VMs, it defaults to the value from
	// the previous run.
	Memory int

	// DiskSize is the size in bytes of the root disk. It's not possible to
	// change the size of an image after it's created. Setting DiskSize has
	// no effect on existing VMs.
	DiskSize int64

	// OS is the guest operating system. It must be either "darwin" or
	// "linux". Setting OS has no effect on existing VMs.
	OS string

	// Arch is the guest machine's architecture. It must be either "aarch64"
	// or "x86_64". Setting Arch has no effect on existing VMs.
	Arch string

	// Install boots from the NixOS installer ISO instead of the main image.
	Install bool

	// SharedDirectories is a list of host directories to share with the
	// guest operating system.
	SharedDirectories []SharedDirectory

	// HostDataDir is a directory containing the VM's state and
	// configuration. If HostDataDir is empty, it is set to a temporary
	// directory that is created the first time the VM starts and deleted
	// after the VM stops.
	HostDataDir string

	// Logger is where the host machine writes logs. It defaults to writing
	// them to a file named "log" inside HostDataDir. The logger's handler
	// should avoid writing to standard out or standard error so as to not
	// interfere with the VM's console output. Set it to a logger with any
	// level above slog.LevelError to disable logging.
	Logger *slog.Logger

	vzvm   *vz.VirtualMachine
	config *vz.VirtualMachineConfiguration
	files  dataDirectory
}

func (vm *VM) Start(ctx context.Context) error {
	var err error
	vm.files, err = fileBundle(vm.HostDataDir)
	if err != nil {
		return fmt.Errorf("create directory for virtual machine data: %v", err)
	}
	if vm.OS == "" {
		vm.OS = "linux"
	}
	if vm.Arch == "" {
		vm.Arch = "aarch64"
	}

	vm.initLogger()
	vm.configureCPUs()
	vm.configureMemory()

	loader, err := vm.linuxBootLoader(ctx)
	if err != nil {
		return fmt.Errorf("create boot loader: %v", err)
	}

	vm.Logger.Debug("creating virtual machine", "cpus", vm.CPUs, "memory", vm.Memory)
	vm.config, err = vz.NewVirtualMachineConfiguration(loader, uint(vm.CPUs), uint64(vm.Memory))
	if err != nil {
		return fmt.Errorf("create virtual machine configuration: %v", err)
	}
	if err := vm.attachConsole(); err != nil {
		return fmt.Errorf("attach console: %v", err)
	}
	if err := vm.attachDisks(ctx); err != nil {
		return fmt.Errorf("attach disks: %v", err)
	}
	if err := vm.attachNetwork(); err != nil {
		return fmt.Errorf("attach network: %v", err)
	}
	if err := vm.attachEntropy(); err != nil {
		return fmt.Errorf("attach entropy: %v", err)
	}
	if err := vm.attachSharedDirs(); err != nil {
		return fmt.Errorf("attach shared directories: %v", err)
	}
	if err := vm.configureLinuxPlatform(); err != nil {
		return fmt.Errorf("configure linux platform: %v", err)
	}

	valid, err := vm.config.Validate()
	if err != nil {
		return fmt.Errorf("invalid configuration: %v", err)
	}
	if !valid {
		return fmt.Errorf("invalid configuration")
	}
	vm.vzvm, err = vz.NewVirtualMachine(vm.config)
	if err != nil {
		return fmt.Errorf("create virtual machine: %v", err)
	}
	vm.Logger.Debug("starting virtual machine")
	return vm.vzvm.Start()
}

func (vm *VM) Stop(ctx context.Context) error {
	if vm == nil || vm.vzvm == nil {
		return nil
	}

	ch := make(chan error)
	go func() {
		ok, err := vm.vzvm.RequestStop()
		if !ok || err != nil {
			vm.vzvm.Stop()
			if err != nil {
				ch <- fmt.Errorf("could not shutdown gracefully: %v", err)
				return
			}
			ch <- fmt.Errorf("invalid machine state for stopping")
			return
		}
		close(ch)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ch:
		return err
	}
}

func (vm *VM) configureCPUs() {
	minCPU := int(vz.VirtualMachineConfigurationMinimumAllowedCPUCount())
	maxCPU := int(vz.VirtualMachineConfigurationMaximumAllowedCPUCount())
	if vm.CPUs == 0 {
		vm.loadStateData("cpu", &vm.CPUs)
		if vm.CPUs == 0 {
			vm.CPUs = clamp(DefaultCPUs, minCPU, maxCPU)
			vm.saveStateData("cpu", vm.CPUs)
			return
		}
	}
	clamped := clamp(vm.CPUs, minCPU, maxCPU)
	if vm.CPUs != clamped {
		vm.CPUs = clamped
		vm.saveStateData("cpu", vm.CPUs)
	}
}

func (vm *VM) configureMemory() {
	minMemory := int(vz.VirtualMachineConfigurationMinimumAllowedMemorySize())
	maxMemory := int(vz.VirtualMachineConfigurationMaximumAllowedMemorySize())
	if vm.Memory == 0 {
		vm.loadStateData("mem", &vm.Memory)
		if vm.Memory == 0 {
			vm.Memory = clamp(DefaultMemory, minMemory, maxMemory)
			vm.saveStateData("mem", vm.Memory)
			return
		}
	}
	clamped := clamp(vm.Memory, minMemory, maxMemory)
	if vm.Memory != clamped {
		vm.Memory = clamped
		vm.saveStateData("mem", vm.Memory)
	}
}

func clamp[T cmp.Ordered](value, min, max T) T {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (vm *VM) linuxBootLoader(ctx context.Context) (*vz.LinuxBootLoader, error) {
	if vm.Install {
		return vm.installerBootLoader(ctx)
	}

	guestInitPath, err := os.Readlink(vm.files.init)
	if err != nil {
		return nil, fmt.Errorf("determine path to kernel init file inside vm: %v", err)
	}
	// quiet loglevel=0 systemd.show_status=false udev.log_level=3
	params := fmt.Sprintf("console=hvc0 root=/dev/vda init=%s quiet boot.shell_on_fail rd.systemd.show_status=false rd.udev.log_level=3 rd.udev.log_priority=3", guestInitPath)
	vm.Logger.Debug("created boot loader", "params", params, "installer", vm.Install)
	return vz.NewLinuxBootLoader(vm.files.kernel,
		vz.WithInitrd(vm.files.initrd),
		vz.WithCommandLine(params),
	)
}

func (vm *VM) efiBootLoader() (*vz.EFIBootLoader, error) {
	nvram, err := vm.nvram()
	if err != nil {
		return nil, err
	}
	return vz.NewEFIBootLoader(vz.WithEFIVariableStore(nvram))
}

func (vm *VM) nvram() (*vz.EFIVariableStore, error) {
	path, err := vm.dataFilePath("nvram")
	if err != nil {
		return nil, fmt.Errorf("create nvram file: %v", err)
	}

	flag := os.O_RDWR | os.O_CREATE | os.O_EXCL
	perm := os.FileMode(0o600)
	f, err := os.OpenFile(path, flag, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create directory for nvram file: %v", err)
		}
		f, err = os.OpenFile(path, flag, perm)
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("open nvram file: %v", err)
	}

	created := err == nil
	if created {
		f.Close()
		nvram, err := vz.NewEFIVariableStore(path, vz.WithCreatingEFIVariableStore())
		if err != nil {
			return nil, fmt.Errorf("create nvram variable store: %v", err)
		}
		return nvram, nil
	}
	nvram, err := vz.NewEFIVariableStore(path)
	if err != nil {
		return nil, fmt.Errorf("load nvram variable store: %v", err)
	}
	return nvram, nil
}

func (vm *VM) attachConsole() error {
	fd := int(os.Stdin.Fd())
	term, err := unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
	if err != nil {
		return fmt.Errorf("put stdin in raw mode: get terminal attributes: %v", err)
	}

	// See `man termios` for reference.
	term.Iflag &^= unix.ICRNL              // disable CR-NL mapping
	term.Lflag &^= unix.ICANON | unix.ECHO // disable input canoncialization and echo

	// VMIN and VTIME control when a system read() call returns. VMIN is the minimum
	// number of characters to wait for and VTIME is the time to wait for them (in
	// tenths of a second). Here we're saying to only return after at least 1 byte is
	// available and to ignore time entirely. These settings are usually the default,
	// but explicitly set them anyway to be safe.
	term.Cc[unix.VMIN] = 1
	term.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, term); err != nil {
		return fmt.Errorf("put stdin in raw mode: set terminal attributes: %v", err)
	}
	attach, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	if err != nil {
		return fmt.Errorf("create serial port attachment: %v", err)
	}
	config, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attach)
	if err != nil {
		return fmt.Errorf("create serial port configuration: %v", err)
	}
	vm.config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{config})
	vm.Logger.Debug("attached console device")
	return nil
}

func (vm *VM) attachKeyboard() error {
	config, err := vz.NewUSBKeyboardConfiguration()
	if err != nil {
		return fmt.Errorf("create keyboard configuration: %w", err)
	}
	vm.config.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{config})
	vm.Logger.Debug("attached usb keyboard device")
	return nil
}

func (vm *VM) attachNetwork() error {
	attach, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return fmt.Errorf("create network attachment: %v", err)
	}
	config, err := vz.NewVirtioNetworkDeviceConfiguration(attach)
	if err != nil {
		return fmt.Errorf("create network configuration: %v", err)
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return fmt.Errorf("create random MAC address: %v", err)
	}
	config.SetMACAddress(mac)
	vm.config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{config})
	vm.Logger.Debug("attached network device")
	return nil
}

func (vm *VM) attachEntropy() error {
	config, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create entropy configuration: %v", err)
	}
	vm.config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{config})
	vm.Logger.Debug("attached entropy device")
	return nil
}

func (vm *VM) configureLinuxPlatform() error {
	err := vm.loadStateData("id", &vm.ID)
	if err != nil {
		return fmt.Errorf("load machine identifier: %v", err)
	}

	var id *vz.GenericMachineIdentifier
	if len(vm.ID) == 0 {
		id, err = vz.NewGenericMachineIdentifier()
		if err != nil {
			return fmt.Errorf("create machine identifier: %v", err)
		}
		vm.ID = id.DataRepresentation()
		if err := vm.saveStateData("id", vm.ID); err != nil {
			return fmt.Errorf("save machine identifier: %v", err)
		}
		vm.Logger.Debug("created new machine identifier")
	} else {
		id, err = vz.NewGenericMachineIdentifierWithData(vm.ID)
		if err != nil {
			return fmt.Errorf("load machine identifier: %v", err)
		}
		vm.Logger.Debug("loaded machine identifier")
	}

	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(id))
	if err != nil {
		return fmt.Errorf("create platform configuration: %v", err)
	}
	vm.config.SetPlatformVirtualMachineConfiguration(platform)
	return nil
}

func (vm *VM) attachSharedDirs() error {
	var configs []vz.DirectorySharingDeviceConfiguration
	if vm.Install {
		dir, err := vm.generateBootstrapScript()
		if err != nil {
			return fmt.Errorf("generate bootstrap files: %v", err)
		}
		bootDir, err := vm.dataFilePath("boot")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(bootDir, 0o700); err != nil {
			return err
		}
		vm.SharedDirectories = append(vm.SharedDirectories,
			SharedDirectory{
				Path:     dir,
				ReadOnly: true,
			},
			SharedDirectory{
				Path: bootDir,
			},
		)
	}
	for _, dir := range vm.SharedDirectories {
		config, err := vm.configureDirShare(dir.Path, dir.ReadOnly)
		if err != nil {
			return err
		}
		configs = append(configs, config)
	}
	vm.config.SetDirectorySharingDevicesVirtualMachineConfiguration(configs)
	vm.Logger.Debug("attached shared directories", "count", len(configs))
	return nil
}

func (vm *VM) configureDirShare(path string, readOnly bool) (*vz.VirtioFileSystemDeviceConfiguration, error) {
	dir, err := vz.NewSharedDirectory(path, readOnly)
	if err != nil {
		return nil, fmt.Errorf("create shared directory %s: %v", path, err)
	}
	share, err := vz.NewSingleDirectoryShare(dir)
	if err != nil {
		return nil, fmt.Errorf("create directory share %s: %v", path, err)
	}
	tag := "host"
	config, err := vz.NewVirtioFileSystemDeviceConfiguration(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("create virtiofs configuration %s -> %s: %v", path, tag, err)
	}
	config.SetDirectoryShare(share)
	vm.Logger.Debug("configured shared directory", "dir", path, "readonly", readOnly)
	return config, nil
}

func (vm *VM) attachDisks(ctx context.Context) error {
	root, err := vm.rootDisk()
	if err != nil {
		return fmt.Errorf("create root disk: %v", err)
	}

	disks := []vz.StorageDeviceConfiguration{root}
	if vm.Install {
		iso, err := vm.installerDisk(ctx)
		if err != nil {
			return fmt.Errorf("create installer disk: %v", err)
		}
		disks = append(disks, iso)
	}
	vm.config.SetStorageDevicesVirtualMachineConfiguration(disks)
	vm.Logger.Debug("attached disks", "count", len(disks))
	return nil
}

func (vm *VM) rootDisk() (vz.StorageDeviceConfiguration, error) {
	path, err := vm.dataFilePath("disk.img")
	if err != nil {
		return nil, fmt.Errorf("create root disk image: %v", err)
	}
	flag := os.O_RDWR | os.O_CREATE | os.O_EXCL
	perm := os.FileMode(0o600)
	f, err := os.OpenFile(path, flag, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create directory for root disk image: %v", err)
		}
		f, err = os.OpenFile(path, flag, perm)
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	created := err == nil
	if created {
		if vm.DiskSize == 0 {
			vm.DiskSize = DefaultDisk
		}
		err := f.Truncate(vm.DiskSize)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("truncate new root disk image: %v", err)
		}
	}

	attach, err := vz.NewDiskImageStorageDeviceAttachment(path, false)
	if err != nil {
		return nil, fmt.Errorf("create root disk image storage device: %v", err)
	}
	config, err := vz.NewVirtioBlockDeviceConfiguration(attach)
	if err != nil {
		return nil, fmt.Errorf("configure root disk image as block device: %v", err)
	}
	return config, nil
}

func (vm *VM) loadStateData(name string, value any) error {
	path, err := vm.dataFilePath(name)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fscanf(f, "%v", value)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

func (vm *VM) saveStateData(name string, value any) error {
	path, err := vm.dataFilePath(name)
	if err != nil {
		return err
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	perm := os.FileMode(0o644)
	f, err := os.OpenFile(path, flag, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err = os.OpenFile(path, flag, perm)
	}
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "%v\n", value)
	return err
}

func (vm *VM) dataFilePath(name string) (string, error) {
	if vm.HostDataDir == "" {
		path, err := os.MkdirTemp("", "devboxvm-")
		if err != nil {
			return "", fmt.Errorf("create temporary directory for virtual machine data: %v", err)
		}
		vm.HostDataDir = path
	}
	return filepath.Join(vm.HostDataDir, name), nil
}

func (vm *VM) nixSystem() string {
	return vm.Arch + "-" + vm.OS
}

func (vm *VM) initLogger() {
	fail := func(err error) {
		vm.Logger = slog.Default()
		vm.Logger.Error("could not create log file, using slog.Default()", "err", err)
	}

	path, err := vm.dataFilePath("log")
	if err != nil {
		fail(err)
		return
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	perm := os.FileMode(0o644)
	f, err := os.OpenFile(path, flag, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fail(err)
			return
		}
		f, err = os.OpenFile(path, flag, perm)
	}
	if err != nil {
		fail(err)
		return
	}
	vm.Logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	}))
}

type SharedDirectory struct {
	Path     string
	ReadOnly bool
}

type dataDirectory struct {
	path   string
	isTemp bool

	init      string
	initrd    string
	kernel    string
	bootstrap string
	image     string
	cpu       string
	memory    string
	id        string
	log       string
}

func fileBundle(dir string) (dataDirectory, error) {
	isTemp := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "devboxvm-")
		if err != nil {
			return dataDirectory{}, fmt.Errorf("create temporary directory for virtual machine data: %v", err)
		}
		isTemp = true
	}
	return dataDirectory{
		path:      dir,
		isTemp:    isTemp,
		init:      filepath.Join(dir, "boot", "default", "init"),
		initrd:    filepath.Join(dir, "boot", "nixos-initrd"),
		kernel:    filepath.Join(dir, "boot", "nixos-kernel"),
		bootstrap: filepath.Join(dir, "install", "install.sh"),
		cpu:       filepath.Join(dir, "cpu"),
		image:     filepath.Join(dir, "disk.img"),
		id:        filepath.Join(dir, "id"),
		log:       filepath.Join(dir, "log"),
		memory:    filepath.Join(dir, "mem"),
	}, nil
}

func (d dataDirectory) cleanup() error {
	if !d.isTemp {
		return nil
	}
	slog.Debug("would delete " + d.path)
	return nil
	// os.RemoveAll(d.path)
}
