package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	mountutils "k8s.io/mount-utils"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"github.com/vultr/vultr-csi/internal/vultrdevice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	diskPath   = "/dev/disk/by-id"
	diskPrefix = "virtio-"

	mkDirMode = 0750

	maxVolumesPerNode = 11

	volumeModeFilesystem = "filesystem"
)

var _ csi.NodeServer = &VultrNodeServer{}

// VultrNodeServer type provides the VultrDriver
type VultrNodeServer struct {
	csi.UnimplementedNodeServer
	Driver *VultrDriver
}

// NewVultrNodeDriver provides a VultrNodeServer
func NewVultrNodeDriver(driver *VultrDriver) *VultrNodeServer {
	return &VultrNodeServer{Driver: driver}
}

// NodeStageVolume perpares the node for the new volume to be mounted. This is
// executed after the ControllerPublishVolume and before the NodePublishVolume.
func (n *VultrNodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) { //nolint:gocyclo,lll,funlen
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: Volume ID must be provided")
	}

	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: Staging Target Path must be provided")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: Volume Capability must be provided")
	}

	n.Driver.log.WithFields(logrus.Fields{
		"volume":   req.VolumeId,
		"target":   req.StagingTargetPath,
		"capacity": req.VolumeCapability,
	}).Info("NodeStageVolume: called")

	publishContext := req.GetPublishContext()
	mountVolName := publishContext["mount_vol_name"]
	storageType := publishContext["storage_type"]

	// A workaround default storage type of 'block'
	// Attach only gets new publish context when the volume is not already
	// attached to the node. That should only happen to existing block storage
	// volumes
	if storageType == "" {
		storageType = "block"
	}

	source := ""
	target := req.StagingTargetPath
	mountBlk := req.VolumeCapability.GetMount()
	options := mountBlk.MountFlags

	n.Driver.log.WithFields(logrus.Fields{
		"volume":   req.VolumeId,
		"target":   req.StagingTargetPath,
		"capacity": req.VolumeCapability,
	}).Infof("NodeStageVolume: creating directory target %s", target)

	if err := os.MkdirAll(target, mkDirMode); err != nil {
		return nil, status.Errorf(codes.Internal, "NodeStageVolume: cannot create directory target: %v", err.Error())
	}

	n.Driver.log.WithFields(logrus.Fields{
		"volume":   req.VolumeId,
		"target":   req.StagingTargetPath,
		"capacity": req.VolumeCapability,
	}).Infof("NodeStageVolume: directory created for target %s", target)

	if storageType == "block" {
		// check and create link for block device if it does not exist
		if err := vultrdevice.LinkBySerial(mountVolName); err != nil {
			return nil, status.Errorf(
				codes.Internal,
				"NodeStageVolume: device for block volume %q is not accesible with serial %q: %v",
				req.VolumeId,
				mountVolName,
				err,
			)
		}

		source = filepath.Join(diskPath, fmt.Sprintf("%s%s", diskPrefix, mountVolName))

		// check for existing mount/staging before attempting format and mount.
		// if already staged, the plugin must reply ok
		blockMountExists, err := n.Driver.mounter.IsMountPoint(req.StagingTargetPath)
		if err != nil {
			n.Driver.log.WithFields(logrus.Fields{
				"volume": req.VolumeId,
				"target": req.StagingTargetPath,
			}).Warnf("NodeStageVolume: error checking block device staging target path: %s", err.Error())
		}

		if blockMountExists {
			n.Driver.log.WithFields(logrus.Fields{
				"volume": req.VolumeId,
				"target": req.StagingTargetPath,
			}).Infof("NodeStageVolume: block device target path already exists and is mounted")

			deviceName, deviceRef, err := mountutils.GetDeviceNameFromMount(n.Driver.mounter.Interface, req.StagingTargetPath)
			if err != nil {
				n.Driver.log.WithFields(logrus.Fields{
					"volume": req.VolumeId,
					"target": req.StagingTargetPath,
				}).Warnf("NodeStageVolume: error checking existing block device mount: %s", err.Error())
			}

			n.Driver.log.WithFields(logrus.Fields{
				"volume":            req.VolumeId,
				"target":            req.StagingTargetPath,
				"device-name":       deviceName,
				"device-references": deviceRef,
			}).Infof("NodeStageVolume: block device existing mount details")

			if deviceName == source {
				n.Driver.log.WithFields(logrus.Fields{
					"volume": req.VolumeId,
					"target": req.StagingTargetPath,
				}).Info("NodeStageVolume: block device is already staged")
				return &csi.NodeStageVolumeResponse{}, nil
			}
		}

		n.Driver.log.WithFields(logrus.Fields{
			"volume":   req.VolumeId,
			"target":   req.StagingTargetPath,
			"capacity": req.VolumeCapability,
		}).Info("NodeStageVolume: attempting block format and mount")

		fsType := "ext4"
		if mountBlk.FsType != "" {
			fsType = mountBlk.FsType
		}

		if err := n.Driver.mounter.FormatAndMount(source, target, fsType, options); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		if _, err := os.Stat(source); err == nil {
			needResize, err := n.Driver.resizer.NeedResize(source, target)
			if err != nil {
				return nil, status.Errorf(
					codes.Internal,
					"NodeStageVolume: could not determine if block volume %q needs to be resized: %v",
					req.VolumeId,
					err,
				)
			}

			if needResize {
				n.Driver.log.WithFields(logrus.Fields{
					"volume":   req.VolumeId,
					"target":   req.StagingTargetPath,
					"capacity": req.VolumeCapability,
				}).Info("NodeStageVolume: resizing block volume")

				if _, err := n.Driver.resizer.Resize(source, target); err != nil {
					return nil, status.Errorf(codes.Internal, "NodeStageVolume: could not resize block volume %q:  %v", req.VolumeId, err)
				}
			}
		}
	} else if storageType == "vfs" {
		source = mountVolName

		n.Driver.log.WithFields(logrus.Fields{
			"volume": req.VolumeId,
			"target": req.StagingTargetPath,
		}).Info("NodeStageVolume: attempting vfs mount")

		// check for existing mount/staging before attempting vfs mount.
		// if already staged, the plugin must reply ok
		vfsMountExists, err := n.Driver.mounter.IsMountPoint(req.StagingTargetPath)
		if err != nil {
			n.Driver.log.WithFields(logrus.Fields{
				"volume": req.VolumeId,
				"target": req.StagingTargetPath,
			}).Warnf("NodeStageVolume: error checking vfs device staging target path: %s", err.Error())
		}

		if vfsMountExists {
			n.Driver.log.WithFields(logrus.Fields{
				"volume": req.VolumeId,
				"target": req.StagingTargetPath,
			}).Infof("NodeStageVolume: vfs device target path already exists and is mounted")

			deviceName, deviceRef, err := mountutils.GetDeviceNameFromMount(n.Driver.mounter.Interface, req.StagingTargetPath)
			if err != nil {
				n.Driver.log.WithFields(logrus.Fields{
					"volume": req.VolumeId,
					"target": req.StagingTargetPath,
				}).Warnf("NodeStageVolume: error checking existing vfs device mount: %s", err.Error())
			}

			n.Driver.log.WithFields(logrus.Fields{
				"volume":            req.VolumeId,
				"target":            req.StagingTargetPath,
				"device-name":       deviceName,
				"device-references": deviceRef,
			}).Infof("NodeStageVolume: vfs existing device mount details")

			if deviceName == source {
				n.Driver.log.WithFields(logrus.Fields{
					"volume": req.VolumeId,
					"target": req.StagingTargetPath,
				}).Info("NodeStageVolume: vfs device is already staged")
				return &csi.NodeStageVolumeResponse{}, nil
			}
		}

		if err := n.Driver.mounter.Mount(source, target, "virtiofs", nil); err != nil {
			return nil, status.Errorf(codes.Internal, "NodeStageVolume: could not mount vfs volume %q: %v", req.VolumeId, err)
		}
	} else {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"NodeStageVolume: invalid storage type context from controller: %v",
			storageType,
		)
	}

	n.Driver.log.Info("NodeStageVolume: volume staged")
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume provides the node volume unstage functionality
func (n *VultrNodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging Target Path must be provided")
	}

	n.Driver.log.WithFields(logrus.Fields{
		"volume-id":           req.VolumeId,
		"staging-target-path": req.StagingTargetPath,
	}).Info("NodeUnstageVolume: called")

	err := mountutils.CleanupMountPoint(req.StagingTargetPath, n.Driver.mounter, true)
	if err != nil {
		return nil, err
	}

	n.Driver.log.Info("NodeUnstageVolume: volume unstaged")
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume allows the volume publish
func (n *VultrNodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) { //nolint:lll
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging Target Path must be provided")
	}

	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target Path must be provided")
	}

	log := n.Driver.log.WithFields(logrus.Fields{
		"volume_id":           req.VolumeId,
		"staging_target_path": req.StagingTargetPath,
		"target_path":         req.TargetPath,
	})
	log.Info("NodePublishVolume: called")

	options := []string{"bind"}
	if req.Readonly {
		options = append(options, "ro")
	}

	mnt := req.VolumeCapability.GetMount()
	options = append(options, mnt.MountFlags...)

	fsType := "ext4"
	if mnt.FsType != "" {
		fsType = mnt.FsType
	}

	err := os.MkdirAll(req.TargetPath, mkDirMode)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	err = n.Driver.mounter.Mount(req.StagingTargetPath, req.TargetPath, fsType, options)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	n.Driver.log.Info("NodePublishVolume: published")
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume allows the volume to be unpublished
func (n *VultrNodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: volume ID must be provided")
	}

	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: target path must be provided")
	}

	n.Driver.log.WithFields(logrus.Fields{
		"volume-id":   req.VolumeId,
		"target-path": req.TargetPath,
	}).Info("NodeUnpublishVolume: called")

	err := mountutils.CleanupMountPoint(req.TargetPath, n.Driver.mounter, true)
	if err != nil {
		return nil, err
	}

	n.Driver.log.Info("NodeUnpublishVolume: unpublished")
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats provides the volume stats
func (n *VultrNodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) { //nolint:lll
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume ID must be provided")
	}

	volumePath := req.VolumePath
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats: volume path must be provided")
	}

	log := n.Driver.log.WithFields(logrus.Fields{
		"volume_id":   req.VolumeId,
		"volume_path": req.VolumePath,
		"method":      "node_get_volume_stats",
	})
	log.Info("NodeGetVolumeStats: called")

	statfs := &unix.Statfs_t{}
	err := unix.Statfs(volumePath, statfs)
	if err != nil {
		return nil, err
	}

	availableBytes := int64(statfs.Bavail) * int64(statfs.Bsize)                    //nolint:unconvert // 32bit builds fail otherwise
	usedBytes := (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize) //nolint:unconvert // 32bit builds fail otherwise
	totalBytes := int64(statfs.Blocks) * int64(statfs.Bsize)                        //nolint:unconvert // 32bit builds fail otherwise
	totalInodes := int64(statfs.Files)
	availableInodes := int64(statfs.Ffree)
	usedInodes := totalInodes - availableInodes

	log.WithFields(logrus.Fields{
		"volume_mode":      volumeModeFilesystem,
		"bytes_available":  availableBytes,
		"bytes_total":      totalBytes,
		"bytes_used":       usedBytes,
		"inodes_available": availableInodes,
		"inodes_total":     totalInodes,
		"inodes_used":      usedInodes,
	}).Info("NodeGetVolumeStats: node capacity statistics retrieved")

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: availableBytes,
				Total:     totalBytes,
				Used:      usedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: availableInodes,
				Total:     totalInodes,
				Used:      usedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

// NodeExpandVolume provides the node volume expansion
func (n *VultrNodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume ID must be provided")
	}

	if req.VolumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeExpandVolume: volume path must be provided")
	}

	n.Driver.log.Logger.WithFields(logrus.Fields{
		"volume_id":      req.VolumeId,
		"volume_path":    req.VolumePath,
		"required_bytes": req.CapacityRange.RequiredBytes,
	}).Info("NodeExpandVolume: called")

	devicePath, _, err := mountutils.GetDeviceNameFromMount(mountutils.New(""), req.VolumePath)
	if err != nil {
		return nil, fmt.Errorf("NodeExpandVolume: failed to determine mount path for %s: %s", req.VolumePath, err)
	}

	n.Driver.log.Logger.WithFields(logrus.Fields{
		"volume_id":      req.VolumeId,
		"volume_path":    req.VolumePath,
		"required_bytes": req.CapacityRange.RequiredBytes,
	}).Infof("NodeExpandVolume: attempting to resize devicepath: %s", devicePath)

	if _, err := n.Driver.resizer.Resize(devicePath, req.VolumePath); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("NodeExpandVolume: failed to resize volume: %s", err))
	}

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: req.CapacityRange.RequiredBytes,
	}, nil
}

// NodeGetCapabilities provides the node capabilities
func (n *VultrNodeServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	nodeCapabilities := []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
		},
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
				},
			},
		},
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
				},
			},
		},
	}

	n.Driver.log.WithFields(logrus.Fields{
		"capabilities": nodeCapabilities,
	}).Info("NodeGetCapabilities: called")

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: nodeCapabilities,
	}, nil
}

// NodeGetInfo provides the node info
func (n *VultrNodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	n.Driver.log.WithFields(logrus.Fields{}).Info("NodeGetInfo: called")

	return &csi.NodeGetInfoResponse{
		NodeId:            n.Driver.nodeID,
		MaxVolumesPerNode: maxVolumesPerNode,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				"region": n.Driver.region,
			},
		},
	}, nil
}
