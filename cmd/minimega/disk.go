// Copyright (2014) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sandia-minimega/minimega/v2/internal/nbd"
	"github.com/sandia-minimega/minimega/v2/pkg/minicli"
	log "github.com/sandia-minimega/minimega/v2/pkg/minilog"
)

// #include "linux/fs.h"
import "C"

const (
	INJECT_COMMAND = iota
	GET_BACKING_IMAGE_COMMAND
)

type DiskInfo struct {
	Format      string
	VirtualSize string
	DiskSize    string
	BackingFile string
	FileSystem  string
}

type FSType string

const (
	LVM   FSType = "lvm"
	ZFS   FSType = "zfs"
	EXT4  FSType = "ext4"
	NTFS  FSType = "ntfs"
	BTRFS FSType = "btrfs"
	NONE  FSType = ""
)

var diskCLIHandlers = []minicli.Handler{
	{ // disk
		HelpShort: "manipulate qcow disk images image",
		HelpLong: `
Manipulate qcow disk images. Supports creating new images, snapshots of
existing images, and injecting one or more files into an existing image.

Example of creating a new disk:

	disk create qcow2 foo.qcow2 100G

The size argument is the size in bytes, or using optional suffixes "k"
(kilobyte), "M" (megabyte), "G" (gigabyte), "T" (terabyte).

Example of taking a snapshot of a disk:

	disk snapshot windows7.qc2 window7_miniccc.qc2

If the destination name is omitted, a name will be randomly generated and the
snapshot will be stored in the 'files' directory. Snapshots are always created
in the 'files' directory.

To inject files into an image:

	disk inject window7_miniccc.qc2 files "miniccc":"Program Files/miniccc"

Each argument after the image should be a source and destination pair,
separated by a ':'. If the file paths contain spaces, use double quotes.
Optionally, you may specify a partition (partition 1 will be used by default):

	disk inject window7_miniccc.qc2:2 files "miniccc":"Program Files/miniccc"

You may also specify that there is no partition on the disk, if your filesystem
was directly written to the disk (this is highly unusual):

	disk inject partitionless_disk.qc2:none files /miniccc:/miniccc

To choose a File System Type specify the fstype flag, the default is EXT4:

	(LVM) disk inject linux_mccc.qc2:<volumegroup>:<logical volume> fstype LVM files "miniccc":"Program Files/miniccc"
	(ZFS) disk inject linux_mccc.qc2:<partition>:<zpool name> fstype ZFS files "miniccc":"Program Files/miniccc"

You can optionally specify mount arguments to use with inject. Multiple options
should be quoted. For example:

	disk inject foo.qcow2 options "-t fat -o offset=100" files foo:bar

Disk image paths are always relative to the 'files' directory. Users may also
use absolute paths if desired. The backing images for snapshots should always
be in the files directory.`,
		Patterns: []string{
			"disk <create,> <qcow2,raw> <image name> <size>",
			"disk <snapshot,> <image> [dst image]",
			"disk <inject,> <image> files <files like /path/to/src:/path/to/dst>...",
			"disk <inject,> <image> options <options> files <files like /path/to/src:/path/to/dst>...",
			"disk <inject,> <image> options <options> fstype <fstype> files <files like /path/to/src:/path/to/dst>...",
			"disk <inject,> <image> fstype <fstype> files <files like /path/to/src:/path/to/dst>...",
			"disk <info,> <image>",
		},
		Call: wrapSimpleCLI(cliDisk),
	},
}

// diskSnapshot creates a new image, dst, using src as the backing image.
func diskSnapshot(src, dst string) error {
	if !strings.HasPrefix(src, *f_iomBase) {
		log.Warn("minimega expects backing images to be in the files directory")
	}

	out, err := processWrapper("qemu-img", "create", "-f", "qcow2", "-b", src, dst)
	if err != nil {
		return fmt.Errorf("[image %s] %v: %v", src, out, err)
	}

	return nil
}

// diskInfo return information about the disk.
func diskInfo(image string) (DiskInfo, error) {
	info := DiskInfo{}

	out, err := processWrapper("qemu-img", "info", image)
	if err != nil {
		return info, fmt.Errorf("[image %s] %v: %v", image, out, err)
	}

	regex := regexp.MustCompile(`.*\(actual path: (.*)\)`)

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}

		switch parts[0] {
		case "file format":
			info.Format = parts[1]
		case "virtual size":
			info.VirtualSize = parts[1]
		case "disk size":
			info.DiskSize = parts[1]
		case "backing file":
			// In come cases, `qemu-img info` includes the actual absolute path for
			// the backing image. We want to use that, if present.
			if match := regex.FindStringSubmatch(parts[1]); match != nil {
				info.BackingFile = match[1]
			} else {
				info.BackingFile = parts[1]
			}
		}
	}

	return info, nil
}

// diskCreate creates a new disk image, dst, of given size/format.
func diskCreate(format, dst, size string) error {
	out, err := processWrapper("qemu-img", "create", "-f", format, dst, size)
	if err != nil {
		log.Error("diskCreate: %v", out)
		return err
	}
	return nil
}

// diskInject injects files into a disk image. dst/partition specify the image
// and the partition number, pairs is the dst, src filepaths. options can be
// used to supply mount arguments.
func diskInject(dst, partition string, fstype string, pairs map[string]string, options []string) error {
	// Load nbd
	if err := nbd.Modprobe(); err != nil {
		return err
	}

	// create a tmp mount point
	mntDir, err := ioutil.TempDir(*f_base, "dstImg")
	if err != nil {
		return err
	}
	log.Debug("temporary mount point: %v", mntDir)
	defer func() {
		if err := os.Remove(mntDir); err != nil {
			log.Error("rm mount dir failed: %v", err)
		}
	}()

	nbdPath, err := nbd.ConnectImage(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := nbd.DisconnectDevice(nbdPath); err != nil {
			log.Error("nbd disconnect failed: %v", err)
		}
	}()

	devPath := nbdPath

	f, err := os.Open(nbdPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// decide whether to mount partition or raw disk
	if partition != "none" {
		// keep rereading partitions and waiting for them to show up for a bit
		timeoutTime := time.Now().Add(5 * time.Second)
		for i := 1; ; i++ {
			if time.Now().After(timeoutTime) {
				return fmt.Errorf("[image %s] no partitions found on image", dst)
			}

			// tell kernel to reread partitions
			syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), C.BLKRRPART, 0)

			_, err = os.Stat(nbdPath + "p1")
			if err == nil {
				log.Info("partitions detected after %d attempt(s)", i)
				break
			}

			time.Sleep(100 * time.Millisecond)
		}

		// default to first partition if there is only one partition
		if partition == "" {
			_, err = os.Stat(nbdPath + "p2")
			if err == nil {
				return fmt.Errorf("[image %s] please specify a partition; multiple found", dst)
			}

			partition = "1"
		}

		devPath = nbdPath + "p" + partition
	}

	var volumeGroup string
	var logicalVolume string
	var zpool string

	// determine file system type and provide mount arguments accordingly
	switch FSType(fstype) {
	case LVM:

		// the format is <volume group>:<logical volume>
		partitionSplit := strings.Split(partition, ":")

		if len(partitionSplit) == 2 {
			volumeGroup = partitionSplit[0]
			logicalVolume = partitionSplit[1]
		} else {
			log.Error("failed to determine LVM. can not find volume group,logical volume.")
			return fmt.Errorf("failed to determine LVM.")
		}

		// scan for existing lvms and check for the one provided
		vgscan, err := processWrapper("vgscan")
		if err != nil {
			log.Error("failed to mount LVM. vgscan does not exist")
			return fmt.Errorf("failed to mount LVM. %s", err)
		}

		if vgscan == "" || !strings.Contains(vgscan, volumeGroup) {
			log.Error("failed to mount LVM. volume group specified does not exist")
			return fmt.Errorf("failed to mount LVM. volume group specified does not exist")
		}

		// activate the volume group so it can be mounted
		_, err = processWrapper("vgchange", "-ay", volumeGroup)

		if err != nil {
			log.Error("failed to mount LVM. failed to activate volume group")
			return fmt.Errorf("failed to mount LVM. failed to activate volume group %s", err)
		}

		// update the path to the disk image to mount
		devPath = fmt.Sprintf("/dev/%s/%s", volumeGroup, logicalVolume)

		args := []string{"mount"}
		if len(options) != 0 {
			args = append(args, options...)
			args = append(args, devPath, mntDir)
		} else {
			args = []string{"mount", "-w", devPath, mntDir}
		}
		log.Debug("mount args: %v", args)

		_, err = processWrapper(args...)

	case ZFS:
		// the format is <physical partition number>:<zpool name>
		var parse bool
		zpool = ""
		partitionSplit := strings.Split(partition, ":")

		if len(partitionSplit) == 2 {
			partition = partitionSplit[0]
			zpool = partitionSplit[1]

		} else if len(partitionSplit) == 1 {
			zpool = partition
			parse = true

		} else {
			log.Error("failed to determine partition. format was incorrect - <physical partition number>:<zpool name>")
			return fmt.Errorf("failed to determine zpool and partition.")
		}

		/*
		 use zpool over mount for zfs
		 zpool import by itself lists available pools
		 zpool import <pool name> will then import(mount) the pool
		 Ensure using the -R flag to specify where the root of the pool goes
		 Also use the -d flag to specify the directory/drive to search for the pool

		 Figure out if you want to parse out the partition number or have it be provided????
		*/

		// List zpools available and determine if the provided one is available
		zpool_scan, err := processWrapper("zpool", "import")

		if !strings.Contains(zpool_scan, zpool) || err != nil {
			return fmt.Errorf("[image %s] desired zpool %s not found", dst, zpool)
		}

		if parse {
			zpool_scan_split := strings.Split(zpool_scan, "\n")
			for i := 0; i < len(zpool_scan_split); i++ {
				line := zpool_scan_split[i]
				if strings.Contains(line, zpool) && strings.Contains(line, "ONLINE") {
					device := strings.Fields(zpool_scan_split[i+1])[0]
					devPath = fmt.Sprintf("/dev/%s", device)
					break
				}
			}
		} else {
			devPath = nbdPath + "p" + partition
		}

		_, err = os.Stat(devPath)
		if err != nil {
			return fmt.Errorf("[image %s] desired partition %s not found", dst, partition)
		} else {
			log.Info("desired partition %s found in image %s", partition, dst)
		}

		args := []string{"zpool", "import"}
		args = append(args, zpool, "-R", mntDir, "-d", devPath, "-f")

		out, err := processWrapper(args...)

		if err != nil {
			log.Error("failed to mount partition")
			return fmt.Errorf("[image %s] %v: %v", dst, out, err)
		}

		// export (unmount) the zpool from the system so the drive can be disconnected

	case NTFS:

		// check that ntfs-3g is installed
		_, err = processWrapper("ntfs-3g", "--version")
		if err != nil {
			log.Error("ntfs-3g not found, ntfs images unwriteable")
		}

		// mount with ntfs-3g
		out, err := processWrapper("mount", "-o", "ntfs-3g", devPath, mntDir)
		if err != nil {
			log.Error("failed to mount partition")
			return fmt.Errorf("[image %s] %v: %v", dst, out, err)
		}

	default:

		args := []string{"mount"}
		if len(options) != 0 {
			args = append(args, options...)
			args = append(args, devPath, mntDir)
		} else {
			args = []string{"mount", "-w", devPath, mntDir}
		}
		log.Debug("mount args: %v", args)

		out, err := processWrapper(args...)

		if err != nil {
			log.Error("failed to mount partition")
			return fmt.Errorf("[image %s] %v: %v", dst, out, err)
		}
	}

	defer func() error {
		if FSType(fstype) == LVM {
			// deactivate the logical volume
			out, err := processWrapper("lvchange", "-an", fmt.Sprintf("%s/%s", volumeGroup, logicalVolume))
			fmt.Println(out)
			if err != nil {
				log.Error("logical volume deactivation failed: %v", err)
			}

			// deactivate the volume group
			out, err = processWrapper("vgchange", "-an", volumeGroup)
			fmt.Println(out)
			if err != nil {
				log.Error("volume group deactivation failed: %v", err)
			}
		} else if FSType(fstype) == ZFS {
			if _, err := processWrapper("zpool", "export", "-f", zpool); err != nil {
				return fmt.Errorf("There was an error while exporting ZFS pool: %v", err)
			}

			dir, err := ioutil.ReadDir(mntDir)

			if err == nil {
				for _, d := range dir {
					os.RemoveAll(path.Join([]string{mntDir, d.Name()}...))
				}
			} else {
				return fmt.Errorf("Could not erase zfs contents left behind: %v", err)
			}
		}

		return nil
	}()

	// unmount the image from the temporary mount point
	defer func() {
		if FSType(fstype) != ZFS {
			fmt.Println("Unmounting Image")

			if err := syscall.Unmount(mntDir, 0); err != nil {
				log.Error("unmount failed: %v", err)
			}
		}
	}()

	// copy files/folders into mntDir
	for dst, src := range pairs {
		dir := filepath.Dir(filepath.Join(mntDir, dst))
		os.MkdirAll(dir, 0775)

		out, err := processWrapper("cp", "-fr", src, filepath.Join(mntDir, dst))
		if err != nil {
			return fmt.Errorf("[image %s] %v: %v", dst, out, err)
		}
	}

	// explicitly flush buffers
	out, err := processWrapper("blockdev", "--flushbufs", devPath)
	if err != nil {
		return fmt.Errorf("[image %s] unable to flush: %v %v", dst, out, err)
	}

	return nil
}

// parseInjectPairs parses a list of strings containing src:dst pairs into a
// map of where the dst is the key and src is the value. We build the map this
// way so that one source file can be written to multiple destinations and so
// that we can detect and return an error if the user tries to inject two files
// with the same destination.
func parseInjectPairs(files []string) (map[string]string, error) {
	pairs := map[string]string{}

	// parse inject pairs
	for _, arg := range files {
		parts := strings.Split(arg, ":")
		if len(parts) != 2 {
			return nil, errors.New("malformed command; expected src:dst pairs")
		}

		if pairs[parts[1]] != "" {
			return nil, fmt.Errorf("destination appears twice: `%v`", parts[1])
		}

		pairs[parts[1]] = parts[0]
		log.Debug("inject pair: %v, %v", parts[0], parts[1])
	}

	return pairs, nil
}

func cliDisk(ns *Namespace, c *minicli.Command, resp *minicli.Response) error {
	image := filepath.Clean(c.StringArgs["image"])
	fstype := c.StringArgs["fstype"]

	// Ensure that relative paths are always relative to /files/
	if !filepath.IsAbs(image) {
		image = path.Join(*f_iomBase, image)
	}
	log.Debug("image: %v", image)

	if c.BoolArgs["snapshot"] {
		dst := c.StringArgs["dst"]

		if dst == "" {
			f, err := ioutil.TempFile(*f_iomBase, "snapshot")
			if err != nil {
				return errors.New("could not create a dst image")
			}

			dst = f.Name()
			resp.Response = dst
		} else if strings.Contains(dst, "/") {
			return errors.New("dst image must filename without path")
		} else {
			dst = path.Join(*f_iomBase, dst)
		}

		log.Debug("destination image: %v", dst)

		return diskSnapshot(image, dst)
	} else if c.BoolArgs["inject"] {
		var partition string

		if strings.Contains(image, ":") {
			parts := strings.Split(image, ":")
			if len(parts) > 3 {
				return errors.New("found way too many ':'s, expected <path/to/image>:<partition> or <volume group>:<logical volume> or <partition>:<zpool name>")
			}

			image, partition = parts[0], strings.Join(parts[1:], ":")
		}

		options := fieldsQuoteEscape("\"", c.StringArgs["options"])
		log.Debug("got options: %v", options)

		pairs, err := parseInjectPairs(c.ListArgs["files"])
		if err != nil {
			return err
		}

		return diskInject(image, partition, fstype, pairs, options)
	} else if c.BoolArgs["create"] {
		size := c.StringArgs["size"]

		format := "raw"
		if _, ok := c.BoolArgs["qcow2"]; ok {
			format = "qcow2"
		}

		return diskCreate(format, image, size)
	} else if c.BoolArgs["info"] {
		info, err := diskInfo(image)
		if err != nil {
			return err
		}

		resp.Header = []string{"image", "format", "virtualsize", "disksize", "backingfile"}
		resp.Tabular = append(resp.Tabular, []string{
			image, info.Format, info.VirtualSize, info.DiskSize, info.BackingFile,
		})

		return nil
	}

	return unreachable()
}
