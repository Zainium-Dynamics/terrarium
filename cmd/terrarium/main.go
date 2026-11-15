// terrarium puts a complete, installed Zainium OS on a USB drive, straight from the ISO: no
// live session, no second stick. The drive then starts any computer (UEFI or BIOS) and keeps
// your files and apps, optionally encrypted.
//
//	terrarium [options] <zainium-live.iso> <usb drive>
//	terrarium --list
//
// It runs as root and uses the installer's own engine, so the result is the same system the
// installer makes. At the end it checks the drive once.
package main

import (
	"bufio"
	"crypto/sha256"
	"debug/elf"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Zainium-Dynamics/zainium-installer/pkg/disk"
	"github.com/Zainium-Dynamics/zainium-installer/pkg/engine"
	"github.com/Zainium-Dynamics/zainium-installer/pkg/plan"
	"github.com/Zainium-Dynamics/zainium-installer/pkg/progress"
)

const work = "/run/terrarium"

var in = bufio.NewReader(os.Stdin)

func main() {
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: terrarium [--encrypt] <zainium-live.iso> <usb drive>\n       terrarium --list\n\nPuts an installed Zainium OS on a USB drive. Everything on the drive is erased.\n")
	}
	list := flag.Bool("list", false, "show the USB drives")
	encrypt := flag.Bool("encrypt", false, "encrypt the drive")
	flag.Parse()

	if *list {
		listUSB()
		return
	}
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	iso, dev := flag.Arg(0), flag.Arg(1)
	if os.Geteuid() != 0 {
		fail(errors.New("run it as root (elevate or sudo)"))
	}

	d, err := findUSB(dev)
	if err != nil {
		fail(err)
	}
	if m := mountedParts(d.Name); len(m) > 0 {
		fail(fmt.Errorf("%s is in use (%s mounted): unmount it first", d.Path, strings.Join(m, ", ")))
	}

	p := plan.Plan{
		Disk: d.Path, Hostname: "zainium", Language: "en_US.UTF-8", Keyboard: "us", Timezone: hostTimezone(),
		Portable: true,
	}
	p.User.FullName = ask("Your name", "")
	p.User.Name = ask("User name", "")
	p.User.Password = askSecret("Password")
	p.User.Admin = true
	if *encrypt {
		p.Encrypt = askSecret("Encryption passphrase (asked at every start)")
	}
	if err := p.Validate(); err != nil {
		fail(err)
	}

	fmt.Printf("\n  Drive:  %s, %s\n  Image:  %s\n  User:   %s\n  Encrypt: %v\n\n", d.Title(), disk.HumanSize(d.Size), iso, p.User.Name, *encrypt)
	fmt.Printf("Everything on %s will be erased. Type the drive name (%s) to go on: ", d.Path, d.Name)
	if a, _ := in.ReadString('\n'); strings.TrimSpace(a) != d.Name && strings.TrimSpace(a) != d.Path {
		fail(errors.New("stopped, nothing was changed"))
	}

	// stop cleanly on Ctrl+C: nothing stays mounted, opened or attached
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\nterrarium: stopping, cleaning up (the drive is not usable until you run terrarium again)")
		emergencyCleanup()
		os.Exit(130)
	}()

	src, err := openImage(iso)
	if err != nil {
		fail(err)
	}
	if err := useImageTools(src); err != nil {
		fail(err)
	}

	// Building straight on a USB stick is very slow: thousands of small files and the
	// fs-verity seal of each. So the system is built in an image on this computer's disk,
	// the same size as the drive, and then written to the drive in long sequential runs.
	staging = filepath.Join(filepath.Dir(absPath(iso)), ".terrarium")
	loop, err := stagingImage(d.Size)
	if err != nil {
		fail(err)
	}
	fmt.Println("\nBuilding Zainium OS on this computer first")
	r := progress.NewReporter(&progress.TextWriter{W: os.Stdout})
	sp := p
	sp.Disk = loop
	e := &engine.Engine{Plan: sp, R: r, Source: src, Staging: true}
	if err := e.Run(); err != nil {
		r.Fail(err)
		fail(errors.New("building the image failed, the drive was not touched"))
	}

	fmt.Printf("\nWriting it to %s\n", d.Path)
	if err := writeDrive(loop, d.Path, p.Encrypt); err != nil {
		fail(fmt.Errorf("writing the drive: %w", err))
	}

	fmt.Println("\nChecking the drive")
	if err := check(d.Path, src, p.Encrypt); err != nil {
		fail(fmt.Errorf("check: %w", err))
	}
	runCleanups()
	fmt.Printf("\nDone. %s now starts Zainium OS on any computer: choose it in the boot menu.\n", d.Path)
}

// cleanups run in reverse at the end, on failure and on Ctrl+C.
var cleanups []func()

func atCleanup(f func()) { cleanups = append(cleanups, f) }

func runCleanups() {
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	cleanups = nil
}

// emergencyCleanup also undoes what the engine had mounted or opened when it was stopped.
func emergencyCleanup() {
	for _, m := range []string{"/run/zainium-install/target", "/run/zainium-install/esp", "/run/zainium-install/source"} {
		unix.Unmount(m, unix.MNT_DETACH)
	}
	for _, name := range []string{"zainium-root", "terrarium-src", "terrarium-dst", "terrarium-check"} {
		if _, err := os.Stat("/dev/mapper/" + name); err == nil {
			exec.Command(tool("cryptsetup"), "close", name).Run()
		}
	}
	runCleanups()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "terrarium:", err)
	emergencyCleanup()
	os.Exit(1)
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// tool finds a program on PATH, else in the base.
func tool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return "/overlayer/syshub/bin/" + name
}

func listUSB() {
	ds, err := disk.List()
	if err != nil {
		fail(err)
	}
	n := 0
	for _, d := range ds {
		if d.USB || d.Removable {
			fmt.Printf("  %-10s %-9s %s\n", d.Path, disk.HumanSize(d.Size), d.Title())
			n++
		}
	}
	if n == 0 {
		fmt.Println("  no USB drive found")
	}
}

// findUSB accepts only a whole USB or removable drive that the installer may erase.
func findUSB(dev string) (disk.Disk, error) {
	ds, err := disk.List()
	if err != nil {
		return disk.Disk{}, err
	}
	for _, d := range ds {
		if d.Path != dev && d.Name != dev {
			continue
		}
		if !d.USB && !d.Removable {
			return d, fmt.Errorf("%s is not a USB drive; terrarium only writes to USB drives (use the installer for internal disks)", d.Path)
		}
		if ok, why := d.Usable(); !ok {
			return d, fmt.Errorf("cannot use %s: %s", d.Path, why)
		}
		return d, nil
	}
	return disk.Disk{}, fmt.Errorf("%s is not a disk on this computer (terrarium --list shows the USB drives)", dev)
}

func mountedParts(name string) []string {
	var m []string
	for _, mi := range disk.Mountinfo() {
		if strings.HasPrefix(filepath.Base(mi.Source), name) {
			m = append(m, mi.Target)
		}
	}
	return m
}

func ask(label, val string) string {
	for val == "" {
		fmt.Printf("%s: ", label)
		s, err := in.ReadString('\n')
		if err != nil && s == "" {
			fail(errors.New("no input"))
		}
		val = strings.TrimSpace(s)
	}
	return val
}

// askSecret reads a secret twice, without echo when stdin is a terminal.
func askSecret(label string) string {
	for {
		a := readHidden(label + ": ")
		b := readHidden("Again: ")
		if a == "" {
			fmt.Println("It can not be empty.")
			continue
		}
		if a != b {
			fmt.Println("They do not match, try again.")
			continue
		}
		return a
	}
}

func readHidden(prompt string) string {
	fmt.Print(prompt)
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		t := *old
		t.Lflag &^= unix.ECHO
		unix.IoctlSetTermios(fd, unix.TCSETS, &t)
		defer func() {
			unix.IoctlSetTermios(fd, unix.TCSETS, old)
			fmt.Println()
		}()
	}
	s, _ := in.ReadString('\n')
	return strings.TrimRight(s, "\r\n")
}

func hostTimezone() string {
	for _, l := range []string{"/etc/localtime", "/overlayer/syshub/etc/localtime"} {
		if t, err := os.Readlink(l); err == nil {
			if i := strings.Index(t, "zoneinfo/"); i >= 0 {
				return t[i+len("zoneinfo/"):]
			}
		}
	}
	return "UTC"
}

// openImage mounts the ISO and the system image inside it, read-only, and returns the tree.
func openImage(iso string) (string, error) {
	isoDir, sysDir := filepath.Join(work, "iso"), filepath.Join(work, "system")
	if err := os.MkdirAll(isoDir, 0o700); err != nil {
		return "", err
	}
	os.MkdirAll(sysDir, 0o700)
	atCleanup(func() { os.RemoveAll(work) })
	l, err := attach(iso, true, false)
	if err != nil {
		return "", err
	}
	if err := mountRO(l, isoDir, "iso9660"); err != nil {
		return "", fmt.Errorf("%s is not a Zainium ISO (mount: %w)", iso, err)
	}
	img := filepath.Join(isoDir, "zaisys/zairoot.img")
	if _, err := os.Stat(img); err != nil {
		return "", fmt.Errorf("%s is not a Zainium live ISO (no zaisys/zairoot.img)", iso)
	}
	if l, err = attach(img, true, false); err != nil {
		return "", err
	}
	if err := mountRO(l, sysDir, "erofs"); err != nil {
		return "", fmt.Errorf("mount the system image (does this kernel have erofs?): %w", err)
	}
	return sysDir, nil
}

// attach sets up a loop device for a file straight with the kernel (losetup differs between
// systems, and Zainium's has no partition scan) and detaches it at cleanup.
func attach(file string, readOnly, partscan bool) (string, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("loop devices: %w", err)
	}
	n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
	ctl.Close()
	if err != nil {
		return "", fmt.Errorf("no free loop device: %w", err)
	}
	dev := fmt.Sprintf("/dev/loop%d", n)
	mode := os.O_RDWR
	if readOnly {
		mode = os.O_RDONLY
	}
	f, err := os.OpenFile(file, mode, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	l, err := os.OpenFile(dev, mode, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", dev, err)
	}
	defer l.Close()
	var cfg unix.LoopConfig
	cfg.Fd = uint32(f.Fd())
	cfg.Info.Flags = 0
	if readOnly {
		cfg.Info.Flags |= unix.LO_FLAGS_READ_ONLY
	}
	if partscan {
		cfg.Info.Flags |= unix.LO_FLAGS_PARTSCAN
	}
	copy(cfg.Info.File_name[:], file)
	if err := unix.IoctlLoopConfigure(int(l.Fd()), &cfg); err != nil {
		return "", fmt.Errorf("loop %s on %s: %w", file, dev, err)
	}
	atCleanup(func() {
		if l, err := os.Open(dev); err == nil {
			unix.IoctlSetInt(int(l.Fd()), unix.LOOP_CLR_FD, 0)
			l.Close()
		}
	})
	return dev, nil
}

func mountRO(dev, dir, fs string) error {
	if err := unix.Mount(dev, dir, fs, unix.MS_RDONLY, ""); err != nil {
		return err
	}
	atCleanup(func() { unix.Unmount(dir, unix.MNT_DETACH) })
	return nil
}

// imageTools are the programs terrarium and the engine run. They are taken from the ISO's own
// system, with its own musl loader and libraries, so terrarium works on any Linux and always
// makes the drive with the same tools as the installer.
var imageTools = []string{"mkfs.ext4", "e2image", "cryptsetup", "limine"}

// useImageTools writes a small wrapper for each of them and puts their directory first on PATH.
func useImageTools(sys string) error {
	base := filepath.Join(sys, "overlayer/syshub")
	loader := filepath.Join(base, "x86_64-zainium-linux-musl/lib/ld-musl-x86_64.so.1")
	libs := filepath.Join(base, "lib") + ":" + filepath.Join(base, "x86_64-zainium-linux-musl/lib")
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	for _, t := range imageTools {
		prog := ""
		for _, dir := range []string{"bin", "sbin"} {
			if _, err := os.Stat(filepath.Join(base, dir, t)); err == nil {
				prog = filepath.Join(base, dir, t)
				break
			}
		}
		if prog == "" {
			return fmt.Errorf("the ISO has no %s", t)
		}
		run := fmt.Sprintf("exec %q \"$@\"\n", prog)
		if dynamic(prog) {
			run = fmt.Sprintf("exec %q --library-path %q --argv0 %q %q \"$@\"\n", loader, libs, t, prog)
		}
		if t == "cryptsetup" {
			// the computer's udev may not answer device-mapper (an old or missing dmsetup),
			// and cryptsetup would wait for it forever: it makes the device nodes itself
			run = "DM_DISABLE_UDEV=1 " + run
		}
		if err := os.WriteFile(filepath.Join(bin, t), []byte("#!/bin/sh\n"+run), 0o700); err != nil {
			return err
		}
	}
	os.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if out, err := exec.Command(tool("mkfs.ext4"), "-V").CombinedOutput(); err != nil {
		return fmt.Errorf("running mkfs.ext4 from the ISO: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dynamic says whether an ELF program needs a loader.
func dynamic(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}

// staging is where the drive is built first: next to the ISO, on this computer's own disk,
// where there is room to write (in Zainium /var/tmp is part of the sealed base).
var staging string

// stagingImage makes a sparse image of the drive's size and attaches it with partitions.
func stagingImage(size int64) (string, error) {
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	var st unix.Statfs_t
	if unix.Statfs(staging, &st) == nil {
		if free := int64(st.Bavail) * st.Bsize; free < 14<<30 {
			return "", fmt.Errorf("%s has %s free; building the drive needs about 14 GB there for a while", staging, disk.HumanSize(free))
		}
	}
	img := filepath.Join(staging, "drive.img")
	os.Remove(img)
	f, err := os.Create(img)
	if err != nil {
		return "", err
	}
	err = f.Truncate(size)
	f.Close()
	if err != nil {
		return "", err
	}
	atCleanup(func() { os.RemoveAll(staging) })
	return attach(img, false, true)
}

// writeDrive copies the built image to the drive: the start (partition table, BIOS boot,
// EFI partition) and the backup partition table byte for byte, and of the root only the
// blocks the file system uses (e2image), through LUKS when it is encrypted.
func writeDrive(loop, dev, passphrase string) error {
	start, size, err := partExtent(loop, disk.PartRoot)
	if err != nil {
		return err
	}
	img, err := os.Open(loop)
	if err != nil {
		return err
	}
	defer img.Close()
	out, err := os.OpenFile(dev, os.O_WRONLY|unix.O_EXCL, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", dev, err)
	}
	total, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		out.Close()
		return err
	}
	if err := copyRange(img, out, 0, start, "partition table and boot partitions"); err != nil {
		out.Close()
		return err
	}
	if err := copyRange(img, out, start+size, total-(start+size), "backup partition table"); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	out.Close()

	// the kernel reads the new partition table
	if f, err := os.Open(dev); err == nil {
		disk.Reread(f)
		f.Close()
	}
	exec.Command(tool("udevadm"), "settle", "--timeout=10").Run()
	srcRoot, dstRoot := disk.PartitionPath(loop, disk.PartRoot), disk.PartitionPath(dev, disk.PartRoot)
	if err := disk.WaitFor(dstRoot, 15e9); err != nil {
		return err
	}

	if passphrase != "" {
		hdr, err := luksDataOffset(srcRoot)
		if err != nil {
			return err
		}
		si, err := os.Open(srcRoot)
		if err != nil {
			return err
		}
		so, err := os.OpenFile(dstRoot, os.O_WRONLY, 0)
		if err != nil {
			si.Close()
			return err
		}
		err = copyRange(si, so, 0, hdr, "encryption header")
		if err == nil {
			err = so.Sync()
		}
		si.Close()
		so.Close()
		if err != nil {
			return err
		}
		if err := luksOpen(srcRoot, "terrarium-src", passphrase, true); err != nil {
			return err
		}
		defer exec.Command(tool("cryptsetup"), "close", "terrarium-src").Run()
		if err := luksOpen(dstRoot, "terrarium-dst", passphrase, false); err != nil {
			return err
		}
		defer exec.Command(tool("cryptsetup"), "close", "terrarium-dst").Run()
		srcRoot, dstRoot = "/dev/mapper/terrarium-src", "/dev/mapper/terrarium-dst"
	}
	fmt.Println("  the system, only the blocks it uses (then the drive finishes writing, which can take a while on a slow stick)")
	c := exec.Command(tool("e2image"), "-ra", "-p", srcRoot, dstRoot)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("e2image: %w", err)
	}
	flush(dev)
	return nil
}

// flush waits for the data still in memory to reach the drive, saying how much is left:
// a USB stick can slow down a lot at the end, and a silent wait looks like a hang.
func flush(dev string) {
	done := make(chan struct{})
	go func() {
		unix.Sync()
		close(done)
	}()
	stat := filepath.Join("/sys/class/block", filepath.Base(dev), "stat")
	written := func() int64 {
		b, _ := os.ReadFile(stat)
		f := strings.Fields(string(b))
		if len(f) < 7 {
			return 0
		}
		var v int64
		fmt.Sscan(f[6], &v)
		return v * 512
	}
	last := written()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			fmt.Println("  all data is on the drive")
			return
		case <-t.C:
			now := written()
			fmt.Printf("  finishing writes to the drive: %s left, %s/s\n", disk.HumanSize(pendingWrites()), disk.HumanSize((now-last)/5))
			last = now
		}
	}
}

// pendingWrites is how much written data the kernel still holds in memory.
func pendingWrites() int64 {
	b, _ := os.ReadFile("/proc/meminfo")
	var total int64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "Dirty:" || f[0] == "Writeback:") {
			var kb int64
			fmt.Sscan(f[1], &kb)
			total += kb << 10
		}
	}
	return total
}

// partExtent is where a partition of a disk starts and how long it is, in bytes.
func partExtent(dev string, n int) (int64, int64, error) {
	sys := filepath.Join("/sys/class/block", filepath.Base(disk.PartitionPath(dev, n)))
	read := func(f string) (int64, error) {
		b, err := os.ReadFile(filepath.Join(sys, f))
		if err != nil {
			return 0, err
		}
		var v int64
		_, err = fmt.Sscan(string(b), &v)
		return v * 512, err
	}
	start, err := read("start")
	if err != nil {
		return 0, 0, fmt.Errorf("partition %d of %s: %w", n, dev, err)
	}
	size, err := read("size")
	return start, size, err
}

// copyRange copies n bytes at off from src to the same offset of dst, showing progress.
func copyRange(src, dst *os.File, off, n int64, what string) error {
	if _, err := dst.Seek(off, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 4<<20)
	r := io.NewSectionReader(src, off, n)
	var done int64
	last := int64(-1)
	for {
		k, err := r.Read(buf)
		if k > 0 {
			if _, werr := dst.Write(buf[:k]); werr != nil {
				return werr
			}
			done += int64(k)
			if pct := done * 100 / max(n, 1); pct/10 != last/10 {
				last = pct
				fmt.Printf("  %s: %s of %s\n", what, disk.HumanSize(done), disk.HumanSize(n))
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// luksDataOffset is the size of the LUKS2 header area: where the encrypted data begins.
func luksDataOffset(dev string) (int64, error) {
	out, err := exec.Command(tool("cryptsetup"), "luksDump", dev).Output()
	if err != nil {
		return 0, fmt.Errorf("cryptsetup luksDump %s: %w", dev, err)
	}
	seg := false
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Data segments:") {
			seg = true
		}
		var v int64
		if seg && strings.HasPrefix(t, "offset:") {
			if _, err := fmt.Sscanf(t, "offset: %d", &v); err == nil && v > 0 {
				return v, nil
			}
		}
	}
	return 0, errors.New("no data offset in the LUKS header")
}

func luksOpen(dev, name, key string, ro bool) error {
	args := []string{"open", "--key-file=-"}
	if ro {
		args = append(args, "--readonly")
	}
	c := exec.Command(tool("cryptsetup"), append(args, dev, name)...)
	c.Stdin = strings.NewReader(key)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("cryptsetup open %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// check mounts the finished drive read-only and compares it with the image: the boot files on
// the EFI partition, the kernel and initramfs, and that every base file was copied.
func check(dev, src, passphrase string) error {
	espDir, rootDir := filepath.Join(work, "check-esp"), filepath.Join(work, "check-root")
	os.MkdirAll(espDir, 0o700)
	os.MkdirAll(rootDir, 0o700)
	exec.Command(tool("udevadm"), "settle").Run()

	esp := disk.PartitionPath(dev, disk.PartESP)
	if err := unix.Mount(esp, espDir, "vfat", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount the EFI partition %s: %w", esp, err)
	}
	defer unix.Unmount(espDir, 0)
	for _, f := range []string{"EFI/BOOT/BOOTX64.EFI", "limine.conf"} {
		if _, err := os.Stat(filepath.Join(espDir, f)); err != nil {
			return fmt.Errorf("%s missing on the EFI partition", f)
		}
	}
	ok("UEFI boot files (EFI/BOOT/BOOTX64.EFI, limine.conf)")

	rootPart := disk.PartitionPath(dev, disk.PartRoot)
	if passphrase != "" {
		c := exec.Command(tool("cryptsetup"), "open", "--readonly", "--key-file=-", rootPart, "terrarium-check")
		c.Stdin = strings.NewReader(passphrase)
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("open the encrypted root: %v: %s", err, out)
		}
		defer exec.Command(tool("cryptsetup"), "close", "terrarium-check").Run()
		rootPart = "/dev/mapper/terrarium-check"
		ok("encrypted root opens with the passphrase")
	}
	if err := unix.Mount(rootPart, rootDir, "ext4", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount the root %s: %w", rootPart, err)
	}
	defer unix.Unmount(rootDir, 0)

	for _, f := range []string{"overlayer/zaisys/kernel/linux-zainium", "overlayer/zaisys/kernel/quantra-ramfs.img"} {
		a, err := hashFile(filepath.Join(src, f))
		if err != nil {
			return err
		}
		b, err := hashFile(filepath.Join(rootDir, f))
		if err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("%s differs from the image", f)
		}
	}
	ok("kernel and initramfs match the image")

	// the base is sealed: fs-verity is on, and reading a sealed file checks it block by block
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filepath.Join(rootDir, "overlayer/zaisys/kernel/linux-zainium"), 0, unix.STATX_BASIC_STATS, &stx); err != nil {
		return err
	}
	if stx.Attributes&unix.STATX_ATTR_VERITY == 0 {
		return errors.New("the base on the drive is not sealed with fs-verity")
	}
	ok("the base is sealed with fs-verity")

	want, got := 0, 0
	for _, top := range []string{"overlayer/syshub", "overlayer/zaisys"} {
		filepath.WalkDir(filepath.Join(src, top), func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return nil
			}
			want++
			rel, _ := filepath.Rel(src, p)
			if _, err := os.Lstat(filepath.Join(rootDir, rel)); err == nil {
				got++
			}
			return nil
		})
	}
	if got < want {
		return fmt.Errorf("%d of %d base files are missing", want-got, want)
	}
	ok(fmt.Sprintf("all %d base files are on the drive", want))
	return nil
}

func ok(s string) { fmt.Printf("  ✓ %s\n", s) }

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
