package portworx

import (
	"encoding/csv"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/heptio/ark/pkg/util/collections"
	crdv1 "github.com/kubernetes-incubator/external-storage/snapshot/pkg/apis/crd/v1"
	crdclient "github.com/kubernetes-incubator/external-storage/snapshot/pkg/client"
	"github.com/kubernetes-incubator/external-storage/snapshot/pkg/controller/snapshotter"
	snapshotVolume "github.com/kubernetes-incubator/external-storage/snapshot/pkg/volume"
	"github.com/libopenstorage/openstorage/api"
	clusterclient "github.com/libopenstorage/openstorage/api/client/cluster"
	volumeclient "github.com/libopenstorage/openstorage/api/client/volume"
	"github.com/libopenstorage/openstorage/cluster"
	"github.com/libopenstorage/openstorage/volume"
	storkvolume "github.com/libopenstorage/stork/drivers/volume"
	stork_crd "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	"github.com/libopenstorage/stork/pkg/errors"
	"github.com/libopenstorage/stork/pkg/log"
	"github.com/libopenstorage/stork/pkg/snapshot"
	"github.com/libopenstorage/stork/pkg/snapshot/rule"
	"github.com/portworx/sched-ops/k8s"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	k8shelper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubeletapis "k8s.io/kubernetes/pkg/kubelet/apis"
)

// TODO: Make some of these configurable
const (
	// driverName is the name of the portworx driver implementation
	driverName = "pxd"

	// serviceName is the name of the portworx service
	serviceName = "portworx-service"

	// namespace is the kubernetes namespace in which portworx daemon set runs
	namespace = "kube-system"

	// default API port
	apiPort = 9001

	// provisionerName is the name for the driver provisioner
	provisionerName = "kubernetes.io/portworx-volume"

	// pvcProvisionerAnnotation is the annotation on PVC which has the provisioner name
	pvcProvisionerAnnotation = "volume.beta.kubernetes.io/storage-provisioner"

	// pvcNameLabel is the key of the label used to store the PVC name
	pvcNameLabel = "pvc"

	// storkSnapNameLabel is the key of the label used to store the stork Snapshot name
	storkSnapNameLabel = "stork-snap"

	// namespaceLabel is the key of the label used to store the namespace of PVC
	// and snapshots
	namespaceLabel = "namespace"

	// pxRackLabelKey Label for rack information
	pxRackLabelKey         = "px/rack"
	snapshotDataNamePrefix = "k8s-volume-snapshot"
	readySnapshotMsg       = "Snapshot created successfully and it is ready"
	// volumeSnapshot* is configuration of exponential backoff for
	// waiting for snapshot operation to complete. Starting with 2
	// seconds, multiplying by 1.5 with each step and taking 20 steps at maximum.
	// It will time out after 20 steps (6650 seconds).
	volumeSnapshotInitialDelay = 2 * time.Second
	volumeSnapshotFactor       = 1.5
	volumeSnapshotSteps        = 20

	// for cloud snaps, we use 5 * maxint32 seconds as timeout since we cannot
	// accurately predict a correct timeout
	cloudSnapshotInitialDelay = 5 * time.Second
	cloudSnapshotFactor       = 1
	cloudSnapshotSteps        = math.MaxInt32
)

type cloudSnapStatus struct {
	status      api.CloudBackupStatusType
	msg         string
	cloudSnapID string
}

// snapshot annotation constants
const (
	pxAnnotationSelectorKeyPrefix = "portworx.selector/"
	pxSnapshotNamespaceIDKey      = pxAnnotationSelectorKeyPrefix + "namespace"
	pxSnapshotGroupIDKey          = pxAnnotationSelectorKeyPrefix + "group-id"
	pxAnnotationKeyPrefix         = "portworx/"
	pxSnapshotTypeKey             = pxAnnotationKeyPrefix + "snapshot-type"
	pxCloudSnapshotCredsIDKey     = pxAnnotationKeyPrefix + "cloud-cred-id"
)

var pxGroupSnapSelectorRegex = regexp.MustCompile(`portworx\.selector/(.+)`)
var pre14VersionRegex = regexp.MustCompile(`1\.2\..+|1\.3\..+`)
var pre2VersionRegex = regexp.MustCompile(`1\.+`)

var snapAPICallBackoff = wait.Backoff{
	Duration: volumeSnapshotInitialDelay,
	Factor:   volumeSnapshotFactor,
	Steps:    volumeSnapshotSteps,
}

var cloudsnapBackoff = wait.Backoff{
	Duration: cloudSnapshotInitialDelay,
	Factor:   cloudSnapshotFactor,
	Steps:    cloudSnapshotSteps,
}

type portworx struct {
	clusterManager cluster.Cluster
	volDriver      volume.VolumeDriver
	store          cache.Store
	stopChannel    chan struct{}
}

func (p *portworx) String() string {
	return driverName
}

func (p *portworx) Init(_ interface{}) error {
	var endpoint string
	svc, err := k8s.Instance().GetService(serviceName, namespace)
	if err == nil {
		endpoint = svc.Spec.ClusterIP
	} else {
		return fmt.Errorf("Failed to get k8s service spec: %v", err)
	}

	if len(endpoint) == 0 {
		return fmt.Errorf("Failed to get endpoint for portworx volume driver")
	}

	logrus.Infof("Using %v as endpoint for portworx volume driver", endpoint)
	endpointPort := fmt.Sprintf("http://%v:%v", endpoint, apiPort)
	clnt, err := clusterclient.NewClusterClient(endpointPort, "v1")
	if err != nil {
		return err
	}
	p.clusterManager = clusterclient.ClusterManager(clnt)

	clnt, err = volumeclient.NewDriverClient(endpointPort, "pxd", "", "stork")
	if err != nil {
		return err
	}

	p.volDriver = volumeclient.VolumeDriver(clnt)

	p.stopChannel = make(chan struct{})
	err = p.startNodeCache()
	return err
}

func (p *portworx) Stop() error {
	close(p.stopChannel)
	return nil
}

func (p *portworx) startNodeCache() error {
	resyncPeriod := 30 * time.Second

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("Error getting cluster config: %v", err)
	}

	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("Error getting client, %v", err)
	}

	restClient := k8sClient.Core().RESTClient()

	watchlist := cache.NewListWatchFromClient(restClient, "nodes", v1.NamespaceAll, fields.Everything())
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return watchlist.List(options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return watchlist.Watch(options)
		},
	}
	store, controller := cache.NewInformer(lw, &v1.Node{}, resyncPeriod,
		cache.ResourceEventHandlerFuncs{},
	)
	p.store = store

	go controller.Run(p.stopChannel)
	return nil
}

func (p *portworx) InspectVolume(volumeID string) (*storkvolume.Info, error) {
	vols, err := p.volDriver.Inspect([]string{volumeID})
	if err != nil {
		return nil, &ErrFailedToInspectVolume{
			ID:    volumeID,
			Cause: fmt.Sprintf("Volume inspect returned err: %v", err),
		}
	}

	if len(vols) != 1 {
		return nil, &errors.ErrNotFound{
			ID:   volumeID,
			Type: "Volume",
		}
	}

	info := &storkvolume.Info{}
	info.VolumeID = vols[0].Id
	info.VolumeName = vols[0].Locator.Name
	for _, rset := range vols[0].ReplicaSets {
		info.DataNodes = append(info.DataNodes, rset.Nodes...)
	}
	if vols[0].Source != nil {
		info.ParentID = vols[0].Source.Parent
	}

	if len(vols[0].Locator.GetVolumeLabels()) > 0 {
		info.Labels = vols[0].Locator.GetVolumeLabels()
	} else {
		info.Labels = make(map[string]string)
	}

	for k, v := range vols[0].Spec.GetVolumeLabels() {
		info.Labels[k] = v
	}

	info.VolumeSourceRef = vols[0]
	return info, nil
}

func (p *portworx) mapNodeStatus(status api.Status) storkvolume.NodeStatus {
	switch status {
	case api.Status_STATUS_NONE:
		fallthrough
	case api.Status_STATUS_INIT:
		fallthrough
	case api.Status_STATUS_OFFLINE:
		fallthrough
	case api.Status_STATUS_ERROR:
		fallthrough
	case api.Status_STATUS_NOT_IN_QUORUM:
		fallthrough
	case api.Status_STATUS_DECOMMISSION:
		fallthrough
	case api.Status_STATUS_MAINTENANCE:
		fallthrough
	case api.Status_STATUS_NEEDS_REBOOT:
		return storkvolume.NodeOffline

	case api.Status_STATUS_OK:
		fallthrough
	case api.Status_STATUS_STORAGE_DOWN:
		return storkvolume.NodeOnline

	case api.Status_STATUS_STORAGE_DEGRADED:
		fallthrough
	case api.Status_STATUS_STORAGE_REBALANCE:
		fallthrough
	case api.Status_STATUS_STORAGE_DRIVE_REPLACE:
		return storkvolume.NodeDegraded
	default:
		return storkvolume.NodeOffline
	}
}

func (p *portworx) getNodeLabels(nodeInfo *storkvolume.NodeInfo) (map[string]string, error) {
	obj, exists, err := p.store.GetByKey(nodeInfo.ID)
	if err != nil {
		return nil, err
	} else if !exists {
		obj, exists, err = p.store.GetByKey(nodeInfo.Hostname)
		if err != nil {
			return nil, err
		} else if !exists {
			return nil, fmt.Errorf("Node %v (%v) not found in cache", nodeInfo.ID, nodeInfo.Hostname)
		}
	}
	node := obj.(*v1.Node)
	return node.Labels, nil
}

func (p *portworx) GetNodes() ([]*storkvolume.NodeInfo, error) {
	cluster, err := p.clusterManager.Enumerate()
	if err != nil {
		return nil, &ErrFailedToGetNodes{
			Cause: err.Error(),
		}
	}

	var nodes []*storkvolume.NodeInfo
	for _, n := range cluster.Nodes {
		nodeInfo := &storkvolume.NodeInfo{
			ID:       n.Id,
			Hostname: strings.ToLower(n.Hostname),
			Status:   p.mapNodeStatus(n.Status),
		}
		nodeInfo.IPs = append(nodeInfo.IPs, n.MgmtIp)
		nodeInfo.IPs = append(nodeInfo.IPs, n.DataIp)

		labels, err := p.getNodeLabels(nodeInfo)
		if err == nil {
			if rack, ok := labels[pxRackLabelKey]; ok {
				nodeInfo.Rack = rack
			}
			if zone, ok := labels[kubeletapis.LabelZoneFailureDomain]; ok {
				nodeInfo.Zone = zone
			}
			if region, ok := labels[kubeletapis.LabelZoneRegion]; ok {
				nodeInfo.Region = region
			}
		} else {
			logrus.Errorf("Error getting labels for node %v: %v", nodeInfo.Hostname, err)
		}

		nodes = append(nodes, nodeInfo)
	}
	return nodes, nil
}

func (p *portworx) OwnsPVC(pvc *v1.PersistentVolumeClaim) bool {
	storageClassName := k8shelper.GetPersistentVolumeClaimClass(pvc)
	if storageClassName == "" {
		logrus.Debugf("Empty StorageClass in PVC %v", pvc.Name)
		return false
	}

	provisioner := ""
	// Check for the provisioner in the PVC annotation. If not populated
	// try getting the provisioner from the Storage class.
	if val, ok := pvc.Annotations[pvcProvisionerAnnotation]; ok {
		provisioner = val
	} else {
		storageClass, err := k8s.Instance().GetStorageClass(storageClassName)
		if err != nil {
			logrus.Errorf("Error getting storageclass for storageclass %v in pvc %v: %v", storageClassName, pvc.Name, err)
			return false
		}
		provisioner = storageClass.Provisioner
	}

	if provisioner != provisionerName && provisioner != snapshotcontroller.GetProvisionerName() {
		logrus.Debugf("Provisioner in Storageclass not Portworx or from the snapshot Provisioner: %v", provisioner)
		return false
	}
	return true
}

func (p *portworx) GetPodVolumes(podSpec *v1.PodSpec, namespace string) ([]*storkvolume.Info, error) {
	var volumes []*storkvolume.Info
	for _, volume := range podSpec.Volumes {
		volumeName := ""
		if volume.PersistentVolumeClaim != nil {
			pvc, err := k8s.Instance().GetPersistentVolumeClaim(
				volume.PersistentVolumeClaim.ClaimName,
				namespace)
			if err != nil {
				return nil, err
			}

			if !p.OwnsPVC(pvc) {
				continue
			}

			if pvc.Status.Phase == v1.ClaimPending {
				return nil, &storkvolume.ErrPVCPending{
					Name: volume.PersistentVolumeClaim.ClaimName,
				}
			}
			volumeName = pvc.Spec.VolumeName
		} else if volume.PortworxVolume != nil {
			volumeName = volume.PortworxVolume.VolumeID
		}

		if volumeName != "" {
			volumeInfo, err := p.InspectVolume(volumeName)
			if err != nil {
				return nil, err
			}
			volumes = append(volumes, volumeInfo)
		}
	}
	return volumes, nil
}

func (p *portworx) GetVolumeClaimTemplates(templates []v1.PersistentVolumeClaim) (
	[]v1.PersistentVolumeClaim, error) {
	var pxTemplates []v1.PersistentVolumeClaim
	for _, t := range templates {
		if p.OwnsPVC(&t) {
			pxTemplates = append(pxTemplates, t)
		}
	}
	return pxTemplates, nil
}

func (p *portworx) GetSnapshotPlugin() snapshotVolume.Plugin {
	return p
}

func (p *portworx) getSnapshotName(tags *map[string]string) string {
	return "snapshot-" + (*tags)[snapshotter.CloudSnapshotCreatedForVolumeSnapshotUIDTag]
}

func (p *portworx) SnapshotCreate(
	snap *crdv1.VolumeSnapshot,
	pv *v1.PersistentVolume,
	tags *map[string]string,
) (*crdv1.VolumeSnapshotDataSource, *[]crdv1.VolumeSnapshotCondition, error) {
	var err error
	if pv == nil || pv.Spec.PortworxVolume == nil {
		err = fmt.Errorf("invalid PV: %v", pv)
		return nil, getErrorSnapshotConditions(err), err
	}

	if snap == nil {
		err = fmt.Errorf("snapshot spec not supplied to the create API")
		return nil, getErrorSnapshotConditions(err), err
	}

	pvcsForSnapshot, err := p.getPVCsForSnapshot(snap)
	if err != nil {
		return nil, getErrorSnapshotConditions(err), err
	}

	if err := rule.ValidateSnapRule(snap); err != nil {
		err = fmt.Errorf("failed to validate snap rule due to: %v", err)
		log.SnapshotLog(snap).Errorf(err.Error())
		return nil, getErrorSnapshotConditions(err), err
	}

	backgroundCommandTermChan, err := rule.ExecutePreSnapRule(pvcsForSnapshot, snap)

	defer func() {
		if backgroundCommandTermChan != nil {
			backgroundCommandTermChan <- true // regardless of what happens, always terminate commands
		}
	}()

	if err != nil {
		err = fmt.Errorf("failed to run pre-snap rule due to: %v", err)
		log.SnapshotLog(snap).Errorf(err.Error())
		return nil, getErrorSnapshotConditions(err), err
	}

	snapStatusConditions := []crdv1.VolumeSnapshotCondition{}
	var snapshotID, snapshotDataName, snapshotCredID string
	spec := &pv.Spec
	volumeID := spec.PortworxVolume.VolumeID

	snapType, err := getSnapshotType(snap)
	if err != nil {
		return nil, getErrorSnapshotConditions(err), err
	}

	switch snapType {
	case crdv1.PortworxSnapshotTypeCloud:
		log.SnapshotLog(snap).Debugf("Cloud SnapshotCreate for pv: %+v \n tags: %v", pv, tags)
		ok, msg, err := p.ensureNodesDontMatchVersionPrefix(pre14VersionRegex)
		if err != nil {
			return nil, nil, err
		}

		if !ok {
			err = &errors.ErrNotSupported{
				Feature: "Cloud snapshots",
				Reason:  "Only supported on PX version 1.4 onwards: " + msg,
			}

			return nil, getErrorSnapshotConditions(err), err
		}

		request := &api.CloudBackupCreateRequest{
			VolumeID:       volumeID,
			CredentialUUID: getCredIDFromSnapshot(snap),
		}
		err = p.volDriver.CloudBackupCreate(request)
		if err != nil {
			return nil, getErrorSnapshotConditions(err), err
		}

		status, err := p.waitForCloudSnapCompletion(api.CloudBackupOp, volumeID, false, backgroundCommandTermChan)
		if err != nil {
			log.SnapshotLog(snap).Errorf("Cloudsnap backup: %s failed due to: %v", status.cloudSnapID, err)
			return nil, getErrorSnapshotConditions(err), err
		}

		snapshotID = status.cloudSnapID
		log.SnapshotLog(snap).Infof("Cloudsnap backup: %s of vol: %s created successfully.", snapshotID, volumeID)
		snapStatusConditions = getReadySnapshotConditions()
	case crdv1.PortworxSnapshotTypeLocal:
		if isGroupSnap(snap) {
			ok, msg, err := p.ensureNodesDontMatchVersionPrefix(pre14VersionRegex)
			if err != nil {
				return nil, nil, err
			}

			if !ok {
				err = &errors.ErrNotSupported{
					Feature: "Group snapshots",
					Reason:  "Only supported on PX version 1.4 onwards: " + msg,
				}

				return nil, getErrorSnapshotConditions(err), err
			}

			groupID := snap.Metadata.Annotations[pxSnapshotGroupIDKey]
			groupLabels := parseGroupLabelsFromAnnotations(snap.Metadata.Annotations)

			err = p.validatePVForGroupSnap(pv.GetName(), groupID, groupLabels)
			if err != nil {
				return nil, getErrorSnapshotConditions(err), err
			}

			infoStr := fmt.Sprintf("Creating group snapshot: [%s] %s", snap.Metadata.Namespace, snap.Metadata.Name)
			if len(groupID) > 0 {
				infoStr = fmt.Sprintf("%s. Group ID: %s", infoStr, groupID)
			}

			if len(groupLabels) > 0 {
				infoStr = fmt.Sprintf("%s. Group labels: %v", infoStr, groupLabels)
			}

			logrus.Infof(infoStr)

			resp, err := p.volDriver.SnapshotGroup(groupID, groupLabels)
			if err != nil {
				return nil, getErrorSnapshotConditions(err), err
			}

			if len(resp.Snapshots) == 0 {
				err = fmt.Errorf("found 0 snapshots using given group selector/ID. resp: %v", resp)
				return nil, getErrorSnapshotConditions(err), err
			}

			snapDataNames := make([]string, 0)
			failedSnapVols := make([]string, 0)
			snapIDs := make([]string, 0)

			// Loop through the response and check if all succeeded. Don't create any k8s objects if
			// any of the snapshots failed
			for volID, newSnapResp := range resp.Snapshots {
				if newSnapResp.GetVolumeCreateResponse() == nil {
					err = fmt.Errorf("Portworx API returned empty response on creation of group snapshot")
					return nil, getErrorSnapshotConditions(err), err
				}

				if newSnapResp.GetVolumeCreateResponse().GetVolumeResponse() != nil &&
					len(newSnapResp.GetVolumeCreateResponse().GetVolumeResponse().GetError()) != 0 {
					log.SnapshotLog(snap).Errorf("failed to create snapshot for volume: %s due to: %v",
						volID, newSnapResp.GetVolumeCreateResponse().GetVolumeResponse().GetError())
					failedSnapVols = append(failedSnapVols, volID)
					continue
				}

				snapIDs = append(snapIDs, newSnapResp.GetVolumeCreateResponse().GetId())
			}

			if len(failedSnapVols) > 0 {
				p.revertPXSnaps(snapIDs)

				err = fmt.Errorf("all snapshots in the group did not succeed. failed: %v succeeded: %v", failedSnapVols, snapIDs)
				return nil, getErrorSnapshotConditions(err), err
			}

			createdSnapObjs := make([]*crdv1.VolumeSnapshot, 0)
			for _, newSnapResp := range resp.Snapshots {
				newSnapID := newSnapResp.GetVolumeCreateResponse().GetId()
				snapObj, err := p.createVolumeSnapshotCRD(newSnapID, newSnapResp, groupID, groupLabels, snap)
				if err != nil {
					p.revertPXSnaps(snapIDs)
					p.revertSnapObjs(createdSnapObjs)

					err = fmt.Errorf("failed to create VolumeSnapshot object for PX snapshot: %s due to: %v", newSnapID, err)
					return nil, getErrorSnapshotConditions(err), err
				}

				createdSnapObjs = append(createdSnapObjs, snapObj)
				snapDataNames = append(snapDataNames, snapObj.Spec.SnapshotDataName)
			}

			snapshotID = strings.Join(snapIDs, ",")
			snapshotDataName = strings.Join(snapDataNames, ",")
			snapStatusConditions = getReadySnapshotConditions()
		} else {
			log.SnapshotLog(snap).Debugf("SnapshotCreate for pv: %+v \n tags: %v", pv, tags)
			snapName := p.getSnapshotName(tags)
			locator := &api.VolumeLocator{
				Name: snapName,
				VolumeLabels: map[string]string{
					storkSnapNameLabel: (*tags)[snapshotter.CloudSnapshotCreatedForVolumeSnapshotNameTag],
					namespaceLabel:     (*tags)[snapshotter.CloudSnapshotCreatedForVolumeSnapshotNamespaceTag],
				},
			}
			snapshotID, err = p.volDriver.Snapshot(volumeID, true, locator, true)
			if err != nil {
				return nil, getErrorSnapshotConditions(err), err
			}
			snapStatusConditions = getReadySnapshotConditions()
		}
	}

	err = rule.ExecutePostSnapRule(pvcsForSnapshot, snap)
	if err != nil {
		err = fmt.Errorf("failed to run post-snap rule due to: %v", err)
		log.SnapshotLog(snap).Errorf(err.Error())
		return nil, getErrorSnapshotConditions(err), err
	}

	return &crdv1.VolumeSnapshotDataSource{
		PortworxSnapshot: &crdv1.PortworxVolumeSnapshotSource{
			SnapshotID:          snapshotID,
			SnapshotData:        snapshotDataName,
			SnapshotType:        snapType,
			SnapshotCloudCredID: snapshotCredID,
		},
	}, &snapStatusConditions, nil
}

func (p *portworx) SnapshotDelete(snapDataSrc *crdv1.VolumeSnapshotDataSource, _ *v1.PersistentVolume) error {
	if snapDataSrc == nil || snapDataSrc.PortworxSnapshot == nil {
		return fmt.Errorf("Invalid snapshot source %v", snapDataSrc)
	}

	switch snapDataSrc.PortworxSnapshot.SnapshotType {
	case crdv1.PortworxSnapshotTypeCloud:
		input := &api.CloudBackupDeleteRequest{
			ID:             snapDataSrc.PortworxSnapshot.SnapshotID,
			CredentialUUID: snapDataSrc.PortworxSnapshot.SnapshotCloudCredID,
		}
		return p.volDriver.CloudBackupDelete(input)
	default:
		if len(snapDataSrc.PortworxSnapshot.SnapshotData) > 0 {
			r := csv.NewReader(strings.NewReader(snapDataSrc.PortworxSnapshot.SnapshotData))
			snapDataNames, err := r.Read()
			if err != nil {
				logrus.Errorf("Failed to parse snap data csv. err: %v", err)
				return err
			}

			if len(snapDataNames) > 0 {
				var lastError error
				for _, snapDataName := range snapDataNames {
					snapData, err := k8s.Instance().GetSnapshotData(snapDataName)
					if err != nil {
						lastError = err
						logrus.Errorf("Failed to get volume snapshot data: %s due to err: %v", snapDataName, err)
						continue
					}

					snapNamespace := snapData.Spec.VolumeSnapshotRef.Namespace
					snapName := snapData.Spec.VolumeSnapshotRef.Name

					logrus.Infof("Deleting VolumeSnapshot:[%s] %s", snapNamespace, snapName)
					err = wait.ExponentialBackoff(snapAPICallBackoff, func() (bool, error) {
						err = k8s.Instance().DeleteSnapshot(snapName, snapNamespace)
						if err != nil {
							return false, nil
						}

						return true, nil
					})
					if err != nil {
						lastError = err
						logrus.Errorf("Failed to get volume snapshot: [%s] %s due to err: %v", snapNamespace, snapName, err)
						continue
					}
				}
				return lastError
			}
		}

		return p.volDriver.Delete(snapDataSrc.PortworxSnapshot.SnapshotID)
	}
}

func (p *portworx) SnapshotRestore(
	snapshotData *crdv1.VolumeSnapshotData,
	pvc *v1.PersistentVolumeClaim,
	_ string,
	parameters map[string]string,
) (*v1.PersistentVolumeSource, map[string]string, error) {
	if snapshotData == nil || snapshotData.Spec.PortworxSnapshot == nil {
		return nil, nil, fmt.Errorf("Invalid Snapshot spec")
	}
	if pvc == nil {
		return nil, nil, fmt.Errorf("Invalid PVC spec")
	}

	logrus.Debugf("SnapshotRestore for pvc: %+v", pvc)

	snapshotName, present := pvc.ObjectMeta.Annotations[crdclient.SnapshotPVCAnnotation]
	if !present || len(snapshotName) == 0 {
		return nil, nil, fmt.Errorf("source volumesnapshot name not present in PVC annotations")
	}

	// Let's verify if source snapshotdata is complete
	err := k8s.Instance().ValidateSnapshotData(snapshotData.Metadata.Name, false)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: %s is not complete. %v", snapshotName, err)
	}

	snapID := snapshotData.Spec.PortworxSnapshot.SnapshotID
	restoreVolumeName := "pvc-" + string(pvc.UID)
	var restoredVolumeID string

	switch snapshotData.Spec.PortworxSnapshot.SnapshotType {
	case crdv1.PortworxSnapshotTypeLocal:
		snapshotNamespace, ok := pvc.Annotations[snapshotcontroller.StorkSnapshotSourceNamespaceAnnotation]
		if !ok {
			snapshotNamespace = pvc.GetNamespace()
		}

		// Check if this is a group snapshot
		snap, err := k8s.Instance().GetSnapshot(snapshotName, snapshotNamespace)
		if err != nil {
			return nil, nil, err
		}

		if isGroupSnap(snap) {
			return nil, nil, fmt.Errorf("volumesnapshot: [%s] %s was used to create group snapshots. "+
				"To restore, use the volumesnapshots that were created by this group snapshot.", snapshotNamespace, snapshotName)
		}

		locator := &api.VolumeLocator{
			Name: restoreVolumeName,
			VolumeLabels: map[string]string{
				pvcNameLabel:   pvc.Name,
				namespaceLabel: pvc.Namespace,
			},
		}
		restoredVolumeID, err = p.volDriver.Snapshot(snapID, false, locator, true)
		if err != nil {
			return nil, nil, err
		}
	case crdv1.PortworxSnapshotTypeCloud:
		response, err := p.volDriver.CloudBackupRestore(&api.CloudBackupRestoreRequest{
			ID:                snapID,
			RestoreVolumeName: restoreVolumeName,
			CredentialUUID:    snapshotData.Spec.PortworxSnapshot.SnapshotCloudCredID,
		})
		if err != nil {
			return nil, nil, err
		}

		restoredVolumeID = response.RestoreVolumeID
		logrus.Infof("Cloudsnap restore of %s to %s started successfully.", snapID, restoredVolumeID)

		_, err = p.waitForCloudSnapCompletion(api.CloudRestoreOp, restoredVolumeID, true, nil)
		if err != nil {
			return nil, nil, err
		}
	}

	// create PV from restored volume
	vols, err := p.volDriver.Inspect([]string{restoredVolumeID})
	if err != nil {
		return nil, nil, &ErrFailedToInspectVolume{
			ID:    restoredVolumeID,
			Cause: fmt.Sprintf("Volume inspect returned err: %v", err),
		}
	}

	if len(vols) == 0 {
		return nil, nil, &errors.ErrNotFound{
			ID:   restoredVolumeID,
			Type: "Volume",
		}
	}

	pv := &v1.PersistentVolumeSource{
		PortworxVolume: &v1.PortworxVolumeSource{
			VolumeID: restoredVolumeID,
			FSType:   vols[0].Format.String(),
			ReadOnly: vols[0].Readonly,
		},
	}

	labels := make(map[string]string)
	return pv, labels, nil
}

func (p *portworx) DescribeSnapshot(snapshotData *crdv1.VolumeSnapshotData) (*[]crdv1.VolumeSnapshotCondition, bool /* isCompleted */, error) {
	var err error
	if snapshotData == nil || snapshotData.Spec.PortworxSnapshot == nil {
		err = fmt.Errorf("Invalid VolumeSnapshotDataSource: %v", snapshotData)
		return getErrorSnapshotConditions(err), false, nil
	}

	switch snapshotData.Spec.PortworxSnapshot.SnapshotType {
	case crdv1.PortworxSnapshotTypeLocal:
		r := csv.NewReader(strings.NewReader(snapshotData.Spec.PortworxSnapshot.SnapshotID))
		snapshotIDs, err := r.Read()
		if err != nil {
			return getErrorSnapshotConditions(err), false, err
		}

		for _, snapshotID := range snapshotIDs {
			_, err = p.InspectVolume(snapshotID)
			if err != nil {
				return getErrorSnapshotConditions(err), false, err
			}
		}
	case crdv1.PortworxSnapshotTypeCloud:
		if snapshotData.Spec.PersistentVolumeRef == nil {
			err = fmt.Errorf("PersistentVolume reference not set for snapshot: %v", snapshotData)
			return getErrorSnapshotConditions(err), false, err
		}

		pv, err := k8s.Instance().GetPersistentVolume(snapshotData.Spec.PersistentVolumeRef.Name)
		if err != nil {
			return getErrorSnapshotConditions(err), false, err
		}

		if pv.Spec.PortworxVolume == nil {
			err = fmt.Errorf("Parent PV: %s for snapshot is not a Portworx volume", pv.Name)
			return getErrorSnapshotConditions(err), false, err
		}

		csStatus := p.checkCloudSnapStatus(api.CloudBackupOp, pv.Spec.PortworxVolume.VolumeID)
		if csStatus.status == api.CloudBackupStatusFailed {
			err = fmt.Errorf(csStatus.msg)
			return getErrorSnapshotConditions(err), false, err
		}

		if csStatus.status == api.CloudBackupStatusNotStarted {
			pendingCond := getPendingSnapshotConditions(csStatus.msg)
			return &pendingCond, false, nil
		}
	}

	snapConditions := getReadySnapshotConditions()
	return &snapConditions, true, err
}

// TODO: Implement FindSnapshot
func (p *portworx) FindSnapshot(tags *map[string]string) (*crdv1.VolumeSnapshotDataSource, *[]crdv1.VolumeSnapshotCondition, error) {
	return nil, nil, &errors.ErrNotImplemented{}
}

func (p *portworx) GetSnapshotType(snap *crdv1.VolumeSnapshot) (string, error) {
	// TODO: Check if is a portworx snapshot
	snapType, err := getSnapshotType(snap)
	if err != nil {
		return "", err
	}
	if isGroupSnap(snap) {
		return "Group " + string(snapType), nil
	}
	return string(snapType), nil
}

func (p *portworx) VolumeDelete(pv *v1.PersistentVolume) error {
	if pv == nil || pv.Spec.PortworxVolume == nil {
		return fmt.Errorf("Invalid PV: %v", pv)
	}
	return p.volDriver.Delete(pv.Spec.PortworxVolume.VolumeID)
}

func (p *portworx) createVolumeSnapshotCRD(
	pxSnapID string,
	newSnapResp *api.SnapCreateResponse,
	groupID string,
	groupLabels map[string]string,
	groupSnap *crdv1.VolumeSnapshot) (*crdv1.VolumeSnapshot, error) {
	parentPVCOrVolID, err := p.findParentPVCOrVolID(pxSnapID)
	if err != nil {
		return nil, err
	}

	namespace := groupSnap.Metadata.Namespace
	volumeSnapshotName := fmt.Sprintf("%s-%s-%s", groupSnap.Metadata.Name, parentPVCOrVolID, groupSnap.GetObjectMeta().GetUID())
	snapDataSource := &crdv1.VolumeSnapshotDataSource{
		PortworxSnapshot: &crdv1.PortworxVolumeSnapshotSource{
			SnapshotID:   pxSnapID,
			SnapshotType: crdv1.PortworxSnapshotTypeLocal,
		},
	}

	snapStatus := getReadySnapshotConditions()

	snapLabels := make(map[string]string)
	if len(groupID) > 0 {
		snapLabels[pxSnapshotGroupIDKey] = groupID
	}

	for k, v := range groupLabels {
		snapLabels[k] = v
	}

	snapData, err := p.createVolumeSnapshotData(volumeSnapshotName, namespace, snapDataSource, &snapStatus, snapLabels)
	if err != nil {
		return nil, err
	}

	snapDataSource.PortworxSnapshot.SnapshotData = snapData.Metadata.Name

	snap := &crdv1.VolumeSnapshot{
		Metadata: metav1.ObjectMeta{
			Name:      volumeSnapshotName,
			Namespace: namespace,
			Labels:    snapLabels,
		},
		Spec: crdv1.VolumeSnapshotSpec{
			SnapshotDataName: snapData.Metadata.Name,
			// this is best effort as can be vol ID if PVC is deleted
			PersistentVolumeClaimName: parentPVCOrVolID,
		},
		Status: crdv1.VolumeSnapshotStatus{
			Conditions: snapStatus,
		},
	}

	log.SnapshotLog(snap).Infof("Creating VolumeSnapshot object")
	err = wait.ExponentialBackoff(snapAPICallBackoff, func() (bool, error) {
		_, err := k8s.Instance().CreateSnapshot(snap)
		if err != nil {
			log.SnapshotLog(snap).Errorf("failed to create volumesnapshot due to err: %v", err)
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		// revert snapdata
		deleteErr := wait.ExponentialBackoff(snapAPICallBackoff, func() (bool, error) {
			deleteErr := k8s.Instance().DeleteSnapshotData(snapData.Metadata.Name)
			if err != nil {
				log.SnapshotLog(snap).Errorf("failed to delete volumesnapshotdata due to err: %v", deleteErr)
				return false, nil
			}

			return true, nil
		})
		if deleteErr != nil {
			log.SnapshotLog(snap).Errorf("failed to revert volumesnapshotdata due to: %v", deleteErr)
		}

		return nil, err
	}

	return snap, nil
}

func (p *portworx) createVolumeSnapshotData(
	snapshotName, snapshotNamespace string,
	snapDataSource *crdv1.VolumeSnapshotDataSource,
	snapStatus *[]crdv1.VolumeSnapshotCondition,
	snapLabels map[string]string) (*crdv1.VolumeSnapshotData, error) {
	var lastCondition crdv1.VolumeSnapshotDataCondition
	if snapStatus != nil && len(*snapStatus) > 0 {
		conditions := *snapStatus
		ind := len(conditions) - 1
		lastCondition = crdv1.VolumeSnapshotDataCondition{
			Type:    (crdv1.VolumeSnapshotDataConditionType)(conditions[ind].Type),
			Status:  conditions[ind].Status,
			Message: conditions[ind].Message,
		}
	}

	snapshotData := &crdv1.VolumeSnapshotData{
		Metadata: metav1.ObjectMeta{
			Name:   snapshotName,
			Labels: snapLabels,
		},
		Spec: crdv1.VolumeSnapshotDataSpec{
			VolumeSnapshotRef: &v1.ObjectReference{
				Kind:      "VolumeSnapshot",
				Name:      snapshotName,
				Namespace: snapshotNamespace,
			},
			VolumeSnapshotDataSource: *snapDataSource,
		},
		Status: crdv1.VolumeSnapshotDataStatus{
			Conditions: []crdv1.VolumeSnapshotDataCondition{
				lastCondition,
			},
		},
	}

	var result *crdv1.VolumeSnapshotData
	err := wait.ExponentialBackoff(snapAPICallBackoff, func() (bool, error) {
		var err error
		result, err = k8s.Instance().CreateSnapshotData(snapshotData)
		if err != nil {
			logrus.Errorf("failed to create snapshotdata due to err: %v", err)
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		err = fmt.Errorf("error creating the VolumeSnapshotData for snap %s due to err: %v", snapshotName, err)
		logrus.Println(err)
		return nil, err
	}

	return result, nil
}

// findParentPVCOrVolID is a best effort routine that attempts to find parent PVC name for
// given snapshot ID. If not found, it returns the Parent volumeID
func (p *portworx) findParentPVCOrVolID(snapID string) (string, error) {
	snapInfo, err := p.InspectVolume(snapID)
	if err != nil {
		return "", err
	}

	if len(snapInfo.ParentID) == 0 {
		return "", fmt.Errorf("Portworx snapshot: %s does not have parent set", snapID)
	}

	snapParentInfo, err := p.InspectVolume(snapInfo.ParentID)
	if err != nil {
		logrus.Warnf("Parent volume: %s for snapshot: %s is not found due to: %v", snapInfo.ParentID, snapID, err)
		return snapInfo.ParentID, nil
	}

	parentPV, err := k8s.Instance().GetPersistentVolume(snapParentInfo.VolumeName)
	if err != nil {
		logrus.Warnf("Parent PV: %s of snapshot: %s is not found due to: %v", snapParentInfo.VolumeName, snapID, err)
		return snapInfo.ParentID, nil
	}

	pvc, err := k8s.Instance().GetPersistentVolumeClaim(parentPV.Spec.ClaimRef.Name, parentPV.Spec.ClaimRef.Namespace)
	if err != nil {
		return snapInfo.ParentID, nil
	}

	return pvc.GetName(), nil
}

func (p *portworx) waitForCloudSnapCompletion(
	op api.CloudBackupOpType,
	volID string,
	verbose bool,
	backgroundCommandTermChan chan bool) (cloudSnapStatus, error) {

	csStatus := cloudSnapStatus{
		status: api.CloudBackupStatusFailed,
		msg:    fmt.Sprintf("cloudsnap status unknown"),
	}

	err := wait.ExponentialBackoff(cloudsnapBackoff, func() (bool, error) {
		csStatus = p.checkCloudSnapStatus(op, volID)
		switch csStatus.status {
		case api.CloudBackupStatusFailed:
			err := fmt.Errorf("Cloudsnap %s of %s failed due to: %s", op, volID, csStatus.msg)
			logrus.Errorf(err.Error())
			return true, err
		case api.CloudBackupStatusDone:
			if verbose {
				logrus.Infof(csStatus.msg)
			}
			return true, nil
		case api.CloudBackupStatusActive:
			if verbose {
				logrus.Infof(csStatus.msg)
			}

			if backgroundCommandTermChan != nil {
				// since cloudsnap is already triggered and active, send signal to terminate background jobs
				backgroundCommandTermChan <- true
			}
			return false, nil
		case api.CloudBackupStatusNotStarted:
			if verbose {
				logrus.Infof(csStatus.msg)
			}
			return false, nil
		default:
			err := fmt.Errorf("received unexpected status for cloudsnap %s of %s. status: %s", op, volID, csStatus.status)
			return false, err
		}
	})

	return csStatus, err
}

func (p *portworx) checkCloudSnapStatus(op api.CloudBackupOpType, volID string) cloudSnapStatus {
	response, err := p.volDriver.CloudBackupStatus(&api.CloudBackupStatusRequest{
		SrcVolumeID: volID,
	})
	if err != nil {
		return cloudSnapStatus{
			status: api.CloudBackupStatusFailed,
			msg:    err.Error(),
		}
	}

	csStatus, present := response.Statuses[volID]
	if !present {
		return cloudSnapStatus{
			status: api.CloudBackupStatusFailed,
			msg:    fmt.Sprintf("failed to get cloudsnap status for volume: %s", volID),
		}
	}

	statusStr := getCloudSnapStatusString(&csStatus)

	if csStatus.Status == api.CloudBackupStatusFailed {
		return cloudSnapStatus{
			status:      api.CloudBackupStatusFailed,
			cloudSnapID: csStatus.ID,
			msg: fmt.Sprintf("cloudsnap %s id: %s for %s failed.",
				op, csStatus.ID, volID),
		}
	}

	if csStatus.Status == api.CloudBackupStatusActive {
		return cloudSnapStatus{
			status:      api.CloudBackupStatusActive,
			cloudSnapID: csStatus.ID,
			msg: fmt.Sprintf("cloudsnap %s id: %s for %s has started and is active.",
				op, csStatus.ID, volID),
		}
	}

	if csStatus.Status != api.CloudBackupStatusDone {
		return cloudSnapStatus{
			status:      api.CloudBackupStatusNotStarted,
			cloudSnapID: csStatus.ID,
			msg: fmt.Sprintf("cloudsnap %s id: %s for %s still not done. status: %s",
				op, csStatus.ID, volID, statusStr),
		}
	}

	return cloudSnapStatus{
		status:      api.CloudBackupStatusDone,
		cloudSnapID: csStatus.ID,
		msg:         fmt.Sprintf("cloudsnap %s id: %s for %s done.", op, csStatus.ID, volID),
	}
}

// revertPXSnaps deletes all given snapIDs
func (p *portworx) revertPXSnaps(snapIDs []string) {
	failedDeletions := make(map[string]error)
	for _, id := range snapIDs {
		err := p.volDriver.Delete(id)
		if err != nil {
			failedDeletions[id] = err
		}
	}

	if len(failedDeletions) > 0 {
		errString := ""
		for failedID, failedErr := range failedDeletions {
			errString = fmt.Sprintf("%s delete of %s failed due to err: %v.\n", errString, failedID, failedErr)
		}

		logrus.Errorf("failed to revert created PX snapshots. err: %s", errString)
		return
	}

	logrus.Infof("Successfully reverted PX snapshots")
}

func (p *portworx) revertSnapObjs(snapObjs []*crdv1.VolumeSnapshot) {
	failedDeletions := make(map[string]error)

	for _, snap := range snapObjs {
		err := wait.ExponentialBackoff(snapAPICallBackoff, func() (bool, error) {
			deleteErr := k8s.Instance().DeleteSnapshot(snap.Metadata.Name, snap.Metadata.Namespace)
			if deleteErr != nil {
				log.SnapshotLog(snap).Infof("failed to delete volumesnapshot due to: %v", deleteErr)
				return false, nil
			}

			return true, nil
		})
		if err != nil {
			failedDeletions[fmt.Sprintf("[%s] %s", snap.Metadata.Namespace, snap.Metadata.Name)] = err
		}
	}

	if len(failedDeletions) > 0 {
		errString := ""
		for failedID, failedErr := range failedDeletions {
			errString = fmt.Sprintf("%s delete of %s failed due to err: %v.\n", errString, failedID, failedErr)
		}

		logrus.Errorf("failed to revert created volumesnapshots. err: %s", errString)
		return
	}

	logrus.Infof("successfully reverted volumesnapshots")
}

func (p *portworx) validatePVForGroupSnap(pvName, groupID string, groupLabels map[string]string) error {
	volInfo, err := p.InspectVolume(pvName)
	if err != nil {
		return err
	}

	if volInfo.VolumeSourceRef == nil {
		return fmt.Errorf("inspect of volume: %s did not set the volume source ref", pvName)
	}

	pxSource, ok := volInfo.VolumeSourceRef.(*api.Volume)
	if !ok {
		return fmt.Errorf("source for volume: %s is not a Portworx volume", pvName)
	}

	// validate group ID
	if len(groupID) > 0 {
		if pxSource.GetGroup() == nil {
			return fmt.Errorf("group ID for volume: %s is not set. expected: %s", pvName, groupID)
		}

		if pxSource.GetGroup().GetId() != groupID {
			return fmt.Errorf("group ID for volume: %s does not match. expected: %s actual: %s",
				pvName, groupID, pxSource.GetGroup().GetId())
		}
	}

	// validate label selectors
	for k, v := range groupLabels {
		if value, present := volInfo.Labels[k]; !present || value != v {
			return fmt.Errorf("label '%s:%s' is not present in volume: %v. Found: %v",
				k, v, pvName, volInfo.Labels)
		}
	}

	return nil
}

func (p *portworx) ensureNodesDontMatchVersionPrefix(versionRegex *regexp.Regexp) (bool, string, error) {
	result, err := p.clusterManager.Enumerate()
	if err != nil {
		return false, "", err
	}

	for _, node := range result.Nodes {
		version := node.NodeLabels["PX Version"]
		if versionRegex.MatchString(version) {
			return false, fmt.Sprintf("node: %s has version: %s", node.Hostname, version), nil
		}
	}

	return true, "all nodes have expected version", nil
}

func (p *portworx) getPVCsForSnapshot(snap *crdv1.VolumeSnapshot) ([]v1.PersistentVolumeClaim, error) {
	var err error
	snapType, err := getSnapshotType(snap)
	if err != nil {
		return nil, err
	}

	switch snapType {
	case crdv1.PortworxSnapshotTypeCloud:
		pvc, err := k8s.Instance().GetPersistentVolumeClaim(snap.Spec.PersistentVolumeClaimName, snap.Metadata.Namespace)
		if err != nil {
			return nil, err
		}

		return []v1.PersistentVolumeClaim{*pvc}, nil
	case crdv1.PortworxSnapshotTypeLocal:
		if isGroupSnap(snap) {
			groupID := snap.Metadata.Annotations[pxSnapshotGroupIDKey]
			groupLabels := parseGroupLabelsFromAnnotations(snap.Metadata.Annotations)

			log.SnapshotLog(snap).Infof("looking for PVCs with group labels: %v", groupLabels)
			pvcs := make([]v1.PersistentVolumeClaim, 0)
			if len(groupID) > 0 {
				groupIDPVCs, err := p.getPVCsForGroupID(snap.Metadata.Namespace, groupID)
				if err != nil {
					return nil, err
				}

				pvcs = append(pvcs, groupIDPVCs...)
			}

			if len(groupLabels) > 0 {
				pvcList, err := k8s.Instance().GetPersistentVolumeClaims(snap.Metadata.Namespace, groupLabels)
				if err != nil {
					return nil, err
				}

				log.SnapshotLog(snap).Infof("found PVCs with group labels: %v", pvcList.Items)
				pvcs = append(pvcs, pvcList.Items...)
			}

			for _, pvc := range pvcs {
				if pvc.Status.Phase == v1.ClaimPending {
					return nil, fmt.Errorf("PVC: [%s] %s is still in %s phase. Group snapshot will trigger after all PVCs are bound",
						pvc.Namespace, pvc.Name, pvc.Status.Phase)
				}
			}

			return pvcs, nil
		}

		// local single snapshot
		pvc, err := k8s.Instance().GetPersistentVolumeClaim(snap.Spec.PersistentVolumeClaimName, snap.Metadata.Namespace)
		if err != nil {
			return nil, err
		}

		return []v1.PersistentVolumeClaim{*pvc}, nil
	default:
		return nil, fmt.Errorf("invalid snapshot type: %s", snapType)
	}
}

func (p *portworx) getPVCsForGroupID(namespace, groupID string) ([]v1.PersistentVolumeClaim, error) {
	// List all PX volumes for given namespace and group ID
	vols, err := p.volDriver.Enumerate(
		&api.VolumeLocator{
			VolumeLabels: map[string]string{
				"namespace": namespace,
			},
		},
		map[string]string{
			api.SpecGroup: groupID,
		},
	)
	if err != nil {
		return nil, err
	}

	pvcsWithGroupID := make([]v1.PersistentVolumeClaim, 0)
	for _, vol := range vols {
		if vol.Locator.VolumeLabels != nil {
			pvcName, ok := vol.Locator.VolumeLabels[pvcNameLabel]
			if ok && len(pvcName) > 0 {
				pvc, err := k8s.Instance().GetPersistentVolumeClaim(pvcName, namespace)
				if err != nil {
					if k8s_errors.IsNotFound(err) {
						// the PVC may have been deleted but the PX volume is still present. Skip this vol.
						continue
					}

					return nil, err
				}
				pvcsWithGroupID = append(pvcsWithGroupID, *pvc)
			}
		}
	}

	return pvcsWithGroupID, nil
}

func getSnapshotType(snap *crdv1.VolumeSnapshot) (crdv1.PortworxSnapshotType, error) {
	if snap.Metadata.Annotations != nil {
		if val, snapTypePresent := snap.Metadata.Annotations[pxSnapshotTypeKey]; snapTypePresent {
			if crdv1.PortworxSnapshotType(val) == crdv1.PortworxSnapshotTypeLocal ||
				crdv1.PortworxSnapshotType(val) == crdv1.PortworxSnapshotTypeCloud {
				return crdv1.PortworxSnapshotType(val), nil
			}

			return "", fmt.Errorf("unsupported Portworx snapshot type: %s", val)
		}
	}

	return crdv1.PortworxSnapshotTypeLocal, nil
}

func getCredIDFromSnapshot(snap *crdv1.VolumeSnapshot) (credID string) {
	if snap.Metadata.Annotations != nil {
		credID = snap.Metadata.Annotations[pxCloudSnapshotCredsIDKey]
	}

	return
}

func parseGroupLabelsFromAnnotations(annotations map[string]string) map[string]string {
	groupLabels := make(map[string]string)
	for k, v := range annotations {
		// skip group id annotation
		if k == pxSnapshotGroupIDKey {
			continue
		}

		matches := pxGroupSnapSelectorRegex.FindStringSubmatch(k)
		if len(matches) == 2 {
			groupLabels[matches[1]] = v
		}
	}

	return groupLabels
}

func isGroupSnap(snap *crdv1.VolumeSnapshot) bool {
	for k := range snap.Metadata.Annotations {
		matches := pxGroupSnapSelectorRegex.FindStringSubmatch(k)
		if len(matches) == 2 {
			return true
		}
	}

	return false
}

func getReadySnapshotConditions() []crdv1.VolumeSnapshotCondition {
	return []crdv1.VolumeSnapshotCondition{
		{
			Type:               crdv1.VolumeSnapshotConditionReady,
			Status:             v1.ConditionTrue,
			Message:            readySnapshotMsg,
			LastTransitionTime: metav1.Now(),
		},
	}
}

func getErrorSnapshotConditions(err error) *[]crdv1.VolumeSnapshotCondition {
	return &[]crdv1.VolumeSnapshotCondition{
		{
			Type:               crdv1.VolumeSnapshotConditionError,
			Status:             v1.ConditionTrue,
			Message:            fmt.Sprintf("snapshot failed due to err: %v", err),
			LastTransitionTime: metav1.Now(),
		},
	}
}

func getPendingSnapshotConditions(msg string) []crdv1.VolumeSnapshotCondition {
	return []crdv1.VolumeSnapshotCondition{
		{
			Type:               crdv1.VolumeSnapshotConditionPending,
			Status:             v1.ConditionUnknown,
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		},
	}
}

func getCloudSnapStatusString(status *api.CloudBackupStatus) string {
	var elapsedTime string
	if !status.StartTime.IsZero() {
		if status.CompletedTime.IsZero() {
			elapsedTime = time.Since(status.StartTime).String()
		} else {
			elapsedTime = status.CompletedTime.Sub(status.StartTime).String()
		}
	}

	statusStr := fmt.Sprintf("Status: %s Bytes done: %s", status.Status, strconv.FormatUint(status.BytesDone, 10))
	if len(elapsedTime) > 0 {
		statusStr = fmt.Sprintf("%s Elapsed time: %s", statusStr, elapsedTime)
	}

	if !status.CompletedTime.IsZero() {
		statusStr = fmt.Sprintf("%s Completion time: %v", statusStr, status.CompletedTime)
	}
	return statusStr
}

func (p *portworx) CreatePair(pair *stork_crd.ClusterPair) (string, error) {
	ok, msg, err := p.ensureNodesDontMatchVersionPrefix(pre2VersionRegex)
	if err != nil {
		return "", err
	}

	if !ok {
		err = &errors.ErrNotSupported{
			Feature: "Cluster pair",
			Reason:  "Only supported on PX version 2.0 onwards: " + msg,
		}
		return "", err
	}

	port := uint64(apiPort)
	if p, ok := pair.Spec.Options["port"]; ok {
		port, err = strconv.ParseUint(p, 10, 32)
		if err != nil {
			return "", fmt.Errorf("invalid port (%v) specified for cluster pair: %v", p, err)
		}
	}
	resp, err := p.clusterManager.CreatePair(&api.ClusterPairCreateRequest{
		RemoteClusterIp:    pair.Spec.Options["ip"],
		RemoteClusterToken: pair.Spec.Options["token"],
		RemoteClusterPort:  uint32(port),
	})
	if err != nil {
		return "", err
	}
	return resp.RemoteClusterId, nil
}

func (p *portworx) DeletePair(pair *stork_crd.ClusterPair) error {
	return p.clusterManager.DeletePair(pair.Status.RemoteStorageID)
}

func (p *portworx) StartMigration(migration *stork_crd.Migration) ([]*stork_crd.VolumeInfo, error) {
	ok, msg, err := p.ensureNodesDontMatchVersionPrefix(pre2VersionRegex)
	if err != nil {
		return nil, err
	}

	if !ok {
		err = &errors.ErrNotSupported{
			Feature: "Migration",
			Reason:  "Only supported on PX version 2.0 onwards: " + msg,
		}
		return nil, err
	}

	if len(migration.Spec.Selectors) != 0 {
		return nil, fmt.Errorf("selectors are not supported yet")
	}

	if len(migration.Spec.Namespaces) == 0 {
		return nil, fmt.Errorf("namespaces for migration cannot be empty")
	}

	clusterPair, err := k8s.Instance().GetClusterPair(migration.Spec.ClusterPair)
	if err != nil {
		return nil, fmt.Errorf("error getting clusterpair: %v", err)
	}
	volumeInfos := make([]*stork_crd.VolumeInfo, 0)
	for _, namespace := range migration.Spec.Namespaces {
		pvcList, err := k8s.Instance().GetPersistentVolumeClaims(namespace, migration.Spec.Selectors)
		if err != nil {
			return nil, fmt.Errorf("error getting list of volumes to migrate: %v", err)
		}
		for _, pvc := range pvcList.Items {
			if !p.OwnsPVC(&pvc) {
				continue
			}
			volumeInfo := &stork_crd.VolumeInfo{}
			volumeInfo.PersistentVolumeClaim = pvc.Name
			volumeInfo.Namespace = pvc.Namespace
			volumeInfos = append(volumeInfos, volumeInfo)

			volume, err := k8s.Instance().GetVolumeForPersistentVolumeClaim(&pvc)
			if err != nil {
				volumeInfo.Status = stork_crd.MigrationStatusFailed
				volumeInfo.Reason = fmt.Sprintf("Error getting volume for PVC: %v", err)
				logrus.Errorf("%v: %v", pvc.Name, volumeInfo.Reason)
				continue
			}
			volumeInfo.Volume = volume
			err = p.volDriver.CloudMigrateStart(&api.CloudMigrateStartRequest{
				Operation: api.CloudMigrate_MigrateVolume,
				ClusterId: clusterPair.Status.RemoteStorageID,
				TargetId:  volume,
			})
			if err != nil {
				volumeInfo.Status = stork_crd.MigrationStatusFailed
				volumeInfo.Reason = fmt.Sprintf("Error starting migration for volume: %v", err)
				logrus.Errorf("%v: %v", pvc.Name, volumeInfo.Reason)
				continue
			}
			volumeInfo.Status = stork_crd.MigrationStatusInProgress
			volumeInfo.Reason = fmt.Sprintf("Volume migration has started")
		}
	}

	return volumeInfos, nil
}

func (p *portworx) GetMigrationStatus(migration *stork_crd.Migration) ([]*stork_crd.VolumeInfo, error) {
	status, err := p.volDriver.CloudMigrateStatus()
	if err != nil {
		return nil, err
	}

	clusterPair, err := k8s.Instance().GetClusterPair(migration.Spec.ClusterPair)
	if err != nil {
		return nil, fmt.Errorf("error getting clusterpair: %v", err)
	}

	clusterInfo, ok := status.Info[clusterPair.Status.RemoteStorageID]
	if !ok {
		return nil, fmt.Errorf("migration status not found for remote cluster %v", clusterPair.Status.RemoteStorageID)
	}

	for _, vInfo := range migration.Status.Volumes {
		found := false
		for _, mInfo := range clusterInfo.List {
			if vInfo.Volume == mInfo.LocalVolumeName {
				found = true
				if mInfo.Status == api.CloudMigrate_Failed {
					vInfo.Status = stork_crd.MigrationStatusFailed
					vInfo.Reason = fmt.Sprintf("Migration %v failed for volume", mInfo.CurrentStage)
				} else if mInfo.CurrentStage == api.CloudMigrate_Done &&
					mInfo.Status == api.CloudMigrate_Complete {
					vInfo.Status = stork_crd.MigrationStatusSuccessful
					vInfo.Reason = fmt.Sprintf("Migration successful for volume")
				}
				break
			}
		}

		// If we didn't get the status for a volume mark it as failed
		if !found {
			vInfo.Status = stork_crd.MigrationStatusFailed
		}
	}

	return migration.Status.Volumes, nil
}

func (p *portworx) CancelMigration(migration *stork_crd.Migration) error {
	logrus.Warnf("Canceling migration not supported for Portworx, ignoring")
	return nil
}

func (p *portworx) UpdateMigratedPersistentVolumeSpec(
	object runtime.Unstructured,
) (runtime.Unstructured, error) {
	portworxSpec, err := collections.GetMap(object.UnstructuredContent(), "spec.portworxVolume")
	if err != nil {
		return nil, err
	}

	metadata, err := meta.Accessor(object)
	if err != nil {
		return nil, err
	}

	portworxSpec["volumeID"] = metadata.GetName()
	return object, nil
}

func init() {
	if err := storkvolume.Register(driverName, &portworx{}); err != nil {
		logrus.Panicf("Error registering portworx volume driver: %v", err)
	}
}
