/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vanilla

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/units"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/apis/migration"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/node"
	cnsvolume "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/cns-lib/vsphere"
	cnsconfig "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/config"
	csifault "sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/fault"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/prometheus"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/common/utils"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common/commonco"
	commoncotypes "sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/common/commonco/types"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v2/pkg/csi/types"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/internalapis/cnsvolumeinfo"
	"sigs.k8s.io/vsphere-csi-driver/v2/pkg/internalapis/cnsvolumeoperationrequest"
)

// NodeManagerInterface provides functionality to manage (VM) nodes.
type NodeManagerInterface interface {
	Initialize(ctx context.Context, useNodeUuid bool) error
	GetSharedDatastoresInK8SCluster(ctx context.Context) ([]*cnsvsphere.DatastoreInfo, error)
	GetSharedDatastoresInTopology(ctx context.Context, topologyRequirement *csi.TopologyRequirement,
		tagManager *tags.Manager, zoneKey string, regionKey string) ([]*cnsvsphere.DatastoreInfo,
		map[string][]map[string]string, error)
	GetNodeByName(ctx context.Context, nodeName string) (*cnsvsphere.VirtualMachine, error)
	GetNodeNameByUUID(ctx context.Context, nodeUUID string) (string, error)
	GetNodeByUuid(ctx context.Context, nodeUuid string) (*cnsvsphere.VirtualMachine, error)
	GetAllNodes(ctx context.Context) ([]*cnsvsphere.VirtualMachine, error)
}

type controller struct {
	// Deprecated
	// To be removed after multi vCenter support is added
	manager  *common.Manager
	managers *common.Managers
	nodeMgr  NodeManagerInterface
	// Deprecated
	// To be removed after multi vCenter support is added
	authMgr     common.AuthorizationService
	authMgrs    map[string]*common.AuthManager
	topologyMgr commoncotypes.ControllerTopologyService
}

var (
	// volumeMigrationService holds the pointer to VolumeMigration instance.
	volumeMigrationService migration.VolumeMigrationService

	// volumeInfoService holds the pointer to VolumeInfo service instance
	// This will hold mapping for VolumeID to vCenter for multi vCenter CSI topology deployment
	volumeInfoService cnsvolumeinfo.VolumeInfoService

	// multivCenterCSITopologyEnabled holds the feature gate status for
	// multi-vcenter-csi-topology feature
	multivCenterCSITopologyEnabled bool

	// variables for list volumes
	volIDsInK8s                             = make([]string, 0)
	cnsQueryResult *cnstypes.CnsQueryResult = nil
)

// New creates a CNS controller.
func New() csitypes.CnsController {
	return &controller{}
}

// Init is initializing controller struct.
func (c *controller) Init(config *cnsconfig.Config, version string) error {
	ctx, log := logger.GetNewContextWithLogger()
	log.Infof("Initializing CNS controller")
	var err error
	var operationStore cnsvolumeoperationrequest.VolumeOperationRequest
	operationStore, err = cnsvolumeoperationrequest.InitVolumeOperationRequestInterface(ctx,
		config.Global.CnsVolumeOperationRequestCleanupIntervalInMin,
		func() bool {
			return commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot)
		})
	if err != nil {
		log.Errorf("failed to initialize VolumeOperationRequestInterface with error: %v", err)
		return err
	}
	multivCenterCSITopologyEnabled = commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx,
		common.MultiVCenterCSITopology)
	isAuthCheckFSSEnabled := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIAuthCheck)

	vcManager := cnsvsphere.GetVirtualCenterManager(ctx)
	if !multivCenterCSITopologyEnabled {
		// Get VirtualCenterManager instance and validate version.
		vcenterconfig, err := cnsvsphere.GetVirtualCenterConfig(ctx, config)
		if err != nil {
			log.Errorf("failed to get VirtualCenterConfig. err=%v", err)
			return err
		}
		vcenter, err := vcManager.RegisterVirtualCenter(ctx, vcenterconfig)
		if err != nil {
			log.Errorf("failed to register VC %q with virtualCenterManager. err=%v", vcenterconfig.Host, err)
			return err
		}
		c.manager = &common.Manager{
			VcenterConfig:  vcenterconfig,
			CnsConfig:      config,
			VolumeManager:  cnsvolume.GetManager(ctx, vcenter, operationStore, true, false),
			VcenterManager: vcManager,
		}
		vc, err := common.GetVCenter(ctx, c.manager)
		if err != nil {
			log.Errorf("failed to get vcenter. err=%v", err)
			return err
		}
		// Check vCenter API Version
		err = common.CheckAPI(ctx, vc.Client.ServiceContent.About.ApiVersion, common.MinSupportedVCenterMajor,
			common.MinSupportedVCenterMinor, common.MinSupportedVCenterPatch)
		if err != nil {
			log.Errorf("checkAPI failed for vcenter API version: %s, err=%v",
				vc.Client.ServiceContent.About.ApiVersion, err)
			return err
		}
		// Check if vSAN FS is enabled for TargetvSANFileShareDatastoreURLs only if
		// CSIAuthCheck FSS is not enabled.
		if !isAuthCheckFSSEnabled && len(c.manager.VcenterConfig.TargetvSANFileShareDatastoreURLs) > 0 {
			datacenters, err := vc.ListDatacenters(ctx)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to find datacenters from VC: %q, Error: %+v", vc.Config.Host, err)
			}
			// Check if file service is enabled on datastore present in
			// targetvSANFileShareDatastoreURLs.
			dsToFileServiceEnabledMap, err := common.IsFileServiceEnabled(ctx,
				c.manager.VcenterConfig.TargetvSANFileShareDatastoreURLs, vc, datacenters)
			if err != nil {
				return logger.LogNewErrorf(log, "file service enablement check failed for datastore specified in "+
					"TargetvSANFileShareDatastoreURLs. err=%v", err)
			}
			for _, targetFSDatastore := range c.manager.VcenterConfig.TargetvSANFileShareDatastoreURLs {
				isFSEnabled := dsToFileServiceEnabledMap[targetFSDatastore]
				if !isFSEnabled {
					return logger.LogNewErrorf(log, "file service is not enabled on datastore %s specified in "+
						"TargetvSANFileShareDatastoreURLs", targetFSDatastore)
				}
			}
		}
		if isAuthCheckFSSEnabled {
			log.Info("CSIAuthCheck feature is enabled, loading AuthorizationService")
			authMgr, err := common.GetAuthorizationService(ctx, vc)
			if err != nil {
				log.Errorf("failed to initialize authMgr. err=%v", err)
				return err
			}
			c.authMgr = authMgr
			go common.ComputeDatastoreMapForBlockVolumes(authMgr.(*common.AuthManager),
				config.Global.CSIAuthCheckIntervalInMin)
			isvSANFileServicesSupported, err := c.manager.VcenterManager.IsvSANFileServicesSupported(ctx,
				c.manager.VcenterConfig.Host)
			if err != nil {
				log.Errorf("failed to verify if vSAN file services is supported or not. Error:%+v", err)
				return err
			}
			if isvSANFileServicesSupported {
				go common.ComputeFSEnabledClustersToDsMap(authMgr.(*common.AuthManager),
					config.Global.CSIAuthCheckIntervalInMin)
			}
		}
	} else {
		// Multi vCenter feature enabled
		c.managers = &common.Managers{
			CnsConfig:      config,
			VcenterManager: vcManager,
		}
		c.managers.VcenterConfigs = make(map[string]*cnsvsphere.VirtualCenterConfig)
		c.managers.VolumeManagers = make(map[string]cnsvolume.Manager)
		// Get VirtualCenterManager instance and validate version.
		vcenterconfigs, err := cnsvsphere.GetVirtualCenterConfigs(ctx, config)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to get VirtualCenterConfigs. err=%v", err)
		}
		for _, vcenterconfig := range vcenterconfigs {
			vcenter, err := vcManager.RegisterVirtualCenter(ctx, vcenterconfig)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to register VC %q with virtualCenterManager. "+
					"err=%v", vcenterconfig.Host, err)
			}
			c.managers.VcenterConfigs[vcenterconfig.Host] = vcenterconfig
			c.managers.VolumeManagers[vcenterconfig.Host] = cnsvolume.GetManager(ctx, vcenter, operationStore,
				true, true)
		}
		vCenters, err := common.GetVCenters(ctx, c.managers)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to get vcenters. err=%v", err)
		}
		// Check vCenter API Version
		for _, vc := range vCenters {
			err = common.CheckAPI(ctx, vc.Client.ServiceContent.About.ApiVersion, common.MinSupportedVCenterMajor,
				common.MinSupportedVCenterMinor, common.MinSupportedVCenterPatch)
			if err != nil {
				return logger.LogNewErrorf(log, "checkAPI failed for vcenter API version: %s for vCenter %s, err=%v",
					vc.Client.ServiceContent.About.ApiVersion, vc.Config.Host, err)
			}
		}
		if isAuthCheckFSSEnabled {
			log.Info("CSIAuthCheck feature is enabled, loading AuthorizationService")
			authMgrs, err := common.GetAuthorizationServices(ctx, vCenters)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to initialize authMgr. err=%v", err)
			}
			c.authMgrs = authMgrs
			for _, authMgr := range authMgrs {
				go common.ComputeDatastoreMapForBlockVolumes(authMgr, config.Global.CSIAuthCheckIntervalInMin)
			}
			for _, vcconfig := range c.managers.VcenterConfigs {
				isvSANFileServicesSupported, err := c.managers.VcenterManager.IsvSANFileServicesSupported(ctx,
					vcconfig.Host)
				if err != nil {
					return logger.LogNewErrorf(log, "failed to verify if vSAN file services is supported or not for vCenter: %s. "+
						"Error:%+v", vcconfig.Host, err)
				}
				if isvSANFileServicesSupported {
					for _, authMgr := range authMgrs {
						go common.ComputeFSEnabledClustersToDsMap(authMgr, config.Global.CSIAuthCheckIntervalInMin)
					}
				}
			}
		}
		if len(c.managers.VcenterConfigs) > 1 {
			log.Info("Loading CnsVolumeInfo Service to persist mapping for VolumeID to vCenter")
			volumeInfoService, err = cnsvolumeinfo.InitVolumeInfoService(ctx)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to load volumeInfoService service. Err: %v", err)
			}
			if volumeInfoService != nil {
				log.Infof("Successfully initialized VolumeInfoService")
			}
		}
	}
	useNodeUuid := false
	if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.UseCSINodeId) {
		useNodeUuid = true
	}
	c.nodeMgr = &node.Nodes{}
	err = c.nodeMgr.Initialize(ctx, useNodeUuid)
	if err != nil {
		log.Errorf("failed to initialize nodeMgr. err=%v", err)
		return err
	}

	cfgPath := common.GetConfigPath(ctx)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Errorf("failed to create fsnotify watcher. err=%v", err)
		return err
	}
	go func() {
		for {
			log.Debugf("Waiting for event on fsnotify watcher")
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				log.Debugf("fsnotify event: %q", event.String())
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					for {
						reloadConfigErr := c.ReloadConfiguration()
						if reloadConfigErr == nil {
							log.Infof("Successfully reloaded configuration from: %q", cfgPath)
							break
						}
						log.Errorf("failed to reload configuration. will retry again in 5 seconds. err: %+v", reloadConfigErr)
						time.Sleep(5 * time.Second)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					log.Errorf("fsnotify error: %+v", err)
					return
				}
			}
			log.Debugf("fsnotify event processed")
		}
	}()
	cfgDirPath := filepath.Dir(cfgPath)
	log.Infof("Adding watch on path: %q", cfgDirPath)
	err = watcher.Add(cfgDirPath)
	if err != nil {
		log.Errorf("failed to watch on path: %q. err=%v", cfgDirPath, err)
		return err
	}
	if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration) {
		if !multivCenterCSITopologyEnabled {
			log.Info("CSI Migration Feature is Enabled. Loading Volume Migration Service")
			volumeMigrationService, err = migration.GetVolumeMigrationService(ctx, &c.manager.VolumeManager, config, false)
			if err != nil {
				log.Errorf("failed to get migration service. Err: %v", err)
				return err
			}
		} else {
			if len(c.managers.VcenterConfigs) == 1 {
				log.Info("CSI Migration Feature is Enabled. Loading Volume Migration Service")
				volumeManager := c.managers.VolumeManagers[c.managers.CnsConfig.Global.VCenterIP]
				volumeMigrationService, err = migration.GetVolumeMigrationService(ctx, &volumeManager, config, false)
				if err != nil {
					log.Errorf("failed to get migration service. Err: %v", err)
					return err
				}
			} else {
				log.Infof("vSphere CSI Migration is not supported on the multi vCenter setup")
			}
		}
	}

	// Create dynamic informer for CSINodeTopology instance if FSS is enabled.
	// Initialize volume topology service.
	c.topologyMgr, err = commonco.ContainerOrchestratorUtility.InitTopologyServiceInController(ctx)
	if err != nil {
		log.Errorf("failed to initialize topology service. Error: %+v", err)
		return err
	}

	// Go module to keep the metrics http server running all the time.
	go func() {
		prometheus.CsiInfo.WithLabelValues(version).Set(1)
		for {
			log.Info("Starting the http server to expose Prometheus metrics..")
			http.Handle("/metrics", promhttp.Handler())
			err = http.ListenAndServe(":2112", nil)
			if err != nil {
				log.Warnf("Http server that exposes the Prometheus exited with err: %+v", err)
			}
			log.Info("Restarting http server to expose Prometheus metrics..")
		}
	}()
	return nil
}

// ReloadConfiguration reloads configuration from the secret, and update
// controller's config cache and VolumeManager's VC Config cache.
func (c *controller) ReloadConfiguration() error {
	ctx, log := logger.GetNewContextWithLogger()
	log.Info("Reloading Configuration")
	cfg, err := common.GetConfig(ctx)
	if err != nil {
		return logger.LogNewErrorf(log, "failed to read config. Error: %+v", err)
	}
	newVCConfig, err := cnsvsphere.GetVirtualCenterConfig(ctx, cfg)
	if err != nil {
		log.Errorf("failed to get VirtualCenterConfig. err=%v", err)
		return err
	}
	if newVCConfig != nil {
		var vcenter *cnsvsphere.VirtualCenter
		if c.manager.VcenterConfig.Host != newVCConfig.Host ||
			c.manager.VcenterConfig.Username != newVCConfig.Username ||
			c.manager.VcenterConfig.Password != newVCConfig.Password {

			// Verify if new configuration has valid credentials by connecting to
			// vCenter. Proceed only if the connection succeeds, else return error.
			newVC := &cnsvsphere.VirtualCenter{Config: newVCConfig}
			if err = newVC.Connect(ctx); err != nil {
				return logger.LogNewErrorf(log, "failed to connect to VirtualCenter host: %q, Err: %+v",
					newVCConfig.Host, err)
			}

			// Reset vCenter singleton instance by passing reload flag as true.
			log.Info("Obtaining new vCenterInstance using new credentials")
			vcenter, err = cnsvsphere.GetVirtualCenterInstance(ctx, &cnsconfig.ConfigurationInfo{Cfg: cfg}, true)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to get VirtualCenter. err=%v", err)
			}
		} else {
			// If it's not a VC host or VC credentials update, same singleton
			// instance can be used and it's Config field can be updated.
			vcenter, err = cnsvsphere.GetVirtualCenterInstance(ctx, &cnsconfig.ConfigurationInfo{Cfg: cfg}, false)
			if err != nil {
				return logger.LogNewErrorf(log, "failed to get VirtualCenter. err=%v", err)
			}
			vcenter.Config = newVCConfig
		}
		var operationStore cnsvolumeoperationrequest.VolumeOperationRequest
		operationStore, err = cnsvolumeoperationrequest.InitVolumeOperationRequestInterface(ctx,
			c.manager.CnsConfig.Global.CnsVolumeOperationRequestCleanupIntervalInMin,
			func() bool {
				return commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot)
			})
		if err != nil {
			log.Errorf("failed to initialize VolumeOperationRequestInterface with error: %v", err)
			return err
		}

		c.manager.VolumeManager.ResetManager(ctx, vcenter)
		c.manager.VcenterConfig = newVCConfig
		c.manager.VolumeManager = cnsvolume.GetManager(ctx, vcenter, operationStore, true, false)
		// Re-Initialize Node Manager to cache latest vCenter config.
		useNodeUuid := false
		if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.UseCSINodeId) {
			useNodeUuid = true
		}
		c.nodeMgr = &node.Nodes{}
		err = c.nodeMgr.Initialize(ctx, useNodeUuid)
		if err != nil {
			log.Errorf("failed to re-initialize nodeMgr. err=%v", err)
			return err
		}
		if c.authMgr != nil {
			c.authMgr.ResetvCenterInstance(ctx, vcenter)
			log.Debugf("Updated vCenter in auth manager")
		}
	}
	if cfg != nil {
		c.manager.CnsConfig = cfg
		log.Debugf("Updated manager.CnsConfig")
	}
	return nil
}

func (c *controller) filterDatastores(ctx context.Context,
	sharedDatastores []*cnsvsphere.DatastoreInfo) ([]*cnsvsphere.DatastoreInfo, error) {
	log := logger.GetLogger(ctx)
	dsMap := c.authMgr.GetDatastoreMapForBlockVolumes(ctx)
	if len(dsMap) == 0 {
		return nil, logger.LogNewError(log, "auth service: no shared datastore found for block volume provisioning")
	}
	log.Debugf("filterDatastores: dsMap %v sharedDatastores %v", dsMap, sharedDatastores)
	var filteredDatastores []*cnsvsphere.DatastoreInfo
	for _, sharedDatastore := range sharedDatastores {
		if _, existsInDsMap := dsMap[sharedDatastore.Info.Url]; existsInDsMap {
			filteredDatastores = append(filteredDatastores, sharedDatastore)
		} else {
			log.Debugf("filter out datastore %v from create volume spec", sharedDatastore)
		}
	}
	log.Debugf("filterDatastores: filteredDatastores %v", filteredDatastores)
	if len(filteredDatastores) == 0 {
		return nil, logger.LogNewError(log, "auth service could not find datastore for block volume provisioning")
	}
	return filteredDatastores, nil
}

// createBlockVolume creates a block volume based on the CreateVolumeRequest.
func (c *controller) createBlockVolume(ctx context.Context, req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, string, error) {
	log := logger.GetLogger(ctx)
	// Volume Size - Default is 10 GiB.
	volSizeBytes := int64(common.DefaultGbDiskSize * common.GbInBytes)
	if req.GetCapacityRange() != nil && req.GetCapacityRange().RequiredBytes != 0 {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	volSizeMB := int64(common.RoundUpSize(volSizeBytes, common.MbInBytes))

	// Check if the feature states are enabled.
	isBlockVolumeSnapshotEnabled := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot)
	filterSuspendedDatastores := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx,
		common.CnsMgrSuspendCreateVolume)
	csiMigrationFeatureState := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration)

	// Check if requested volume size and source snapshot size matches
	volumeSource := req.GetVolumeContentSource()
	var contentSourceSnapshotID string
	if isBlockVolumeSnapshotEnabled && volumeSource != nil {
		isCnsSnapshotSupported, err := c.manager.VcenterManager.IsCnsSnapshotSupported(ctx,
			c.manager.VcenterConfig.Host)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to check if cns snapshot operations are supported on VC due to error: %v", err)
		}
		if !isCnsSnapshotSupported {
			return nil, csifault.CSIUnimplementedFault, logger.LogNewErrorCode(log, codes.Unimplemented,
				"VC version does not support snapshot operations")
		}
		sourceSnapshot := volumeSource.GetSnapshot()
		if sourceSnapshot == nil {
			return nil, csifault.CSIInvalidArgumentFault,
				logger.LogNewErrorCode(log, codes.InvalidArgument, "unsupported VolumeContentSource type")
		}
		contentSourceSnapshotID = sourceSnapshot.GetSnapshotId()

		cnsVolumeID, _, err := common.ParseCSISnapshotID(contentSourceSnapshotID)
		if err != nil {
			return nil, csifault.CSIInvalidArgumentFault,
				logger.LogNewErrorCode(log, codes.InvalidArgument, err.Error())
		}
		// Query capacity in MB and datastore url for block volume snapshot
		volumeIds := []cnstypes.CnsVolumeId{{Id: cnsVolumeID}}
		cnsVolumeDetailsMap, err := utils.QueryVolumeDetailsUtil(ctx, c.manager.VolumeManager, volumeIds)
		if err != nil {
			log.Errorf("failed to retrieve the volume: %s details. err: %+v", cnsVolumeID, err)
			return nil, csifault.CSIInternalFault, err
		}
		if _, ok := cnsVolumeDetailsMap[cnsVolumeID]; !ok {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"cns query volume did not return the volume: %s", cnsVolumeID)
		}
		snapshotSizeInMB := cnsVolumeDetailsMap[cnsVolumeID].SizeInMB
		snapshotSizeInBytes := snapshotSizeInMB * common.MbInBytes
		if volSizeBytes != snapshotSizeInBytes {
			return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCodef(log, codes.InvalidArgument,
				"size mismatches, requested volume size %d and source snapshot size %d",
				volSizeBytes, snapshotSizeInBytes)
		}
	}

	// Fetching the feature state for csi-migration before parsing storage class
	// params.
	scParams, err := common.ParseStorageClassParams(ctx, req.Parameters, csiMigrationFeatureState)
	// TODO: Need to figure out the fault returned by ParseStorageClassParams.
	// Currently, just return "csi.fault.Internal".
	if err != nil {
		return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.InvalidArgument,
			"parsing storage class parameters failed with error: %+v", err)
	}
	if csiMigrationFeatureState && scParams.CSIMigration == "true" {
		if len(scParams.Datastore) != 0 {
			log.Infof("Converting datastore name: %q to Datastore URL", scParams.Datastore)
			// Get vCenter.
			// Need to extract fault from err returned by GetVirtualCenter.
			// Currently, just return "csi.fault.Internal".
			vCenter, err := cnsvsphere.GetVirtualCenterManager(ctx).GetVirtualCenter(ctx, c.manager.VcenterConfig.Host)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get vCenter. err: %+v", err)
			}
			dcList, err := vCenter.GetDatacenters(ctx)
			// Need to extract fault from err returned by GetDatacenters.
			// Currently, just return "csi.fault.Internal".
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get datacenter list. err: %+v", err)
			}
			foundDatastoreURL := false
			for _, dc := range dcList {
				dsURLTodsInfoMap, err := dc.GetAllDatastores(ctx)
				// Need to extract fault from err returned by GetAllDatastores.
				// Currently, just return "csi.fault.Internal".
				if err != nil {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to get dsURLTodsInfoMap. err: %+v", err)
				}
				for dsURL, dsInfo := range dsURLTodsInfoMap {
					if dsInfo.Info.Name == scParams.Datastore {
						scParams.DatastoreURL = dsURL
						log.Infof("Found datastoreURL: %q for datastore name: %q", scParams.DatastoreURL, scParams.Datastore)
						foundDatastoreURL = true
						break
					}
				}
				if foundDatastoreURL {
					break
				}
			}
			if !foundDatastoreURL {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to find datastoreURL for datastore name: %q", scParams.Datastore)
			}
		}
	}

	var createVolumeSpec = common.CreateVolumeSpec{
		CapacityMB:              volSizeMB,
		Name:                    req.Name,
		ScParams:                scParams,
		VolumeType:              common.BlockVolumeType,
		ContentSourceSnapshotID: contentSourceSnapshotID,
	}

	// Check if vCenter task for this volume is already registered as part of
	// improved idempotency CR
	log.Debugf("Checking if vCenter task for volume %s is already registered.", req.Name)
	var (
		volTaskAlreadyRegistered bool
		volumeInfo               *cnsvolume.CnsVolumeInfo
		faultType                string
		vcenter                  *cnsvsphere.VirtualCenter
	)
	// Get VirtualCenter instance
	vcenter, err = c.manager.VcenterManager.GetVirtualCenter(ctx, c.manager.VcenterConfig.Host)
	if err != nil {
		return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to get vCenter. Err: %v", err)
	}
	// Get operation store
	operationStore := c.manager.VolumeManager.GetOperationStore()
	if operationStore == nil {
		return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
			"Operation store cannot be nil")
	}

	volumeOperationDetails, err := operationStore.GetRequestDetails(ctx, req.Name)
	if err != nil {
		log.Debugf("CreateVolume task details for volume %s are not found, %+v",
			req.Name, err)
	} else if volumeOperationDetails.OperationDetails != nil {
		if volumeOperationDetails.OperationDetails.TaskStatus ==
			cnsvolumeoperationrequest.TaskInvocationStatusSuccess &&
			volumeOperationDetails.VolumeID != "" {
			// If task status is successful for this volume, then it means that volume is
			// already created and there is no need to create it again.
			log.Infof("Volume with name %q and id %q is already created on CNS with opId: %q.",
				req.Name, volumeOperationDetails.VolumeID, volumeOperationDetails.OperationDetails.OpID)

			volumeInfo = &cnsvolume.CnsVolumeInfo{
				DatastoreURL: "",
				VolumeID: cnstypes.CnsVolumeId{
					Id: volumeOperationDetails.VolumeID,
				},
			}
			volTaskAlreadyRegistered = true
		} else if volumeOperationDetails.OperationDetails.TaskStatus ==
			cnsvolumeoperationrequest.TaskInvocationStatusInProgress &&
			volumeOperationDetails.OperationDetails.TaskID != "" {
			// If task is created in CNS for this volume but task is in progress, then
			// we need to monitor the task to check if volume creation is completed or not.
			log.Infof("Volume with name %s has CreateVolume task %s pending on CNS.",
				req.Name, volumeOperationDetails.OperationDetails.TaskID)

			taskMoRef := types.ManagedObjectReference{
				Type:  "Task",
				Value: volumeOperationDetails.OperationDetails.TaskID,
			}
			task := object.NewTask(vcenter.Client.Client, taskMoRef)

			volumeInfo, faultType, err = c.manager.VolumeManager.MonitorCreateVolumeTask(ctx,
				&volumeOperationDetails, task, req.Name, c.manager.CnsConfig.Global.ClusterID)
			if err != nil {
				return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to monitor task for volume %s. Error: %+v", req.Name, err)
			}
			// Persist the operation details.
			if volumeOperationDetails != nil && volumeOperationDetails.OperationDetails != nil &&
				volumeOperationDetails.OperationDetails.TaskStatus !=
					cnsvolumeoperationrequest.TaskInvocationStatusInProgress {
				err := operationStore.StoreRequestDetails(ctx, volumeOperationDetails)
				if err != nil {
					log.Warnf("failed to store CreateVolume details with error: %v", err)
				}
			}

			volTaskAlreadyRegistered = true
		}
	}

	var (
		sharedDatastores    []*cnsvsphere.DatastoreInfo
		topologyRequirement *csi.TopologyRequirement
	)
	if !volTaskAlreadyRegistered {
		// Get accessibility.
		topologyRequirement = req.GetAccessibilityRequirements()
		if topologyRequirement != nil {
			// Check if topology domains have been provided in the vSphere CSI config secret.
			// NOTE: We do not support kubernetes.io/hostname as a topology label.
			if c.manager.CnsConfig.Labels.TopologyCategories == "" && c.manager.CnsConfig.Labels.Zone == "" &&
				c.manager.CnsConfig.Labels.Region == "" {
				return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCode(log, codes.InvalidArgument,
					"topology category names not specified in the vsphere config secret")
			}

			// Get shared accessible datastores for matching topology requirement.
			if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.TopologyPreferentialDatastores) {
				sharedDatastores, err = c.topologyMgr.GetSharedDatastoresInTopology(ctx,
					commoncotypes.VanillaTopologyFetchDSParams{
						TopologyRequirement: topologyRequirement,
						Vc:                  vcenter,
						StoragePolicyName:   scParams.StoragePolicyName,
					})
				if err != nil || len(sharedDatastores) == 0 {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to get shared datastores for topology requirement: %+v. Error: %+v",
						topologyRequirement, err)
				}
			} else {
				sharedDatastores, err = c.topologyMgr.GetSharedDatastoresInTopology(ctx,
					commoncotypes.VanillaTopologyFetchDSParams{TopologyRequirement: topologyRequirement})
				if err != nil || len(sharedDatastores) == 0 {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to get shared datastores for topology requirement: %+v. Error: %+v",
						topologyRequirement, err)
				}
			}
			log.Debugf("Shared datastores [%+v] retrieved for topologyRequirement [%+v]", sharedDatastores,
				topologyRequirement)
		} else {
			sharedDatastores, err = c.nodeMgr.GetSharedDatastoresInK8SCluster(ctx)
			if err != nil || len(sharedDatastores) == 0 {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get shared datastores in kubernetes cluster. Error: %+v", err)
			}
			if len(sharedDatastores) == 0 {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCode(log, codes.Internal,
					"No datastore found for volume provisioning.")
			}
		}

		if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIAuthCheck) {
			// Filter datastores which in datastoreMap from sharedDatastores.
			sharedDatastores, err = c.filterDatastores(ctx, sharedDatastores)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to create volume. Error: %+v", err)
			}
		}

		volumeInfo, faultType, err = common.CreateBlockVolumeUtil(ctx, cnstypes.CnsClusterFlavorVanilla,
			c.manager, &createVolumeSpec, sharedDatastores, filterSuspendedDatastores, false)
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to create volume. Error: %+v", err)
		}
	}

	attributes := make(map[string]string)
	attributes[common.AttributeDiskType] = common.DiskTypeBlockVolume
	if csiMigrationFeatureState && scParams.CSIMigration == "true" {
		// In case if feature state switch is enabled after controller is
		// deployed, we need to initialize the volumeMigrationService.
		if err := initVolumeMigrationService(ctx, c); err != nil {
			// Error is already wrapped in CSI error code.
			return nil, csifault.CSIInternalFault, err
		}
		// Return InitialVolumeFilepath in the response for TranslateCSIPVToInTree.
		volumePath, err := volumeMigrationService.GetVolumePath(ctx, volumeInfo.VolumeID.Id)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get volume path for volume id: %q. Error: %+v", volumeInfo.VolumeID.Id, err)
		}
		attributes[common.AttributeInitialVolumeFilepath] = volumePath
	}

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeInfo.VolumeID.Id,
			CapacityBytes: int64(units.FileSize(volSizeMB * common.MbInBytes)),
			VolumeContext: attributes,
		},
	}

	// For topology aware provisioning, populate the topology segments parameter
	// in the CreateVolumeResponse struct.
	if topologyRequirement != nil {
		var (
			datastoreAccessibleTopology []map[string]string
			allNodeVMs                  []*cnsvsphere.VirtualMachine
		)

		// Retrieve the datastoreURL of the Provisioned Volume. If CNS CreateVolume
		// API does not return datastoreURL, retrieve this by calling QueryVolume.
		// Otherwise, retrieve this from PlacementResults in the response of
		// CreateVolume API.
		datastoreURL := volumeInfo.DatastoreURL
		if datastoreURL == "" {
			volumeIds := []cnstypes.CnsVolumeId{{Id: volumeInfo.VolumeID.Id}}
			queryFilter := cnstypes.CnsQueryFilter{
				VolumeIds: volumeIds,
			}

			querySelection := cnstypes.CnsQuerySelection{
				Names: []string{string(cnstypes.QuerySelectionNameTypeDataStoreUrl)},
			}

			queryResult, err := utils.QueryVolumeUtil(ctx, c.manager.VolumeManager, queryFilter, &querySelection, true)
			if err != nil {
				// TODO: QueryVolume need to return faultType.
				// Need to return faultType which is returned from QueryVolume.
				// Currently, just return "csi.fault.Internal".
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"queryVolumeUtil failed for volumeID: %s, err: %+v", volumeInfo.VolumeID.Id, err)
			}
			if len(queryResult.Volumes) == 0 || queryResult.Volumes[0].DatastoreUrl == "" {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"queryVolumeUtil could not retrieve volume information for volume ID: %q",
					volumeInfo.VolumeID.Id)
			}
			datastoreURL = queryResult.Volumes[0].DatastoreUrl
		}

		// If improved topology FSS is enabled, retrieve datastore topology information
		// from CSINodeTopology CRs.
		// Get all nodeVMs in cluster.
		allNodeVMs, err = c.nodeMgr.GetAllNodes(ctx)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to find VirtualMachines for the registered nodes in the cluster. Error: %v", err)
		}
		// Find datastore topology from the retrieved datastoreURL.
		datastoreAccessibleTopology, err = c.getAccessibleTopologiesForDatastore(ctx, vcenter, topologyRequirement,
			allNodeVMs, datastoreURL)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to calculate accessible topologies for the datastore %q", datastoreURL)
		}

		// Add topology segments to the CreateVolumeResponse.
		for _, topoSegments := range datastoreAccessibleTopology {
			volumeTopology := &csi.Topology{
				Segments: topoSegments,
			}
			resp.Volume.AccessibleTopology = append(resp.Volume.AccessibleTopology, volumeTopology)
		}
	}

	// Set the Snapshot VolumeContentSource in the CreateVolumeResponse
	if contentSourceSnapshotID != "" {
		resp.Volume.ContentSource = &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: contentSourceSnapshotID,
				},
			},
		}
	}
	return resp, "", nil
}

// getAccessibleTopologiesForDatastore figures out the list of topologies from
// which the given datastore is accessible.
func (c *controller) getAccessibleTopologiesForDatastore(ctx context.Context, vcenter *cnsvsphere.VirtualCenter,
	topologyRequirement *csi.TopologyRequirement, allNodeVMs []*cnsvsphere.VirtualMachine, datastoreURL string) (
	[]map[string]string, error) {
	log := logger.GetLogger(ctx)
	var datastoreAccessibleTopology []map[string]string

	// Find out all nodes which have access to the chosen datastore.
	accessibleNodes, err := common.GetNodeVMsWithAccessToDatastore(ctx, vcenter, datastoreURL, allNodeVMs)
	if err != nil || len(accessibleNodes) == 0 {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to find all the nodes from which the datastore %q is accessible", datastoreURL)
	}

	// Get node names for the accessible nodeVMs so that we can query CSINodeTopology CRs.
	var accessibleNodeNames []string
	for _, vmref := range accessibleNodes {
		// Get UUID from VM reference.
		vmUUID, err := cnsvsphere.GetUUIDFromVMReference(ctx, vcenter, vmref.Reference())
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				err.Error())
		}
		// Get NodeVM name from VM UUID.
		nodeName, err := c.nodeMgr.GetNodeNameByUUID(ctx, vmUUID)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				err.Error())
		}
		accessibleNodeNames = append(accessibleNodeNames, nodeName)
	}

	datastoreAccessibleTopology, err = c.topologyMgr.GetTopologyInfoFromNodes(ctx,
		commoncotypes.VanillaRetrieveTopologyInfoParams{
			NodeNames:           accessibleNodeNames,
			DatastoreURL:        datastoreURL,
			TopologyRequirement: topologyRequirement,
		})
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to find accessible topologies for the remaining nodes %v. Error: %+v",
			accessibleNodeNames, err)
	}
	return datastoreAccessibleTopology, nil
}

// createFileVolume creates a file volume based on the CreateVolumeRequest.
func (c *controller) createFileVolume(ctx context.Context, req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, string, error) {
	log := logger.GetLogger(ctx)
	// Error out if TopologyRequirement is provided during file volume provisioning
	// as this is not supported yet.
	if req.GetAccessibilityRequirements() != nil {
		return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCode(log, codes.InvalidArgument,
			"volume topology feature for file volumes is not supported.")
	}

	// Volume Size - Default is 10 GiB.
	volSizeBytes := int64(common.DefaultGbDiskSize * common.GbInBytes)
	if req.GetCapacityRange() != nil && req.GetCapacityRange().RequiredBytes != 0 {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	volSizeMB := int64(common.RoundUpSize(volSizeBytes, common.MbInBytes))

	// Fetching the feature state for csi-migration before parsing storage class
	// params.
	csiMigrationFeatureState := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration)
	scParams, err := common.ParseStorageClassParams(ctx, req.Parameters, csiMigrationFeatureState)
	// TODO: Need to figure out the fault returned by ParseStorageClassParams.
	// Currently, just return "csi.fault.Internal".
	if err != nil {
		return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.InvalidArgument,
			"parsing storage class parameters failed with error: %+v", err)
	}

	var createVolumeSpec = common.CreateVolumeSpec{
		CapacityMB: volSizeMB,
		Name:       req.Name,
		ScParams:   scParams,
		VolumeType: common.FileVolumeType,
	}
	var volumeID string
	var faultType string
	filterSuspendedDatastores := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CnsMgrSuspendCreateVolume)
	if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIAuthCheck) {
		fsEnabledClusterToDsInfoMap := c.authMgr.GetFsEnabledClusterToDsMap(ctx)

		var filteredDatastores []*cnsvsphere.DatastoreInfo
		for _, datastores := range fsEnabledClusterToDsInfoMap {
			filteredDatastores = append(filteredDatastores, datastores...)
		}

		if len(filteredDatastores) == 0 {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCode(log, codes.Internal,
				"no datastores found to create file volume")
		}
		volumeID, faultType, err = common.CreateFileVolumeUtil(ctx, cnstypes.CnsClusterFlavorVanilla,
			c.manager, &createVolumeSpec, filteredDatastores, filterSuspendedDatastores, false)
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to create volume. Error: %+v", err)
		}
	} else {
		volumeID, faultType, err = common.CreateFileVolumeUtilOld(ctx, cnstypes.CnsClusterFlavorVanilla,
			c.manager, &createVolumeSpec, filterSuspendedDatastores, false)
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to create volume. Error: %+v", err)
		}
	}

	attributes := make(map[string]string)
	attributes[common.AttributeDiskType] = common.DiskTypeFileVolume

	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: int64(units.FileSize(volSizeMB * common.MbInBytes)),
			VolumeContext: attributes,
		},
	}
	return resp, "", nil
}

// CreateVolume is creating CNS Volume using volume request specified in
// CreateVolumeRequest.
func (c *controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType
	createVolumeInternal := func() (
		*csi.CreateVolumeResponse, string, error) {
		log.Infof("CreateVolume: called with args %+v", *req)
		//TODO: If the err is returned by invoking CNS API, then faultType should be
		// populated by the underlying layer.
		// If the request failed due to validate the request, "csi.fault.InvalidArgument" will be return.
		// If thr reqeust failed due to object not found, "csi.fault.NotFound" will be return.
		// For all other cases, the faultType will be set to "csi.fault.Internal" for now.
		// Later we may need to define different csi faults.
		volumeCapabilities := req.GetVolumeCapabilities()
		if err := common.IsValidVolumeCapabilities(ctx, volumeCapabilities); err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.InvalidArgument,
				"volume capability not supported. Err: %+v", err)
		}
		if common.IsFileVolumeRequest(ctx, volumeCapabilities) {
			volumeType = prometheus.PrometheusFileVolumeType
			isvSANFileServicesSupported, err := c.manager.VcenterManager.IsvSANFileServicesSupported(ctx,
				c.manager.VcenterConfig.Host)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to verify if vSAN file services is supported or not. Error:%+v", err)
			}
			if !isvSANFileServicesSupported {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCode(log, codes.FailedPrecondition,
					"fileshare volume creation is not supported on vSAN 67u3 release")
			}
			return c.createFileVolume(ctx, req)
		}
		volumeType = prometheus.PrometheusBlockVolumeType
		return c.createBlockVolume(ctx, req)
	}
	resp, faultType, err := createVolumeInternal()
	log.Debugf("createVolumeInternal: returns fault %q", faultType)
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusCreateVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusCreateVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Volume created successfully. Volume Handle: %q, PV Name: %q", resp.Volume.VolumeId, req.Name)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusCreateVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

// DeleteVolume is deleting CNS Volume specified in DeleteVolumeRequest.
func (c *controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType
	cnsVolumeType := common.UnknownVolumeType

	deleteVolumeInternal := func() (
		*csi.DeleteVolumeResponse, string, error) {
		log.Infof("DeleteVolume: called with args: %+v", *req)
		//TODO: If the err is returned by invoking CNS API, then faultType should be
		// populated by the underlying layer.
		// If the request failed due to validate the request, "csi.fault.InvalidArgument" will be return.
		// If thr reqeust failed due to object not found, "csi.fault.NotFound" will be return.
		// For all other cases, the faultType will be set to "csi.fault.Internal" for now.
		// Later we may need to define different csi faults.
		var (
			faultType      string
			err            error
			volumePath     string
			volumeManager  cnsvolume.Manager
			vCenterHost    string
			vCenterManager cnsvsphere.VirtualCenterManager
		)

		err = validateVanillaDeleteVolumeRequest(ctx, req)
		if err != nil {
			return nil, csifault.CSIInvalidArgumentFault, err
		}
		if strings.Contains(req.VolumeId, ".vmdk") {
			volumeType = prometheus.PrometheusBlockVolumeType
			cnsVolumeType = common.BlockVolumeType
			// In-tree volume support.
			if !commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration) {
				// Migration feature switch is disabled.
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"volume-migration feature switch is disabled. Cannot use volume with vmdk path :%q", req.VolumeId)
			}
			// Migration feature switch is enabled.
			// If this is multi-VC configuration, fail the operation.
			if multivCenterCSITopologyEnabled && len(c.managers.VcenterConfigs) > 1 {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"migrated volumes are not supported in multi-VC setup. Cannot use volume with vmdk path: %q", req.VolumeId)
			}

			volumePath = req.VolumeId
			// In case if feature state switch is enabled after controller is
			// deployed, we need to initialize the volumeMigrationService.
			if err := initVolumeMigrationService(ctx, c); err != nil {
				// Error is already wrapped in CSI error code.
				return nil, csifault.CSIInternalFault, err
			}
			req.VolumeId, err = volumeMigrationService.GetVolumeID(ctx, &migration.VolumeSpec{VolumePath: req.VolumeId}, false)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get VolumeID from volumeMigrationService for volumePath: %q", volumePath)
			}
		}
		// Fetch vCenterHost, vCenterManager & volumeManager for given volume, based on VC configuration
		vCenterManager = getVCenterManagerForVCenter(ctx, c)
		vCenterHost, volumeManager, err = getVCenterAndVolumeManagerForVolumeID(ctx, c, req.VolumeId, volumeInfoService)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get vCenter/volume manager for volume Id: %q. Error: %v", req.VolumeId, err)
		}

		if cnsVolumeType == common.UnknownVolumeType {
			cnsVolumeType, err = common.GetCnsVolumeType(ctx, volumeManager, req.VolumeId)
			if err != nil {
				if err.Error() == common.ErrNotFound.Error() {
					// The volume couldn't be found during query, assuming the delete operation as success
					return &csi.DeleteVolumeResponse{}, "", nil
				} else {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to determine CNS volume type for volume: %q. Error: %+v", req.VolumeId, err)
				}
			}
			volumeType = convertCnsVolumeType(ctx, cnsVolumeType)
		}
		// Check if the volume contains CNS snapshots only for block volumes.
		if cnsVolumeType == common.BlockVolumeType &&
			commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot) {
			isCnsSnapshotSupported, err := vCenterManager.IsCnsSnapshotSupported(ctx, vCenterHost)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to check if cns snapshot operations are supported on VC due to error: %v", err)
			}
			if isCnsSnapshotSupported {
				snapshots, _, err := common.QueryVolumeSnapshotsByVolumeID(ctx, volumeManager, req.VolumeId,
					common.QuerySnapshotLimit)
				if err != nil {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to retrieve snapshots for volume: %s. Error: %+v", req.VolumeId, err)
				}
				if len(snapshots) == 0 {
					log.Infof("no CNS snapshots found for volume: %s, the volume can be safely deleted",
						req.VolumeId)
				} else {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
						"volume: %s with existing snapshots %v cannot be deleted, "+
							"please delete snapshots before deleting the volume", req.VolumeId, snapshots)
				}
			}
		}
		faultType, err = common.DeleteVolumeUtil(ctx, volumeManager, req.VolumeId, true)
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to delete volume: %q. Error: %+v", req.VolumeId, err)
		}
		// Migration feature switch is enabled and volumePath is set.
		if volumePath != "" {
			// Delete VolumePath to VolumeID mapping.
			err = volumeMigrationService.DeleteVolumeInfo(ctx, req.VolumeId)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to delete volumeInfo CR for volume: %q. Error: %+v", req.VolumeId, err)
			}
		}
		// If this is multi-VC configuration, delete CnsVolumeInfo CR
		if multivCenterCSITopologyEnabled && len(c.managers.VcenterConfigs) > 1 {
			err = volumeInfoService.DeleteVolumeInfo(ctx, req.VolumeId)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to delete cnsvolumeInfo CR for volume: %q. Error: %+v", req.VolumeId, err)
			}
		}
		return &csi.DeleteVolumeResponse{}, "", nil
	}
	resp, faultType, err := deleteVolumeInternal()
	log.Debugf("deleteVolumeInternal: returns fault %q for volume %q", faultType, req.VolumeId)
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusDeleteVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDeleteVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Volume %q deleted successfully.", req.VolumeId)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDeleteVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

// ControllerPublishVolume attaches a volume to the Node VM.
// Volume id and node name is retrieved from ControllerPublishVolumeRequest.
func (c *controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType

	controllerPublishVolumeInternal := func() (
		*csi.ControllerPublishVolumeResponse, string, error) {
		log.Infof("ControllerPublishVolume: called with args %+v", *req)
		//TODO: If the err is returned by invoking CNS API, then faultType should be
		// populated by the underlying layer.
		// If the request failed due to validate the request, "csi.fault.InvalidArgument" will be return.
		// If thr reqeust failed due to object not found, "csi.fault.NotFound" will be return.
		// For all other cases, the faultType will be set to "csi.fault.Internal" for now.
		// Later we may need to define different csi faults.
		err := validateVanillaControllerPublishVolumeRequest(ctx, req)
		if err != nil {

			return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCodef(log, codes.Internal,
				"validation for PublishVolume Request: %+v has failed. Error: %v", *req, err)
		}
		publishInfo := make(map[string]string)
		_, volumeManager, err := getVCenterAndVolumeManagerForVolumeID(ctx, c, req.VolumeId, volumeInfoService)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get volume manager for volume Id: %q. Error: %v", req.VolumeId, err)
		}
		// Check whether its a block or file volume.
		if common.IsFileVolumeRequest(ctx, []*csi.VolumeCapability{req.GetVolumeCapability()}) {
			volumeType = prometheus.PrometheusFileVolumeType
			// File Volume.
			queryFilter := cnstypes.CnsQueryFilter{
				VolumeIds: []cnstypes.CnsVolumeId{{Id: req.VolumeId}},
			}
			querySelection := cnstypes.CnsQuerySelection{
				Names: []string{
					string(cnstypes.QuerySelectionNameTypeBackingObjectDetails),
				},
			}
			// Select only the backing object details.
			queryResult, err := volumeManager.QueryAllVolume(ctx, queryFilter, querySelection)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"queryVolume failed for volumeID: %q with err=%+v", req.VolumeId, err)
			}
			if len(queryResult.Volumes) == 0 {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"volumeID %s not found in QueryVolume", req.VolumeId)
			}

			vSANFileBackingDetails :=
				queryResult.Volumes[0].BackingObjectDetails.(*cnstypes.CnsVsanFileShareBackingDetails)
			publishInfo[common.AttributeDiskType] = common.DiskTypeFileVolume
			nfsv4AccessPointFound := false
			for _, kv := range vSANFileBackingDetails.AccessPoints {
				if kv.Key == common.Nfsv4AccessPointKey {
					publishInfo[common.Nfsv4AccessPoint] = kv.Value
					nfsv4AccessPointFound = true
					break
				}
			}
			if !nfsv4AccessPointFound {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get NFSv4 access point for volume: %q. Returned vSAN file backing details: %+v",
					req.VolumeId, vSANFileBackingDetails)
			}
		} else {
			// Block Volume.
			volumeType = prometheus.PrometheusBlockVolumeType
			if strings.Contains(req.VolumeId, ".vmdk") {
				// In-tree volume support.
				if !commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration) {
					// Migration feature switch is disabled.
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"volume-migration feature switch is disabled. Cannot use volume with vmdk path :%q", req.VolumeId)
				} else {
					if multivCenterCSITopologyEnabled && len(c.managers.VcenterConfigs) > 1 {
						// Migration feature switch is enabled and multi vCenter feature is enabled, and
						// Kubernetes Cluster is spread on multiple vCenter Servers.
						return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
							"volume-migration feature is not supported on the Multi-vCenter deployment. "+
								"Cannot use volume with vmdk path :%q", req.VolumeId)
					}
				}
				// Migration feature switch is enabled.
				storagePolicyName := req.VolumeContext[common.AttributeStoragePolicyName]
				volumePath := req.VolumeId
				// In case if feature state switch is enabled after controller is
				// deployed, we need to initialize the volumeMigrationService.
				if err := initVolumeMigrationService(ctx, c); err != nil {
					// Error is already wrapped in CSI error code.
					return nil, csifault.CSIInternalFault, err
				}
				req.VolumeId, err = volumeMigrationService.GetVolumeID(ctx,
					&migration.VolumeSpec{VolumePath: volumePath, StoragePolicyName: storagePolicyName}, false)
				if err != nil {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to get VolumeID from volumeMigrationService for volumePath: %q", volumePath)
				}
				err = volumeMigrationService.ProtectVolumeFromVMDeletion(ctx, req.VolumeId)
				if err != nil {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to set keepAfterDeleteVm control flag for VolumeID %q", req.VolumeId)
				}
			}
			var nodevm *cnsvsphere.VirtualMachine
			if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.UseCSINodeId) {
				// if node is not yet updated to run the release of the driver publishing Node VM UUID as Node ID
				// look up Node by name
				nodevm, err = c.nodeMgr.GetNodeByName(ctx, req.NodeId)
				if err == node.ErrNodeNotFound {
					log.Infof("Performing node VM lookup using node VM UUID: %q", req.NodeId)
					nodevm, err = c.nodeMgr.GetNodeByUuid(ctx, req.NodeId)
				}

			} else {
				nodevm, err = c.nodeMgr.GetNodeByName(ctx, req.NodeId)
			}
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to find VirtualMachine for node:%q. Error: %v", req.NodeId, err)
			}
			log.Debugf("Found VirtualMachine for node:%q.", req.NodeId)
			// faultType is returned from manager.AttachVolume.
			diskUUID, faultType, err := common.AttachVolumeUtil(ctx, volumeManager, nodevm, req.VolumeId,
				false)
			if err != nil {
				return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to attach disk: %+q with node: %q err %+v", req.VolumeId, req.NodeId, err)
			}
			publishInfo[common.AttributeDiskType] = common.DiskTypeBlockVolume
			publishInfo[common.AttributeFirstClassDiskUUID] = common.FormatDiskUUID(diskUUID)
		}
		log.Infof("ControllerPublishVolume successful with publish context: %v", publishInfo)
		return &csi.ControllerPublishVolumeResponse{
			PublishContext: publishInfo,
		}, "", nil
	}
	resp, faultType, err := controllerPublishVolumeInternal()
	log.Debugf("controllerPublishVolumeInternal: returns fault %q for volume %q", faultType, req.VolumeId)
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusAttachVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusAttachVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Volume %q attached successfully to node %q.", req.VolumeId, req.NodeId)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusAttachVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

// ControllerUnpublishVolume detaches a volume from the Node VM. Volume id and
// node name is retrieved from ControllerUnpublishVolumeRequest.
func (c *controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType

	controllerUnpublishVolumeInternal := func() (
		*csi.ControllerUnpublishVolumeResponse, string, error) {
		var faultType string
		log.Infof("ControllerUnpublishVolume: called with args %+v", *req)
		//TODO: If the err is returned by invoking CNS API, then faultType should be
		// populated by the underlying layer.
		// If the request failed due to validate the request, "csi.fault.InvalidArgument" will be return.
		// If thr reqeust failed due to object not found, "csi.fault.NotFound" will be return.
		// For all other cases, the faultType will be set to "csi.fault.Internal" for now.
		// Later we may need to define different csi faults.
		err := validateVanillaControllerUnpublishVolumeRequest(ctx, req)
		if err != nil {
			return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCodef(log, codes.Internal,
				"validation for UnpublishVolume Request: %+v has failed. Error: %v", *req, err)
		}

		_, volumeManager, err := getVCenterAndVolumeManagerForVolumeID(ctx, c, req.VolumeId, volumeInfoService)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get volume manager for volume Id: %q. Error: %v", req.VolumeId, err)
		}
		if !strings.Contains(req.VolumeId, ".vmdk") {
			// Check if volume is block or file, skip detach for file volume.
			queryFilter := cnstypes.CnsQueryFilter{
				VolumeIds: []cnstypes.CnsVolumeId{{Id: req.VolumeId}},
			}
			querySelection := cnstypes.CnsQuerySelection{
				Names: []string{
					string(cnstypes.QuerySelectionNameTypeVolumeType),
				},
			}
			// Select only the volume type.
			queryResult, err := volumeManager.QueryAllVolume(ctx, queryFilter, querySelection)
			// TODO: QueryAllVolumeUtil need return faultType
			//	and we should return the faultType.
			// Currently, just return "csi.fault.Internal"
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"queryVolume failed for volumeID: %q with err=%+v", req.VolumeId, err)
			}

			if len(queryResult.Volumes) == 0 {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"volumeID %q not found in QueryVolume", req.VolumeId)
			}
			if queryResult.Volumes[0].VolumeType == common.FileVolumeType {
				volumeType = prometheus.PrometheusFileVolumeType
				log.Infof("Skipping ControllerUnpublish for file volume %q", req.VolumeId)
				return &csi.ControllerUnpublishVolumeResponse{}, "", nil
			}
		} else {
			// In-tree volume support.
			volumeType = prometheus.PrometheusBlockVolumeType
			if !commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.CSIMigration) {
				// Migration feature switch is disabled.
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"volume-migration feature switch is disabled. Cannot use volume with vmdk path: %q", req.VolumeId)
			} else {
				if multivCenterCSITopologyEnabled && len(c.managers.VcenterConfigs) > 1 {
					// Migration feature switch is enabled and multi vCenter feature is enabled, and
					// Kubernetes Cluster is spread on multiple vCenter Servers.
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"volume-migration feature is not supported on the Multi-vCenter deployment. "+
							"Cannot use volume with vmdk path :%q", req.VolumeId)
				}
			}
			// Migration feature switch is enabled.
			//
			// ControllerUnpublishVolume will never be the first call back for vmdk
			// registration with CNS. Here in the migration.VolumeSpec, we do not
			// supply SPBM Policy name. Node drain is the pre-requisite for volume
			// migration, so volume will be registered with SPBM policy during
			// ControllerPublish if metadata-syncer fails to register volume using
			// associated SPBM Policy. For ControllerUnpublishVolume, we anticipate
			// volume is already registered with CNS, and volumeMigrationService
			// should return volumeID for requested VolumePath.
			volumePath := req.VolumeId
			// In case if feature state switch is enabled after controller is
			// deployed, we need to initialize the volumeMigrationService.
			if err := initVolumeMigrationService(ctx, c); err != nil {
				// Error is already wrapped in CSI error code.
				return nil, csifault.CSIInternalFault, err
			}
			req.VolumeId, err = volumeMigrationService.GetVolumeID(ctx, &migration.VolumeSpec{VolumePath: volumePath}, false)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to get VolumeID from volumeMigrationService for volumePath: %q", volumePath)
			}
		}
		// Block Volume.
		volumeType = prometheus.PrometheusBlockVolumeType
		var nodevm *cnsvsphere.VirtualMachine
		if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.UseCSINodeId) {
			// if node is not yet updated to run the release of the driver publishing Node VM UUID as Node ID
			// look up Node by name
			nodevm, err = c.nodeMgr.GetNodeByName(ctx, req.NodeId)
			if err == node.ErrNodeNotFound {
				log.Infof("Performing node VM lookup using node VM UUID: %q", req.NodeId)
				nodevm, err = c.nodeMgr.GetNodeByUuid(ctx, req.NodeId)
			}
		} else {
			nodevm, err = c.nodeMgr.GetNodeByName(ctx, req.NodeId)
		}
		if err != nil {
			if err == cnsvsphere.ErrVMNotFound {
				log.Infof("Virtual Machine for Node ID: %v is not present in the VC Inventory. "+
					"Marking ControllerUnpublishVolume for Volume: %q as successful.", req.NodeId, req.VolumeId)
				return &csi.ControllerUnpublishVolumeResponse{}, "", nil
			} else {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to find VirtualMachine for node:%q. Error: %v", req.NodeId, err)
			}
		}
		faultType, err = common.DetachVolumeUtil(ctx, volumeManager, nodevm, req.VolumeId)
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to detach disk: %+q from node: %q err %+v", req.VolumeId, req.NodeId, err)
		}
		log.Infof("ControllerUnpublishVolume successful for volume ID: %s", req.VolumeId)
		return &csi.ControllerUnpublishVolumeResponse{}, "", nil
	}
	resp, faultType, err := controllerUnpublishVolumeInternal()
	log.Debugf("controllerUnpublishVolumeInternal: returns fault %q for volume %q", faultType, req.VolumeId)
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusDetachVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDetachVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Volume %q detached successfully from node %q.", req.VolumeId, req.NodeId)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDetachVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

// ControllerExpandVolume expands a volume.
// Volume id and size is retrieved from ControllerExpandVolumeRequest.
func (c *controller) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (
	*csi.ControllerExpandVolumeResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType
	controllerExpandVolumeInternal := func() (
		*csi.ControllerExpandVolumeResponse, string, error) {
		var (
			vCenterHost    string
			vCenterManager cnsvsphere.VirtualCenterManager
			volumeManager  cnsvolume.Manager
			err            error
			faultType      string
		)

		log.Infof("ControllerExpandVolume: called with args %+v", *req)
		// TODO: If the err is returned by invoking CNS API, then faultType should be
		// populated by the underlying layer.
		// If the request failed due to validate the request, "csi.fault.InvalidArgument" will be return.
		// If thr reqeust failed due to object not found, "csi.fault.NotFound" will be return.
		// For all other cases, the faultType will be set to "csi.fault.Internal" for now.
		// Later we may need to define different csi faults.

		// csifault.CSIInternalFault csifault.CSIUnimplementedFault csifault.CSIInvalidArgumentFault
		if strings.Contains(req.VolumeId, ".vmdk") {
			return nil, csifault.CSIUnimplementedFault, logger.LogNewErrorCodef(log, codes.Unimplemented,
				"cannot expand migrated vSphere volume. :%q", req.VolumeId)
		}

		// Fetch vCenterHost, vCenterManager & volumeManager for given volume, based on VC configuration
		vCenterManager = getVCenterManagerForVCenter(ctx, c)
		vCenterHost, volumeManager, err = getVCenterAndVolumeManagerForVolumeID(ctx, c, req.VolumeId, volumeInfoService)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get vCenter/volume manager for volume Id: %q. Error: %v", req.VolumeId, err)
		}

		isOnlineExpansionSupported, err := vCenterManager.IsOnlineExtendVolumeSupported(ctx, vCenterHost)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to check if online expansion is supported due to error: %v", err)
		}
		isOnlineExpansionEnabled := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.OnlineVolumeExtend)
		err = validateVanillaControllerExpandVolumeRequest(ctx, req, isOnlineExpansionEnabled, isOnlineExpansionSupported)
		if err != nil {
			msg := fmt.Sprintf("validation for ExpandVolume Request: %+v has failed. Error: %v", *req, err)
			log.Error(msg)
			return nil, csifault.CSIInternalFault, err
		}
		volumeType = prometheus.PrometheusBlockVolumeType

		volumeID := req.GetVolumeId()
		volSizeBytes := int64(req.GetCapacityRange().GetRequiredBytes())
		volSizeMB := int64(common.RoundUpSize(volSizeBytes, common.MbInBytes))
		// Check if the volume contains CNS snapshots.
		if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot) {
			isCnsSnapshotSupported, err := vCenterManager.IsCnsSnapshotSupported(ctx, vCenterHost)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"failed to check if cns snapshot is supported on VC due to error: %v", err)
			}
			if isCnsSnapshotSupported {
				snapshots, _, err := common.QueryVolumeSnapshotsByVolumeID(ctx, volumeManager, volumeID,
					common.QuerySnapshotLimit)
				if err != nil {
					return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
						"failed to retrieve snapshots for volume: %s. Error: %+v", volumeID, err)
				}
				if len(snapshots) == 0 {
					log.Infof("The volume %s can be safely expanded as no CNS snapshots were found.",
						req.VolumeId)
				} else {
					return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
						"volume: %s with existing snapshots %v cannot be expanded. "+
							"Please delete snapshots before expanding the volume", req.VolumeId, snapshots)
				}
			}
		}

		faultType, err = common.ExpandVolumeUtil(ctx, vCenterManager, vCenterHost, volumeManager, volumeID,
			volSizeMB, commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.AsyncQueryVolume))
		if err != nil {
			return nil, faultType, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to expand volume: %q to size: %d with error: %+v", volumeID, volSizeMB, err)
		}

		// Always set nodeExpansionRequired to true, even if requested size is equal
		// to current size. Volume expansion may succeed on CNS but external-resizer
		// may fail to update API server. Requests are requeued in this case. Setting
		// nodeExpandsionRequired to false marks PVC resize as finished which
		// prevents kubelet from expanding the filesystem.
		// Ref: https://github.com/kubernetes-csi/external-resizer/blob/master/pkg/controller/controller.go#L335
		nodeExpansionRequired := true
		// Node expansion is not required for raw block volumes.
		if _, ok := req.GetVolumeCapability().GetAccessType().(*csi.VolumeCapability_Block); ok {
			nodeExpansionRequired = false
		}
		resp := &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         int64(units.FileSize(volSizeMB * common.MbInBytes)),
			NodeExpansionRequired: nodeExpansionRequired,
		}
		return resp, "", nil
	}

	resp, faultType, err := controllerExpandVolumeInternal()
	if err != nil {
		log.Debugf("controllerExpandVolumeInternal: returns fault %q for volume %q", faultType, req.VolumeId)
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusExpandVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusExpandVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Volume %q expanded successfully.", req.VolumeId)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusExpandVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return resp, err
}

// ValidateVolumeCapabilities returns the capabilities of the volume.
func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerGetCapabilities: called with args %+v", *req)
	volCaps := req.GetVolumeCapabilities()
	var confirmed *csi.ValidateVolumeCapabilitiesResponse_Confirmed
	if err := common.IsValidVolumeCapabilities(ctx, volCaps); err == nil {
		confirmed = &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: volCaps}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: confirmed,
	}, nil
}

func (c *controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusUnknownVolumeType
	if !commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.ListVolumes) {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented, "List Volumes")
	}
	cfg, err := common.GetConfig(ctx)
	if err != nil {
		return nil, logger.LogNewErrorf(log, "failed to read config. Error: %+v", err)
	}

	// Get the global query limit
	maxEntries := cfg.Global.QueryLimit
	if req.MaxEntries != 0 {
		maxEntries = int(req.MaxEntries)
	}

	listVolumesInternal := func() (*csi.ListVolumesResponse, string, error) {
		log.Debugf("ListVolumes: called with args %+v", *req)

		startingToken := 0
		if req.StartingToken != "" {
			startingToken, err = strconv.Atoi(req.StartingToken)
			if err != nil {
				log.Errorf("Unable to convert startingToken from string to int err=%v", err)
				return nil, csifault.CSIInvalidArgumentFault, logger.LogNewErrorCode(log, codes.InvalidArgument,
					"startingToken not a valid integer")
			}
		}

		// Step 1: Get all the volume IDs of PVs, from K8s cluster
		// If startingToken is 0, then listVolume request is a new one and not part of a previous request.
		// Therefore, fetch all the volumes from K8s and CNS.
		if startingToken == 0 || cnsQueryResult == nil {
			volIDsInK8s = commonco.ContainerOrchestratorUtility.GetAllK8sVolumes()
			log.Debugf("Number of Volume IDs of PVs from K8s cluster %v, list of volumes %v", len(volIDsInK8s),
				volIDsInK8s)

			// Step 2: Get all Volume IDs from CNS QueryAll API
			queryFilter := cnstypes.CnsQueryFilter{
				ContainerClusterIds: []string{cfg.Global.ClusterID},
			}
			querySelection := cnstypes.CnsQuerySelection{
				Names: []string{
					string(cnstypes.QuerySelectionNameTypeVolumeType),
				},
			}
			// Select only the volume type.
			cnsQueryResult, err = c.manager.VolumeManager.QueryAllVolume(ctx, queryFilter, querySelection)
			if err != nil {
				return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
					"queryVolume failed on Cluster ID %q with err = %+v ", cfg.Global.ClusterID, err)
			}
		}

		// Step 3: If the difference between number of K8s volumes and CNS volumes is greater than threshold,
		// fail the operation, as it can result in too many attach calls.
		if len(volIDsInK8s)-len(cnsQueryResult.Volumes) > cfg.Global.ListVolumeThreshold {
			log.Errorf("difference between number of K8s volumes: %d, and CNS volumes: %d, is greater than "+
				"threshold: %d, and completely out of sync.", len(volIDsInK8s), len(cnsQueryResult.Volumes),
				cfg.Global.ListVolumeThreshold)
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
				"difference between number of K8s volumes and CNS volumes is greater than threshold.")
		}

		if maxEntries > len(cnsQueryResult.Volumes) {
			maxEntries = len(cnsQueryResult.Volumes)
		}
		// Step 4: process queryLimit number of items starting from ListVolumeRequest.start_token
		var allNodeVMs []*cnsvsphere.VirtualMachine
		var entries []*csi.ListVolumesResponse_Entry

		// Get all nodes from the vanilla K8s cluster from the node manager
		allNodeVMs, err = c.nodeMgr.GetAllNodes(ctx)
		if err != nil {
			return nil, csifault.CSIInternalFault, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to get nodes(node vms) in the vanilla cluster. Error: %v", err)
		}

		nextToken := ""
		endIndex := maxEntries + startingToken
		if endIndex > len(cnsQueryResult.Volumes) {
			endIndex = len(cnsQueryResult.Volumes)
		}
		log.Debugf("Starting token: %d, End index: %d, Length of Query volume result: %d, Max entries: %d ",
			startingToken, endIndex, len(cnsQueryResult.Volumes), maxEntries)
		entries, nextToken, volumeType, err = c.processQueryResultsListVolumes(ctx, startingToken, endIndex,
			cnsQueryResult, allNodeVMs)
		if err != nil {
			return nil, csifault.CSIInternalFault, fmt.Errorf("error while processing query results for list "+
				" volumes, err: %v", err)
		}
		resp := &csi.ListVolumesResponse{
			Entries:   entries,
			NextToken: nextToken,
		}

		log.Debugf("ListVolumes served %d results, token for next set: %s", len(entries), nextToken)
		return resp, "", nil
	}
	listVolResponse, faultType, err := listVolumesInternal()
	log.Debugf("List volume response: %+v", listVolResponse)
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusListVolumeOpType, volumeType, faultType)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusListVolumeOpType,
			prometheus.PrometheusFailStatus, faultType).Observe(time.Since(start).Seconds())
	} else {
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusListVolumeOpType,
			prometheus.PrometheusPassStatus, faultType).Observe(time.Since(start).Seconds())
	}
	return listVolResponse, err
}

func (c *controller) processQueryResultsListVolumes(ctx context.Context, startingToken int, endIndex int,
	queryResult *cnstypes.CnsQueryResult, allNodeVMs []*cnsvsphere.VirtualMachine) ([]*csi.ListVolumesResponse_Entry,
	string, string, error) {

	volumeType := ""
	nextToken := ""
	log := logger.GetLogger(ctx)
	var entries []*csi.ListVolumesResponse_Entry

	volumeIDToNodeUUIDMap, err := getBlockVolumeToHostMap(ctx, c.manager, allNodeVMs)
	if err != nil {
		return entries, nextToken, volumeType, err
	}

	for i := startingToken; i < endIndex; i++ {
		if queryResult.Volumes[i].VolumeType == common.FileVolumeType {
			volumeType = prometheus.PrometheusFileVolumeType
			fileVolID := queryResult.Volumes[i].VolumeId.Id

			// Populate csi.Volume info for the given volume
			fileVolumeInfo := &csi.Volume{
				VolumeId: fileVolID,
			}
			// Getting published nodes
			publishedNodeIds := commonco.ContainerOrchestratorUtility.GetNodesForVolumes(ctx, []string{fileVolID})
			for volID, nodeName := range publishedNodeIds {
				if volID == fileVolID && len(nodeName) != 0 {
					nodeVMObj, err := c.nodeMgr.GetNodeByName(ctx, publishedNodeIds[fileVolID][0])
					if err != nil {
						log.Errorf("Failed to get node vm object from the node name, err:%v", err)
						return entries, nextToken, volumeType, err
					}
					nodeVMUUID := nodeVMObj.UUID

					// Populate published node
					volStatus := &csi.ListVolumesResponse_VolumeStatus{
						PublishedNodeIds: []string{nodeVMUUID},
					}

					// Populate List Volumes Entry Response
					entry := &csi.ListVolumesResponse_Entry{
						Volume: fileVolumeInfo,
						Status: volStatus,
					}

					entries = append(entries, entry)
				}
			}
		} else {
			volumeType = prometheus.PrometheusBlockVolumeType
			blockVolID := queryResult.Volumes[i].VolumeId.Id
			nodeVMUUID, found := volumeIDToNodeUUIDMap[blockVolID]
			if found {
				//Populate csi.Volume info for the given volume
				blockVolumeInfo := &csi.Volume{
					VolumeId: blockVolID,
				}
				// Getting published nodes
				volStatus := &csi.ListVolumesResponse_VolumeStatus{
					PublishedNodeIds: []string{nodeVMUUID},
				}
				entry := &csi.ListVolumesResponse_Entry{
					Volume: blockVolumeInfo,
					Status: volStatus,
				}
				// Populate List Volumes Entry Response
				entries = append(entries, entry)
			}
		}
	}

	// if length of queryAll entries > queryLimit, set nextToken to
	// start_token + queryLimit
	if len(queryResult.Volumes) > endIndex {
		nextTokenInt := endIndex
		nextToken = strconv.Itoa(nextTokenInt)
	}
	return entries, nextToken, volumeType, nil

}

func (c *controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("GetCapacity: called with args %+v", *req)
	return nil, logger.LogNewErrorCode(log, codes.Unimplemented, "getCapacity")
}

// initVolumeMigrationService is a helper method to initialize
// volumeMigrationService in controller.
func initVolumeMigrationService(ctx context.Context, c *controller) error {
	log := logger.GetLogger(ctx)
	// This check prevents unnecessary RLocks on the volumeMigration instance.
	if volumeMigrationService != nil {
		return nil
	}
	// In case if feature state switch is enabled after controller is deployed,
	// we need to initialize the volumeMigrationService.
	var err error
	volumeMigrationService, err = migration.GetVolumeMigrationService(ctx,
		&c.manager.VolumeManager, c.manager.CnsConfig, false)
	if err != nil {
		return logger.LogNewErrorCodef(log, codes.Internal,
			"failed to get migration service. Err: %v", err)
	}
	return nil
}

func (c *controller) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerGetCapabilities: called with args %+v", *req)

	controllerCaps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	}

	if commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.ListVolumes) {
		controllerCaps = append(controllerCaps, csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
			csi.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES)
	}
	var caps []*csi.ControllerServiceCapability
	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (c *controller) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (
	*csi.CreateSnapshotResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("CreateSnapshot: called with args %+v", *req)

	isBlockVolumeSnapshotEnabled := commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot)
	if !isBlockVolumeSnapshotEnabled {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented, "createSnapshot")
	}
	isCnsSnapshotSupported, err := c.manager.VcenterManager.IsCnsSnapshotSupported(ctx,
		c.manager.VcenterConfig.Host)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to check if cns snapshot is supported on VC due to error: %v", err)
	}
	if !isCnsSnapshotSupported {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented,
			"VC version does not support snapshot operations")
	}
	volumeType := prometheus.PrometheusUnknownVolumeType
	createSnapshotInternal := func() (*csi.CreateSnapshotResponse, error) {
		// Validate CreateSnapshotRequest
		if err := validateVanillaCreateSnapshotRequestRequest(ctx, req); err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"validation for CreateSnapshot Request: %+v has failed. Error: %v", *req, err)
		}
		volumeID := req.GetSourceVolumeId()

		// Check if the source volume is migrated vSphere volume
		if strings.Contains(volumeID, ".vmdk") {
			return nil, logger.LogNewErrorCodef(log, codes.Unimplemented,
				"cannot snapshot migrated vSphere volume. :%q", volumeID)
		}
		volumeType = prometheus.PrometheusBlockVolumeType
		// Query capacity in MB and datastore url for block volume snapshot
		volumeIds := []cnstypes.CnsVolumeId{{Id: volumeID}}
		cnsVolumeDetailsMap, err := utils.QueryVolumeDetailsUtil(ctx, c.manager.VolumeManager, volumeIds)
		if err != nil {
			return nil, err
		}
		if _, ok := cnsVolumeDetailsMap[volumeID]; !ok {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"cns query volume did not return the volume: %s", volumeID)
		}
		snapshotSizeInMB := cnsVolumeDetailsMap[volumeID].SizeInMB
		datastoreUrl := cnsVolumeDetailsMap[volumeID].DatastoreUrl
		if cnsVolumeDetailsMap[volumeID].VolumeType != common.BlockVolumeType {
			return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
				"queried volume doesn't have the expected volume type. Expected VolumeType: %v. "+
					"Queried VolumeType: %v", volumeType, cnsVolumeDetailsMap[volumeID].VolumeType)
		}
		// Check if snapshots number of this volume reaches the granular limit on VSAN/VVOL
		maxSnapshotsPerBlockVolume := c.manager.CnsConfig.Snapshot.GlobalMaxSnapshotsPerBlockVolume
		log.Infof("The limit of the maximum number of snapshots per block volume is "+
			"set to the global maximum (%v) by default.", maxSnapshotsPerBlockVolume)
		if c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVSAN > 0 ||
			c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVVOL > 0 {

			var isGranularMaxEnabled bool
			if strings.Contains(datastoreUrl, strings.ToLower(string(types.HostFileSystemVolumeFileSystemTypeVsan))) {
				if c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVSAN > 0 {
					maxSnapshotsPerBlockVolume = c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVSAN
					isGranularMaxEnabled = true

				}
			} else if strings.Contains(datastoreUrl, strings.ToLower(string(types.HostFileSystemVolumeFileSystemTypeVVOL))) {
				if c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVVOL > 0 {
					maxSnapshotsPerBlockVolume = c.manager.CnsConfig.Snapshot.GranularMaxSnapshotsPerBlockVolumeInVVOL
					isGranularMaxEnabled = true
				}
			}

			if isGranularMaxEnabled {
				log.Infof("The limit of the maximum number of snapshots per block volume on datastore %q is "+
					"overridden by the granular maximum (%v).", datastoreUrl, maxSnapshotsPerBlockVolume)
			}
		}

		// Check if snapshots number of this volume reaches the limit
		snapshotList, _, err := common.QueryVolumeSnapshotsByVolumeID(ctx, c.manager.VolumeManager, volumeID,
			common.QuerySnapshotLimit)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to query snapshots of volume %s for the limit check. Error: %v", volumeID, err)
		}

		if len(snapshotList) >= maxSnapshotsPerBlockVolume {
			return nil, logger.LogNewErrorCodef(log, codes.FailedPrecondition,
				"the number of snapshots on the source volume %s reaches the configured maximum (%v)",
				volumeID, c.manager.CnsConfig.Snapshot.GlobalMaxSnapshotsPerBlockVolume)
		}

		// the returned snapshotID below is a combination of CNS VolumeID and CNS SnapshotID concatenated by the "+"
		// sign. That is, a string of "<UUID>+<UUID>". Because, all other CNS snapshot APIs still require both
		// VolumeID and SnapshotID as the input, while corresponding snapshot APIs in upstream CSI require SnapshotID.
		// So, we need to bridge the gap in vSphere CSI driver and return a combined SnapshotID to CSI Snapshotter.
		snapshotID, snapshotCreateTimePtr, err := common.CreateSnapshotUtil(ctx, c.manager, volumeID, req.Name)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"failed to create snapshot on volume %q: %v", volumeID, err)
		}
		snapshotCreateTimeInProto := timestamppb.New(*snapshotCreateTimePtr)

		createSnapshotResponse := &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SizeBytes:      snapshotSizeInMB * common.MbInBytes,
				SnapshotId:     snapshotID,
				SourceVolumeId: volumeID,
				CreationTime:   snapshotCreateTimeInProto,
				ReadyToUse:     true,
			},
		}

		log.Infof("CreateSnapshot succeeded for snapshot %s "+
			"on volume %s size %d Time proto %+v Timestamp %+v Response: %+v",
			snapshotID, volumeID, snapshotSizeInMB*common.MbInBytes, snapshotCreateTimeInProto,
			*snapshotCreateTimePtr, createSnapshotResponse)
		return createSnapshotResponse, nil
	}

	start := time.Now()
	resp, err := createSnapshotInternal()
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusCreateSnapshotOpType, volumeType, "NotComputed")
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusCreateSnapshotOpType,
			prometheus.PrometheusFailStatus, "NotComputed").Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Snapshot for volume %q created successfully.", req.GetSourceVolumeId())
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusCreateSnapshotOpType,
			prometheus.PrometheusPassStatus, "").Observe(time.Since(start).Seconds())
	}
	return resp, err
}

func (c *controller) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (
	*csi.DeleteSnapshotResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DeleteSnapshot: called with args %+v", *req)

	isBlockVolumeSnapshotEnabled :=
		commonco.ContainerOrchestratorUtility.IsFSSEnabled(ctx, common.BlockVolumeSnapshot)
	if !isBlockVolumeSnapshotEnabled {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented, "deleteSnapshot")
	}

	isCnsSnapshotSupported, err := c.manager.VcenterManager.IsCnsSnapshotSupported(ctx,
		c.manager.VcenterConfig.Host)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to check if cns snapshot is supported on VC due to error: %v", err)
	}
	if !isCnsSnapshotSupported {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented,
			"VC version does not support snapshot operations")
	}

	deleteSnapshotInternal := func() (*csi.DeleteSnapshotResponse, error) {
		csiSnapshotID := req.GetSnapshotId()
		err := common.DeleteSnapshotUtil(ctx, c.manager, csiSnapshotID)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal,
				"Failed to delete snapshot %q. Error: %+v",
				csiSnapshotID, err)
		}

		log.Infof("DeleteSnapshot: successfully deleted snapshot %q", csiSnapshotID)
		return &csi.DeleteSnapshotResponse{}, nil
	}

	volumeType := prometheus.PrometheusBlockVolumeType
	start := time.Now()
	resp, err := deleteSnapshotInternal()
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusDeleteSnapshotOpType, volumeType, "NotComputed")
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDeleteSnapshotOpType,
			prometheus.PrometheusFailStatus, "NotComputed").Observe(time.Since(start).Seconds())
	} else {
		log.Infof("Snapshot %q deleted successfully.", req.SnapshotId)
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusDeleteSnapshotOpType,
			prometheus.PrometheusPassStatus, "").Observe(time.Since(start).Seconds())
	}
	return resp, err

}

func (c *controller) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (
	*csi.ListSnapshotsResponse, error) {
	start := time.Now()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	volumeType := prometheus.PrometheusBlockVolumeType

	isCnsSnapshotSupported, err := c.manager.VcenterManager.IsCnsSnapshotSupported(ctx,
		c.manager.VcenterConfig.Host)
	if err != nil {
		return nil, logger.LogNewErrorCodef(log, codes.Internal,
			"failed to check if cns snapshot is supported on VC due to error: %v", err)
	}
	if !isCnsSnapshotSupported {
		return nil, logger.LogNewErrorCode(log, codes.Unimplemented,
			"VC version does not support snapshot operations")
	}

	listSnapshotsInternal := func() (*csi.ListSnapshotsResponse, error) {
		log.Infof("ListSnapshots: called with args %+v", *req)
		err := validateVanillaListSnapshotRequest(ctx, req)
		if err != nil {
			return nil, err
		}
		maxEntries := common.QuerySnapshotLimit
		if req.MaxEntries != 0 {
			maxEntries = int64(req.MaxEntries)
		}
		snapshots, nextToken, err := common.ListSnapshotsUtil(ctx, c.manager.VolumeManager, req.SourceVolumeId,
			req.SnapshotId, req.StartingToken, maxEntries)
		if err != nil {
			return nil, logger.LogNewErrorCodef(log, codes.Internal, " failed to retrieve the snapshots, err: %+v", err)
		}
		var entries []*csi.ListSnapshotsResponse_Entry
		for _, snapshot := range snapshots {
			entry := &csi.ListSnapshotsResponse_Entry{
				Snapshot: snapshot,
			}
			entries = append(entries, entry)
		}
		resp := &csi.ListSnapshotsResponse{
			Entries:   entries,
			NextToken: nextToken,
		}
		log.Infof("ListSnapshot served %d results, token for next set: %s", len(entries), nextToken)
		return resp, nil
	}
	resp, err := listSnapshotsInternal()
	if err != nil {
		log.Errorf("Operation failed, reporting failure status to Prometheus."+
			" Operation Type: %q, Volume Type: %q, Fault Type: %q",
			prometheus.PrometheusListSnapshotsOpType, volumeType, "NotComputed")
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusListSnapshotsOpType,
			prometheus.PrometheusFailStatus, "NotComputed").Observe(time.Since(start).Seconds())
	} else {
		prometheus.CsiControlOpsHistVec.WithLabelValues(volumeType, prometheus.PrometheusListSnapshotsOpType,
			prometheus.PrometheusPassStatus, "").Observe(time.Since(start).Seconds())
	}
	return resp, err
}

func (c *controller) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (
	*csi.ControllerGetVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerGetVolume: called with args %+v", *req)
	return nil, logger.LogNewErrorCode(log, codes.Unimplemented, "controllerGetVolume")
}
