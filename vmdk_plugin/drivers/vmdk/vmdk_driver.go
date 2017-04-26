// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vmdk

//
// VMWare vSphere Docker Data Volume plugin.
//
// Provide support for --driver=vsphere in Docker, when Docker VM is running
// under ESX.
//
// Serves requests from Docker Engine related to VMDK volume operations.
// Depends on vmdk-opsd service to be running on hosting ESX
// (see ./esx_service)
///

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/go-plugins-helpers/volume"
	"github.com/vmware/docker-volume-vsphere/vmci"
	"github.com/vmware/docker-volume-vsphere/vmdk_plugin/drivers/vmdk/vmdkops"
	"github.com/vmware/docker-volume-vsphere/vmdk_plugin/utils/fs"
	"github.com/vmware/docker-volume-vsphere/vmdk_plugin/utils/refcount"
)

const (
	devWaitTimeout   = 1 * time.Second
	sleepBeforeMount = 1 * time.Second
	watchPath        = "/dev/disk/by-path"
	version          = "vSphere Volume Driver v0.4"
)

// VolumeDriver - VMDK driver struct
type VolumeDriver struct {
	useMockEsx bool
	ops        vmdkops.VmdkOps
	refCounts  *refcount.RefCountsMap
}

var mountRoot string

// NewVolumeDriver instantiates and returns a new VolumeDriver object.
//
// The flag useMockESX indicates whether or not to use a mock driver.
func NewVolumeDriver(
	port int, useMockEsx bool, mountDir, driverName string) *VolumeDriver {

	var d *VolumeDriver

	vmci.EsxPort = port
	mountRoot = mountDir

	if useMockEsx {
		d = &VolumeDriver{
			useMockEsx: true,
			ops:        vmdkops.VmdkOps{Cmd: vmdkops.MockVmdkCmd{}},
			refCounts:  refcount.NewRefCountsMap(),
		}
	} else {
		d = &VolumeDriver{
			useMockEsx: false,
			ops: vmdkops.VmdkOps{
				Cmd: vmci.EsxVmdkCmd{
					Mtx: &sync.Mutex{},
				},
			},
			refCounts: refcount.NewRefCountsMap(),
		}
	}

	d.refCounts.Init(d, mountDir, driverName)
	log.WithFields(log.Fields{
		"version":  version,
		"port":     vmci.EsxPort,
		"mock_esx": useMockEsx,
	}).Info("Docker VMDK plugin started")

	return d
}

// getRefCount returns the number of references for the given volume.
func (d *VolumeDriver) getRefCount(vol string) uint {
	return d.refCounts.GetCount(vol)
}

// incrRefCount increments the reference count for the given volume.
func (d *VolumeDriver) incrRefCount(vol string) (refcnt uint) {
	defer func() {
		log.WithFields(log.Fields{
			"name":   vol,
			"refcnt": refcnt,
		}).Debug("incremented ref count")
	}()
	return d.refCounts.Incr(vol)
}

// decrRefCount decrements the reference count for the given volume.
func (d *VolumeDriver) decrRefCount(vol string) (refcnt uint, err error) {
	defer func() {
		if err != nil {
			log.WithField("name", vol).WithError(err).Error(
				"error decrementing ref count")
			return
		}
		log.WithFields(log.Fields{
			"name":   vol,
			"refcnt": refcnt,
		}).Debug("decremented ref count")
	}()
	return d.refCounts.Decr(vol)
}

// getMountPoint returns the mount point for the given volume.
func getMountPoint(volName string) string {
	return filepath.Join(mountRoot, volName)
}

// Get returns info about a single volume.
func (d *VolumeDriver) Get(r volume.Request) volume.Response {
	status, err := d.GetVolume(r.Name)
	if err != nil {
		return volume.Response{Err: err.Error()}
	}
	mountpoint := getMountPoint(r.Name)
	return volume.Response{Volume: &volume.Volume{Name: r.Name,
		Mountpoint: mountpoint,
		Status:     status}}
}

// List returns the volumes known to the driver.
func (d *VolumeDriver) List(r volume.Request) volume.Response {
	volumes, err := d.ops.List()
	if err != nil {
		return volume.Response{Err: err.Error()}
	}
	responseVolumes := make([]*volume.Volume, 0, len(volumes))
	for _, vol := range volumes {
		mountpoint := getMountPoint(vol.Name)
		responseVol := volume.Volume{Name: vol.Name, Mountpoint: mountpoint}
		responseVolumes = append(responseVolumes, &responseVol)
	}
	return volume.Response{Volumes: responseVolumes}
}

// GetVolume returns a volume's meta-data.
func (d *VolumeDriver) GetVolume(name string) (map[string]interface{}, error) {
	return d.ops.Get(name)
}

// MountVolume - Request attach and them mounts the volume.
// Actual mount - send attach to ESX and do the in-guest magic
// Returns mount point and  error (or nil)
func (d *VolumeDriver) MountVolume(
	name, fstype, id string,
	isReadOnly, skipAttach bool) (string, error) {

	mountpoint := getMountPoint(name)

	// First, make sure  that mountpoint exists.
	if err := fs.Mkdir(mountpoint); err != nil {
		log.WithFields(log.Fields{
			"name": name,
			"dir":  mountpoint,
		}).WithError(err).Error("Failed to make directory for volume mount")
		return mountpoint, err
	}

	watcher, skipInotify := fs.DevAttachWaitPrep(name, watchPath)

	// Have ESX attach the disk
	dev, err := d.ops.Attach(name, nil)
	if err != nil {
		return mountpoint, err
	}

	if d.useMockEsx {
		return mountpoint, fs.Mount(mountpoint, fstype, string(dev[:]), false)
	}

	device, err := fs.GetDevicePath(dev)
	if err != nil {
		return mountpoint, err
	}

	if skipInotify {
		time.Sleep(sleepBeforeMount)
		return mountpoint, fs.Mount(mountpoint, fstype, device, false)
	}

	fs.DevAttachWait(watcher, name, device)

	// May have timed out waiting for the attach to complete,
	// attempt the mount anyway.
	return mountpoint, fs.Mount(mountpoint, fstype, device, isReadOnly)
}

// UnmountVolume unmounts the volume then submits a detach request.
func (d *VolumeDriver) UnmountVolume(name string) error {
	mountpoint := getMountPoint(name)
	if err := fs.Unmount(mountpoint); err != nil {
		log.WithField("mountpoint", mountpoint).WithError(err).Error(
			"Failed to unmount volume. Now trying to detach...")
		// Do not return error. Continue with detach.
	}
	return d.ops.Detach(name, nil)
}

// No need to actually manifest the volume on the filesystem yet
// (until Mount is called).
// Name and driver specific options passed through to the ESX host

// Create submits a volume creation request.
func (d *VolumeDriver) Create(r volume.Request) volume.Response {

	if r.Options == nil {
		r.Options = make(map[string]string)
	}

	// If cloning a existent volume, create and return
	if _, ok := r.Options["clone-from"]; ok {
		if err := d.ops.Create(r.Name, r.Options); err != nil {
			log.WithField("name", r.Name).WithError(err).Error(
				"Clone volume failed")
			return volume.Response{Err: err.Error()}
		}
		return volume.Response{Err: ""}
	}

	// Use default fstype if not specified
	if _, ok := r.Options["fstype"]; !ok {
		r.Options["fstype"] = fs.FstypeDefault
	}

	// Get existent filesystem tools
	supportedFs := fs.MkfsLookup()

	// Verify the existence of fstype mkfs
	mkfscmd, ok := supportedFs[r.Options["fstype"]]
	if !ok {
		buf := &bytes.Buffer{}
		fmt.Fprintf(buf, "Not found mkfs for %s\n", r.Options["fstype"])
		fmt.Fprint(buf, "Supported filesystems found: ")
		lenSupportedFS := len(supportedFs)
		x := 0
		for fs := range supportedFs {
			fmt.Fprint(buf, fs)
			if x < lenSupportedFS-1 {
				fmt.Fprint(buf, ", ")
			}
			x++
		}
		log.WithFields(log.Fields{
			"name":   r.Name,
			"fstype": r.Options["fstype"],
		}).Error("Not found")
		return volume.Response{Err: buf.String()}
	}

	if err := d.ops.Create(r.Name, r.Options); err != nil {
		log.WithField("name", r.Name).WithError(err).Error(
			"Create volume failed")
		return volume.Response{Err: err.Error()}
	}

	// Handle filesystem creation
	log.WithFields(log.Fields{
		"name":   r.Name,
		"fstype": r.Options["fstype"],
	}).Info("Attaching volume and creating filesystem")

	watcher, skipInotify := fs.DevAttachWaitPrep(r.Name, watchPath)

	dev, errAttach := d.ops.Attach(r.Name, nil)
	if errAttach != nil {
		log.WithField("name", r.Name).WithError(errAttach).Error(
			"Attach volume failed; removing the volume")
		// An internal error for the attach may have the volume attached to
		// this client, detach before removing below.
		d.ops.Detach(r.Name, nil)
		if err := d.ops.Remove(r.Name, nil); err != nil {
			log.WithField("name", r.Name).WithError(err).Warning(
				"Remove volume failed")
		}
		return volume.Response{Err: errAttach.Error()}
	}

	device, errGetDevicePath := fs.GetDevicePath(dev)
	if errGetDevicePath != nil {
		log.WithField("name", r.Name).WithError(errGetDevicePath).Error(
			"Could not find attached device; removing the volume")
		if err := d.ops.Detach(r.Name, nil); err != nil {
			log.WithField("name", r.Name).WithError(err).Warn(
				"Detach volume failed")
		}
		if err := d.ops.Remove(r.Name, nil); err != nil {
			log.WithField("name", r.Name).WithError(err).Warn(
				"Remove volume failed")
		}
		return volume.Response{Err: errGetDevicePath.Error()}
	}

	if skipInotify {
		time.Sleep(sleepBeforeMount)
	} else {
		// Wait for the attach to complete, may timeout
		// in which case we continue creating the file system.
		fs.DevAttachWait(watcher, r.Name, device)
	}
	if err := fs.Mkfs(mkfscmd, r.Name, device); err != nil {
		log.WithField("name", r.Name).WithError(err).Error(
			"Create filesystem failed, removing the volume")
		if err := d.ops.Detach(r.Name, nil); err != nil {
			log.WithField("name", r.Name).WithError(err).Warn(
				"Detach volume failed")
		}
		if err := d.ops.Remove(r.Name, nil); err != nil {
			log.WithField("name", r.Name).WithError(err).Warn(
				"Remove volume failed")
		}
		return volume.Response{Err: err.Error()}
	}

	if err := d.ops.Detach(r.Name, nil); err != nil {
		log.WithField("name", r.Name).WithError(err).Error(
			"Detach volume failed")
		return volume.Response{Err: err.Error()}
	}

	log.WithFields(log.Fields{
		"name":   r.Name,
		"fstype": r.Options["fstype"],
	}).Info("Volume and filesystem created")
	return volume.Response{Err: ""}
}

// Remove - removes individual volume. Docker would call it only if is not
// using it anymore
func (d *VolumeDriver) Remove(r volume.Request) volume.Response {
	log.WithField("name", r.Name).Info("Removing volume")

	// Docker is supposed to block 'remove' command if the volume is used.
	// Verify.
	if refcnt := d.getRefCount(r.Name); refcnt != 0 {
		log.WithFields(log.Fields{
			"name":   r.Name,
			"refcnt": refcnt,
		}).Error("remove failure; volume is still mounted")
		msg := fmt.Sprintf("Remove failure - volume is still mounted. "+
			" volume=%s, refcount=%d", r.Name, refcnt)
		return volume.Response{Err: msg}
	}

	if err := d.ops.Remove(r.Name, r.Options); err != nil {
		log.WithField("name", r.Name).WithError(err).Error(
			"Failed to remove volume")
		return volume.Response{Err: err.Error()}
	}

	return volume.Response{Err: ""}
}

// Path - give docker a reminder of the volume mount path
func (d *VolumeDriver) Path(r volume.Request) volume.Response {
	return volume.Response{Mountpoint: getMountPoint(r.Name)}
}

// Mount - Provide a volume to docker container - called once per container
// start. We need to keep refcount and unmount on refcount drop to 0.
//
// The serialization of operations per volume is assured by the volume/store
// of the docker daemon.
//
// As long as the refCountsMap is protected is unnecessary to do any locking
// at this level during create/mount/umount/remove.
//
func (d *VolumeDriver) Mount(r volume.MountRequest) volume.Response {
	log.WithField("name", r.Name).Info("Mounting volume")

	// If the volume is already mounted , just increase the refcount.
	//
	// Note: We are deliberately incrementing refcount first, before trying
	// to do anything else. If Mount fails, Docker will send Unmount request,
	// and we will happily decrement the refcount there, and will fail the
	// unmount since the volume will have been never mounted.
	//
	// Note: for new keys, GO maps return zero value, so no need for if_exists.

	if refcnt := d.incrRefCount(r.Name); refcnt > 1 { // save map traversal
		log.WithFields(log.Fields{
			"name":   r.Name,
			"refcnt": refcnt,
		}).Debug("already mounted; skipping mount")
		return volume.Response{Mountpoint: getMountPoint(r.Name)}
	}

	// This is the first time we are asked to mount the volume, so comply
	status, err := d.ops.Get(r.Name)
	if err != nil {
		d.decrRefCount(r.Name)
		return volume.Response{Err: err.Error()}
	}

	var (
		ok         bool
		value      string
		isReadOnly bool

		fstype = fs.FstypeDefault
	)

	// Check access type.
	if value, ok = status["access"].(string); !ok {
		log.WithField("name", r.Name).Error(
			"invalid access type; assuming RW access")
		isReadOnly = false
	} else if value == "read-only" {
		isReadOnly = true
	}

	// Check file system type.
	if value, ok = status["fstype"].(string); !ok {
		log.WithFields(log.Fields{
			"name":        r.Name,
			"assumedType": fstype,
		}).Error("invalid FS type; using assumed type")
		// Fail back to a default version that we can try with.
		value = fs.FstypeDefault
	}

	fstype = value

	mountpoint, err := d.MountVolume(r.Name, fstype, "", isReadOnly, false)
	if err != nil {
		log.WithField("name", r.Name).WithError(err).Error("failed to mount")

		if refcnt, _ := d.decrRefCount(r.Name); refcnt == 0 {
			log.WithField("name", r.Name).Info("detaching unused volume")

			// try to detach before failing the request for volume
			d.ops.Detach(r.Name, nil)
		}
		return volume.Response{Err: err.Error()}
	}

	return volume.Response{Mountpoint: mountpoint}
}

// Unmount request from Docker. If mount refcount is drop to 0.
// Unmount and detach from VM
func (d *VolumeDriver) Unmount(r volume.UnmountRequest) volume.Response {
	log.WithField("name", r.Name).Info("Unmounting Volume")

	// if the volume is still used by other containers, just return OK
	refcnt, err := d.decrRefCount(r.Name)
	if err != nil {
		// something went wrong - yell, but still try to unmount
		log.WithFields(log.Fields{
			"name":     r.Name,
			"refcount": refcnt,
		}).Error("Refcount error - still trying to unmount...")
	}

	if refcnt >= 1 {
		log.WithFields(log.Fields{
			"name":     r.Name,
			"refcount": refcnt,
		}).Debug("volume still in used; skipping unmount request")
		return volume.Response{Err: ""}
	}

	// and if nobody needs it, unmount and detach
	if err := d.UnmountVolume(r.Name); err != nil {
		log.WithField("name", r.Name).WithError(err).Error("failed to mount")
		return volume.Response{Err: err.Error()}
	}
	return volume.Response{Err: ""}
}

// Capabilities - Report plugin scope to Docker
func (d *VolumeDriver) Capabilities(r volume.Request) volume.Response {
	return volume.Response{Capabilities: volume.Capability{Scope: "global"}}
}
