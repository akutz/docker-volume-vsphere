#!/usr/bin/env python
# Copyright 2016 VMware, Inc. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
import logging
import os
import os.path
import atexit


import threadutils
import log_config
import vmdk_utils
import vmdk_ops

from pyVmomi import VmomiSupport, vim, vmodl

VM_POWERSTATE = 'runtime.powerState'
POWERSTATE_POWEROFF = 'poweredOff'

def start_vm_changelistener():
    """
    Listen to power state changes of VMs running on current host
    """
    si = vmdk_ops.get_si()
    pc = si.content.propertyCollector
    err_msg = create_vm_powerstate_filter(pc, si.content.rootFolder)
    if err_msg:
        return err_msg
    # Start a separate thread to listen to changes
    threadutils.start_new_thread(target=listen_vm_propertychange,
                                 args=(pc,),
                                 daemon=True)

def create_vm_powerstate_filter(pc, from_node):
    """
    Create a filter spec to list to VM power state changes
    """

    filterSpec = vmodl.query.PropertyCollector.FilterSpec()
    objSpec = vmodl.query.PropertyCollector.ObjectSpec(obj=from_node,
                                                       selectSet=vm_folder_traversal())
    filterSpec.objectSet.append(objSpec)
    # Add the property specs
    propSpec = vmodl.query.PropertyCollector.PropertySpec(type=vim.VirtualMachine, all=False)
    propSpec.pathSet.append(VM_POWERSTATE)
    filterSpec.propSet.append(propSpec)
    try:
        pcFilter = pc.CreateFilter(filterSpec, True)
        atexit.register(pcFilter.Destroy)
        return None
    except Exception as e:
        err_msg = "Problem creating PropertyCollector filter: {}".format(str(e))
        logging.error(err_msg)
        return err_msg

def listen_vm_propertychange(pc):
    logging.info("VMChangeListener thread started")
    version = ''
    while True:
        result = pc.WaitForUpdates(version)

        try:
            # process the updates result
            for filterSet in result.filterSet:
                for objectSet in filterSet.objectSet:
                    if objectSet.kind != 'modify':
                        continue
                    for change in objectSet.changeSet:
                        # if the event was powerOff for a VM, set the status of all
                        # docker volumes attached to the VM to be detached
                        if change.name != VM_POWERSTATE or change.val != POWERSTATE_POWEROFF:
                            continue

                        moref = getattr(objectSet, 'obj', None)
                        # Do we need to alert the admin? how?
                        if not moref:
                            logging.error("Could not retrieve the managed object")
                            continue

                        logging.info("VM poweroff change found for %s", moref.config.name)

                        set_device_detached(moref.config.hardware.device)
        except Exception as e:
            # Do we need to alert the admin? how?
            logging.error("VMChangeListener: error %s", str(e))

        version = result.version

    logging.info("VMChangeListener thread exiting")

def vm_folder_traversal():
    """
    Build the traversal spec for the property collector to traverse vmFolder
    """

    TraversalSpec = vmodl.query.PropertyCollector.TraversalSpec
    SelectionSpec = vmodl.query.PropertyCollector.SelectionSpec

    # Traversal through vmFolder branch
    dcToVmf = TraversalSpec(name='dcToVmf', type=vim.Datacenter, path='vmFolder', skip=False)
    dcToVmf.selectSet.append(SelectionSpec(name='visitFolders'))

    # Recurse through the folders
    visitFolders = TraversalSpec(name='visitFolders', type=vim.Folder, path='childEntity', skip=False)
    visitFolders.selectSet.extend((SelectionSpec(name='visitFolders'), SelectionSpec(name='dcToVmf'),))

    return SelectionSpec.Array((visitFolders, dcToVmf,))

def set_device_detached(device_list):
    """
    For all devices in device_list, if it is a DVS volume, set its status to detached in KV
    """

    for dev in device_list:
        # if it is a dvs managed volume, set its status as detached
        vmdk_path = vmdk_utils.find_dvs_volume(dev)
        if vmdk_path:
            logging.info("Setting detach status for %s", vmdk_path)
            vmdk_ops.setStatusDetached(vmdk_path)
