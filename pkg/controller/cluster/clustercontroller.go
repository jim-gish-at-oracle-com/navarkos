/*
Copyright 2017 The Kubernetes Authors.
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

package cluster

import (
	"strconv"
	"time"

	"fmt"
	"github.com/golang/glog"
	common "github.com/kubernetes-incubator/navarkos/pkg/common"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	v1beta1 "k8s.io/kubernetes/federation/apis/federation/v1beta1"
	clustercache "k8s.io/kubernetes/federation/client/cache"
	federationclientset "k8s.io/kubernetes/federation/client/clientset_generated/federation_clientset"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/controller"
)

//ReconcileOnClusterChange defines callback to DeploymentController
type ReconcileOnClusterChange func()

//ClusterController provides for management of cluster
type ClusterController struct {
	knownClusterSet sets.String

	// federationClient used to operate cluster
	federationClient federationclientset.Interface

	// clusterMonitorPeriod is the period for updating status of cluster
	clusterMonitorPeriod time.Duration
	// clusterClusterStatusMap is a mapping of clusterName and cluster status of last sampling
	clusterClusterStatusMap map[string]v1beta1.ClusterStatus

	// clusterKubeClientMap is a mapping of clusterName and restclient
	clusterKubeClientMap map[string]ClusterClient

	// cluster framework and store
	clusterController cache.Controller
	clusterStore      clustercache.StoreToClusterLister

	//call back to dpmt controller reconcile
	OnClusterChange ReconcileOnClusterChange
}

// StartClusterController starts a new cluster controller
func StartClusterController(config *restclient.Config, stopChan <-chan struct{}, clusterMonitorPeriod time.Duration) *ClusterController {
	restclient.AddUserAgent(config, "cluster-controller")
	client := federationclientset.NewForConfigOrDie(config)
	controller := newClusterController(client, clusterMonitorPeriod)
	glog.Infof("Starting navarkos cluster controller")
	controller.Run(stopChan)
	return controller
}

// newClusterController returns a new cluster controller
func newClusterController(federationClient federationclientset.Interface, clusterMonitorPeriod time.Duration) *ClusterController {
	cc := &ClusterController{
		knownClusterSet:         make(sets.String),
		federationClient:        federationClient,
		clusterMonitorPeriod:    clusterMonitorPeriod,
		clusterClusterStatusMap: make(map[string]v1beta1.ClusterStatus),
		clusterKubeClientMap:    make(map[string]ClusterClient),
	}
	cc.clusterStore.Store, cc.clusterController = cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return cc.federationClient.Federation().Clusters().List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return cc.federationClient.Federation().Clusters().Watch(options)
			},
		},
		&v1beta1.Cluster{},
		controller.NoResyncPeriodFunc(),
		cache.ResourceEventHandlerFuncs{
			DeleteFunc: cc.delFromClusterSet,
			UpdateFunc: cc.updateCluster,
			AddFunc:    cc.addToClusterSet,
		},
	)
	return cc
}

// IsSynced returns if clusterController HasSynced or not
func (cc *ClusterController) IsSynced() bool {
	isSynced := cc.clusterController.HasSynced()
	return isSynced
}

// GetReadyClusters returns all clusters for which the sub-informers are run.
func (cc *ClusterController) GetReadyClusters() ([]*v1beta1.Cluster, error) {
	items, err := cc.clusterStore.List()

	if err != nil {
		return nil, err
	}
	result := make([]*v1beta1.Cluster, 0, len(items.Items))
	for _, cluster := range items.Items {
		if cc.isClusterReady(&cluster) {
			var clusterCopy *v1beta1.Cluster = cluster.DeepCopy()
			result = append(result, clusterCopy)
		}
	}
	return result, nil
}

// GetUnreadyClusters returns all clusters for which the sub-informers are not run.
func (cc *ClusterController) GetUnreadyClusters() ([]*v1beta1.Cluster, error) {
	items, err := cc.clusterStore.List()

	if err != nil {
		return nil, err
	}
	result := make([]*v1beta1.Cluster, 0, len(items.Items))
	for _, cluster := range items.Items {
		if !cc.isClusterReady(&cluster) {
			var clusterCopy *v1beta1.Cluster = cluster.DeepCopy()
			result = append(result, clusterCopy)
		}
	}
	return result, nil
}

// delFromClusterSet delete a cluster from clusterSet and
// delete the corresponding restclient from the map clusterKubeClientMap
func (cc *ClusterController) delFromClusterSet(obj interface{}) {
	cluster := obj.(*v1beta1.Cluster)
	cc.delFromClusterSetByName(cluster.Name)
}

// delFromClusterSetByName delete a cluster from clusterSet by name and
// delete the corresponding restclient from the map clusterKubeClientMap
func (cc *ClusterController) delFromClusterSetByName(clusterName string) {
	glog.V(1).Infof("ClusterController observed a cluster deletion: %v", clusterName)
	cc.knownClusterSet.Delete(clusterName)
	delete(cc.clusterKubeClientMap, clusterName)

	if cc.OnClusterChange != nil {
		cc.OnClusterChange()
	}

}

func (cc *ClusterController) updateCluster(oldObj, obj interface{}) {
	cluster := obj.(*v1beta1.Cluster)
	oldCluster := oldObj.(*v1beta1.Cluster)
	glog.V(1).Infof("ClusterController observed an update on cluster: %v", cluster.Name)
	if !cc.knownClusterSet.Has(cluster.Name) {
		cc.knownClusterSet.Insert(cluster.Name)
	}
	oldClusterStatus, ok := oldCluster.Annotations[common.NavarkosClusterStateKey]
	if !ok {
		oldClusterStatus = ""
	}

	if cluster.ObjectMeta.DeletionTimestamp != nil {
		//this cluster is being deleted
		cc.delFromClusterSetByName(cluster.Name)
		return
	}

	// create the restclient of ready cluster
	if newClusterStatus, ok := cluster.Annotations[common.NavarkosClusterStateKey]; ok {
		if oldClusterStatus != common.NavarkosClusterStateReady && newClusterStatus == common.NavarkosClusterStateReady {
			if _, ok := cc.clusterKubeClientMap[cluster.Name]; !ok {
				glog.Infof("CLuster odx state is ready, will create clientset config: %v", cluster.Name)
				cc.updateClusterClient(cluster)
			}
		} else if newClusterStatus == common.NavarkosClusterStateOffline {
			if _, ok := cc.clusterKubeClientMap[cluster.Name]; ok {
				delete(cc.clusterKubeClientMap, cluster.Name)
			}
		}
	}
}

// addToClusterSet insert the new cluster to clusterSet and create a corresponding
// restclient to map clusterKubeClientMap
func (cc *ClusterController) addToClusterSet(obj interface{}) {
	cluster := obj.(*v1beta1.Cluster)
	glog.V(1).Infof("ClusterController observed a new cluster: %v", cluster.Name)
	cc.knownClusterSet.Insert(cluster.Name)
	// create the restclient of cluster

	if clusterStatus, ok := cluster.Annotations[common.NavarkosClusterStateKey]; ok {
		if clusterStatus == common.NavarkosClusterStateReady {
			cc.updateClusterClient(cluster)
		} else {
			glog.Infof("CLuster odx state is not ready, will skip setting client config: %v", cluster.Name)

		}
	} else {
		cc.updateClusterClient(cluster)
	}

}

// Run begins watching and syncing.
func (cc *ClusterController) Run(stopChan <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go cc.clusterController.Run(stopChan)
	// monitor cluster status periodically
	go wait.Until(func() {
		if err := cc.UpdateClusterStatus(); err != nil {
			glog.Errorf("Error monitoring cluster status: %v", err)
		}
	}, cc.clusterMonitorPeriod, stopChan)
}

func (cc *ClusterController) updateClusterClient(cluster *v1beta1.Cluster) (*ClusterClient, error) {
	glog.Infof("It's a new cluster, a cluster client will be created")
	client, err := NewClusterClientSet(cluster)
	if err != nil || client == nil {
		glog.Errorf("Failed to create cluster client for cluster %v, err: %v", cluster.Name, err)
		return nil, err
	}
	cc.clusterKubeClientMap[cluster.Name] = *client
	return client, nil
}

//GetClusterClient retrieves the client for given cluster based on config
func (cc *ClusterController) GetClusterClient(cluster *v1beta1.Cluster) (*ClusterClient, error) {
	clusterClient, found := cc.clusterKubeClientMap[cluster.Name]
	if !found {
		glog.Infof("It's a new cluster, a cluster client will be created")
		return cc.updateClusterClient(cluster)
	}
	client := &clusterClient
	return client, nil
}

//GetClusterDeployment looks for specified deployment in given cluster
func (cc *ClusterController) GetClusterDeployment(cluster *v1beta1.Cluster, namespace string, name string) (*extensions.Deployment, error) {

	clusterClient, found := cc.clusterKubeClientMap[cluster.Name]
	if !found {
		err := fmt.Errorf("Can not get client for cluster %v", cluster.Name)
		return nil, err
	}

	if !cc.isClusterReady(cluster) {
		err := fmt.Errorf("Cluster %v is not in ready state, can not get deployments", cluster.Name)
		return nil, err
	}

	return clusterClient.kubeClient.Extensions().Deployments(namespace).Get(name, metav1.GetOptions{})

}

// UpdateClusterStatus checks cluster status and get the metrics from cluster's restapi
func (cc *ClusterController) UpdateClusterStatus() error {

	glog.V(4).Infof("!!!Going to refresh cluster stats for all clusters")
	clusters, err := cc.federationClient.Federation().Clusters().List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, cluster := range clusters.Items {
		if !cc.knownClusterSet.Has(cluster.Name) {
			cc.addToClusterSet(&cluster)
		}
	}

	// If there's a difference between lengths of known clusters and observed clusters
	if len(cc.knownClusterSet) != len(clusters.Items) {
		observedSet := make(sets.String)
		for _, cluster := range clusters.Items {
			observedSet.Insert(cluster.Name)
		}
		deleted := cc.knownClusterSet.Difference(observedSet)
		for clusterName := range deleted {
			cc.delFromClusterSetByName(clusterName)
		}
	}

	for _, clusterObj := range clusters.Items {

		if clusterObj.ObjectMeta.DeletionTimestamp != nil || !cc.isClusterReady(&clusterObj) {
			glog.V(4).Infof("Cluster %v is not in ready state, will skipp collecting stats", clusterObj.Name)
			continue
		}

		glog.V(4).Infof("Going to refresh cluster stats for %v", clusterObj.Name)

		newStats, err := cc.getClusterStats(&clusterObj)
		if err != nil {
			glog.Warningf("Failed to get metrics for cluster %s", clusterObj.Name)
			continue
		}

		//refresh cluster
		cluster, err := cc.federationClient.FederationV1beta1().Clusters().Get(clusterObj.Name, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Failed to refresh cluster %s", cluster.Name)
			continue
		}
		if !cc.isClusterReady(cluster) {
			glog.V(4).Infof("Cluster %v is not in ready state, will skipp collecting stats", cluster.Name)
			continue
		}

		updateCluster := false

		if cluster.Annotations == nil {
			cluster.Annotations = make(map[string]string)
		}

		if !cc.IsCapacityDataPresent(cluster) {
			glog.V(4).Infof("Capacity data is not present on cluster %s", cluster.Name)
			cc.updateClusterStats(cluster, newStats)
			updateCluster = true
		} else {

			if cc.isStatsChanged(newStats, cluster) {
				cc.updateClusterStats(cluster, newStats)
				updateCluster = true
			}
			updateCluster = updateCluster || cc.markClusterToShutDownIfNoPods(cluster)
			updateCluster = updateCluster || cc.shutDownClusterIfTTLExpired(cluster)
			updateCluster = updateCluster || cc.markClusterForScalingIfPodsHitThresholds(cluster)
		}
		if updateCluster {
			updatedCluster, err := cc.federationClient.Federation().Clusters().Update(cluster)
			if err != nil {
				glog.Errorf("Failed to add annotations to cluster %s, error is : %v", updatedCluster.Name, err)
				// Don't return err here, as we want to continue processing remaining clusters.
				continue
			} else {
				glog.V(4).Infof("Successfully added annotations to cluster %s", updatedCluster.Name)
			}

			if cc.OnClusterChange != nil {
				cc.OnClusterChange()
			}
		} else {
			glog.V(4).Infof("Cluster %s doesn't require pods annotation update", cluster.Name)
		}
	}
	return nil
}

//IsCapacityDataPresent retrieves the capacity info from cluster annotations
func (cc *ClusterController) IsCapacityDataPresent(cluster *v1beta1.Cluster) bool {

	_, allocatablePresent := cluster.Annotations[common.NavarkosClusterCapacityAllocatablePodsKey]
	_, capacityPresent := cluster.Annotations[common.NavarkosClusterCapacityPodsKey]
	_, usedPresent := cluster.Annotations[common.NavarkosClusterCapacityUsedPodsKey]
	_, systemUsedPresent := cluster.Annotations[common.NavarkosClusterCapacitySystemPodsKey]

	return allocatablePresent && capacityPresent && usedPresent && systemUsedPresent
}

//IsClusterScaling confirm scalability based on cluster annotations
func (cc *ClusterController) IsClusterScaling(cluster *v1beta1.Cluster) bool {

	navarkosState, found := cluster.Annotations[common.NavarkosClusterStateKey]

	if !found {
		//this is not navarkos managed cluster
		return false
	}

	if navarkosState == common.NavarkosClusterStatePendingScaleUp ||
		navarkosState == common.NavarkosClusterStatePendingScaleDown ||
		navarkosState == common.NavarkosClusterStateScalingDown ||
		navarkosState == common.NavarkosClusterStateScalingUp {
		// cluster is scaling
		return true
	}

	return false
}

func (cc *ClusterController) isStatsChanged(newStats map[string]int, cluster *v1beta1.Cluster) bool {
	for metricName, newValue := range newStats {
		oldValue := getAnnotationIntegerValue(cluster, metricName, 0)
		if oldValue != newValue {
			glog.V(4).Infof("%v value changed from %v to %v on cluster %s", metricName, oldValue, newValue, cluster.Name)
			return true
		}
	}
	return false
}

func (cc *ClusterController) updateClusterStats(cluster *v1beta1.Cluster, stats map[string]int) {
	for metricName, value := range stats {
		cluster.Annotations[metricName] = strconv.Itoa(value)
	}
}

func (cc *ClusterController) getClusterStats(cluster *v1beta1.Cluster) (map[string]int, error) {
	stats := make(map[string]int)

	clusterClient, err := cc.GetClusterClient(cluster)

	if clusterClient == nil {
		return nil, err
	}

	currentAllocatablePods, currentCapacityPods, err := clusterClient.GetClusterPods()
	if err != nil {
		glog.Warningf("Failed to get allocatablePods and capacityPods of cluster: %s, error is : %v", cluster.Name, err)
		return nil, err
	}
	currentUsedPods, err := cc.getCountOfUsedPods(cluster.Name, *clusterClient)
	if err != nil {
		glog.Warningf("Failed to get list of used pods for cluster: %s, error is : %v", cluster.Name, err)
		return nil, err
	}
	glog.V(4).Infof("Total number of used pods is %v for cluster %s", currentUsedPods, cluster.Name)

	usedSysPods, err := cc.getCountOfUsedPodsSystemNS(*clusterClient)
	if err != nil {
		glog.Warningf("Failed to get list of system pods for cluster: %s, error is : %v", cluster.Name, err)
		return nil, err
	}

	stats[common.NavarkosClusterCapacityAllocatablePodsKey] = int(currentAllocatablePods)
	stats[common.NavarkosClusterCapacityPodsKey] = int(currentCapacityPods)
	stats[common.NavarkosClusterCapacityUsedPodsKey] = int(currentUsedPods)
	stats[common.NavarkosClusterCapacitySystemPodsKey] = int(usedSysPods)

	return stats, nil
}

func (cc *ClusterController) getCountOfUsedPods(clusterName string, clusterClient ClusterClient) (int, error) {
	usedPodsList, err := clusterClient.kubeClient.Core().Pods(api.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		glog.Warningf("Failed to get list of used pods for cluster: %s, error is : %v", clusterName, err)
		return 0, err
	}

	var count int

	for _, pod := range usedPodsList.Items {
		if pod.Status.Phase == api.PodPending || pod.Status.Phase == api.PodRunning {
			count++
		}
	}
	return count, nil
}

func (cc *ClusterController) getCountOfUsedPodsSystemNS(clusterClient ClusterClient) (int, error) {
	usedPodsSystemNS, err := clusterClient.kubeClient.Core().Pods("kube-system").List(metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Failed to get usedPodsFederationSystemNS: %v", err)
		return 0, err
	}
	// TODO: make the list of system name spaces configurable
	var usedPodsBmcNS int
	usedPodsBmcNSList, err := clusterClient.kubeClient.Core().Pods("oracle-bmc").List(metav1.ListOptions{})
	if err != nil {
		glog.Errorf("Failed to get usedPodsOracleBmcNS: %v", err)
	} else {
		usedPodsBmcNS = len(usedPodsBmcNSList.Items)
	}

	return len(usedPodsSystemNS.Items) + usedPodsBmcNS, nil
}

func (cc *ClusterController) markClusterToShutDownIfNoPods(cluster *v1beta1.Cluster) bool {

	usedPods := getAnnotationIntegerValue(cluster, common.NavarkosClusterCapacityUsedPodsKey, 0)
	usedPodsSystemNS := getAnnotationIntegerValue(cluster, common.NavarkosClusterCapacitySystemPodsKey, 0)

	isNoUserPodsLeft := usedPods == usedPodsSystemNS
	if isNoUserPodsLeft {
		if _, ok := cluster.Annotations[common.NavarkosClusterShutdownStartTimeKey]; !ok {
			gasClusterState := cluster.Annotations[common.NavarkosClusterStateKey]
			if gasClusterState == common.NavarkosClusterStateReady {
				cluster.Annotations[common.NavarkosClusterShutdownStartTimeKey] = time.Now().UTC().Format(time.UnixDate)
				glog.V(4).Infof("markClusterToShutDownIfNoPods will mark Cluster %s for shrinking after %d sec. starting on %s",
					cluster.Name,
					getAnnotationIntegerValue(cluster, common.NavarkosClusterTimeToLiveBeforeShutdownKey, common.NavarkosDefaultClusterTimeToLiveBeforeShutdown),
					cluster.Annotations[common.NavarkosClusterShutdownStartTimeKey])
				return true
			}
		}
	} else {
		glog.V(10).Infof("markClusterToShutDownIfNoPods odx.io/cluster-totalUsedPods is not 0, " +
			"clusterstate not changing ")
		// New Pods were created to this cluster so remove TTL monitoring if it exists
		if _, ok := cluster.Annotations[common.NavarkosClusterShutdownStartTimeKey]; ok {
			delete(cluster.Annotations, common.NavarkosClusterShutdownStartTimeKey)
			return true
		}
	}
	return false
}

func (cc *ClusterController) shutDownClusterIfTTLExpired(cluster *v1beta1.Cluster) bool {
	if clusterShutdownStartTimeInString, ok := cluster.Annotations[common.NavarkosClusterShutdownStartTimeKey]; ok {
		if clusterShutdownStartTime, err := time.Parse(time.UnixDate, clusterShutdownStartTimeInString); err == nil {
			clusterTimeToLiveBeforeShutdown := getAnnotationIntegerValue(cluster, common.NavarkosClusterTimeToLiveBeforeShutdownKey, common.NavarkosDefaultClusterTimeToLiveBeforeShutdown)
			if clusterTimeToLiveBeforeShutdown > 0 && int(time.Since(clusterShutdownStartTime).Seconds()) >= clusterTimeToLiveBeforeShutdown {
				glog.V(4).Infof("TTL of %d sec. has expired for cluster %s and hence will be set to %s for shrinking",
					clusterTimeToLiveBeforeShutdown, cluster.Name, common.NavarkosClusterStatePendingShutdown)
				cluster.Annotations[common.NavarkosClusterStateKey] = common.NavarkosClusterStatePendingShutdown
				delete(cluster.Annotations, common.NavarkosClusterShutdownStartTimeKey)
				return true
			}
		}
	}
	return false
}

func (cc *ClusterController) markClusterForScalingIfPodsHitThresholds(cluster *v1beta1.Cluster) bool {

	usedPods := getAnnotationIntegerValue(cluster, common.NavarkosClusterCapacityUsedPodsKey, 0)
	usedSysPods := getAnnotationIntegerValue(cluster, common.NavarkosClusterCapacitySystemPodsKey, 0)
	capacityPods := getAnnotationIntegerValue(cluster, common.NavarkosClusterCapacityPodsKey, 0)

	if autoScaleEnabledStr, ok := cluster.Annotations[common.NavarkosClusterAutoScaleKey]; !ok {
		//autoScale is not enabled
		return false
	} else {
		autoScaleEnabled, err := strconv.ParseBool(autoScaleEnabledStr)
		if !autoScaleEnabled || err != nil {
			//autoScale is not enabled
			return false
		}
	}
	gasClusterState := cluster.Annotations[common.NavarkosClusterStateKey]
	if gasClusterState == common.NavarkosClusterStateReady {
		scaleUpCapacityThreshold := getAnnotationIntegerValue(cluster, common.NavarkosClusterScaleUpCapacityThresholdKey, common.NavarkosDefaultClusterScaleUpCapacityThreshold)
		scaleDownCapacityThreshold := getAnnotationIntegerValue(cluster, common.NavarkosClusterScaleDownCapacityThresholdKey, common.NavarkosClusterScaleDownCapacityThreshold)
		userPods := usedPods - usedSysPods
		userCapacityPods := capacityPods - usedSysPods
		usedOverCapacity := (userPods * 100) / userCapacityPods
		switch {
		case usedOverCapacity >= scaleUpCapacityThreshold:
			glog.V(4).Infof("markClusterForScalingIfPodsHitThresholds usedOverCapacity >= %d%%, usedOverCapacity: %d%%."+
				" Cluster %s will be marked for scale up.",
				scaleUpCapacityThreshold, usedOverCapacity, cluster.Name)
			cluster.Annotations[common.NavarkosClusterStateKey] = common.NavarkosClusterStatePendingScaleUp
			return true
		case usedOverCapacity <= scaleDownCapacityThreshold:
			glog.V(4).Infof("markClusterForScalingIfPodsHitThresholds usedOverCapacity <= %d%%, usedOverCapacity: %d%%."+
				" Cluster %s will be marked for scale down.",
				scaleDownCapacityThreshold, usedOverCapacity, cluster.Name)
			cluster.Annotations[common.NavarkosClusterStateKey] = common.NavarkosClusterStatePendingScaleDown
			return true
		default:
			glog.V(4).Infof("markClusterForScalingIfPodsHitThresholds usedOverCapacity < %d%% and > %d%%, usedOverCapacity: %d%%, total usedPods: %d, systemPods %d for cluster %s.",
				scaleUpCapacityThreshold, scaleDownCapacityThreshold, usedOverCapacity, usedPods, usedSysPods, cluster.Name)
		}
	}
	return false
}

func getAnnotationIntegerValue(cluster *v1beta1.Cluster, annotationName string, defaultValue int) int {
	annotationOriginalValue, ok := cluster.Annotations[annotationName]
	if ok && annotationOriginalValue != "" {
		annotationConvertedValue, err := strconv.Atoi(annotationOriginalValue)
		if err == nil {
			return annotationConvertedValue
		}
	}
	return defaultValue
}

//GetCluster retrieves the cluster from federation store
func (cc *ClusterController) GetCluster(clusterName string) (*v1beta1.Cluster, error) {
	return cc.federationClient.FederationV1beta1().Clusters().Get(clusterName, metav1.GetOptions{})
}

//UpdateCluster updates the cluster in federation store
func (cc *ClusterController) UpdateCluster(cluster *v1beta1.Cluster) error {
	_, err := cc.federationClient.Federation().Clusters().Update(cluster)
	return err
}

func (cc *ClusterController) isClusterReady(cluster *v1beta1.Cluster) bool {
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == v1beta1.ClusterReady {
			if condition.Status == apiv1.ConditionTrue {
				return true
			}
		}
	}
	return false
}
