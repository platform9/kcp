/*
Copyright 2022 The KCP Authors.

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

package apireconciler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	kcpcache "github.com/kcp-dev/apimachinery/pkg/cache"
	"github.com/kcp-dev/logicalcluster/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	workloadv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/workload/v1alpha1"
	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	apisinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/apis/v1alpha1"
	workloadinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions/workload/v1alpha1"
	apislisters "github.com/kcp-dev/kcp/pkg/client/listers/apis/v1alpha1"
	workloadlisters "github.com/kcp-dev/kcp/pkg/client/listers/workload/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/logging"
	"github.com/kcp-dev/kcp/pkg/virtual/framework/dynamic/apidefinition"
	dynamiccontext "github.com/kcp-dev/kcp/pkg/virtual/framework/dynamic/context"
)

const (
	ControllerName                     = "kcp-virtual-syncer-api-reconciler"
	indexSyncTargetsByExport           = ControllerName + "ByExport"
	indexAPIExportsByAPIResourceSchema = ControllerName + "ByAPIResourceSchema"
)

type CreateAPIDefinitionFunc func(syncTargetWorkspace logicalcluster.Name, syncTargetName string, apiResourceSchema *apisv1alpha1.APIResourceSchema, version string, identityHash string) (apidefinition.APIDefinition, error)

func NewAPIReconciler(
	kcpClusterClient kcpclient.ClusterInterface,
	syncTargetInformer workloadinformers.SyncTargetInformer,
	apiResourceSchemaInformer apisinformers.APIResourceSchemaInformer,
	apiExportInformer apisinformers.APIExportInformer,
	createAPIDefinition CreateAPIDefinitionFunc,
) (*APIReconciler, error) {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName)

	c := &APIReconciler{
		kcpClusterClient: kcpClusterClient,

		syncTargetLister:  syncTargetInformer.Lister(),
		syncTargetIndexer: syncTargetInformer.Informer().GetIndexer(),

		apiResourceSchemaLister: apiResourceSchemaInformer.Lister(),

		apiExportLister:  apiExportInformer.Lister(),
		apiExportIndexer: apiExportInformer.Informer().GetIndexer(),

		queue: queue,

		createAPIDefinition: createAPIDefinition,

		apiSets: map[dynamiccontext.APIDomainKey]apidefinition.APIDefinitionSet{},
	}

	if err := syncTargetInformer.Informer().AddIndexers(cache.Indexers{
		indexSyncTargetsByExport: indexSyncTargetsByExports,
	}); err != nil {
		return nil, err
	}

	if err := apiExportInformer.Informer().AddIndexers(cache.Indexers{
		indexAPIExportsByAPIResourceSchema: indexAPIExportsByAPIResourceSchemas,
	}); err != nil {
		return nil, err
	}

	logger := logging.WithReconciler(klog.Background(), ControllerName)

	syncTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { c.enqueueSyncTarget(obj, logger, "") },
		UpdateFunc: func(old, obj interface{}) {
			oldCluster := old.(*workloadv1alpha1.SyncTarget)
			newCluster := obj.(*workloadv1alpha1.SyncTarget)

			// only enqueue when syncedResource is changed.
			if !equality.Semantic.DeepEqual(oldCluster.Status.SyncedResources, newCluster.Status.SyncedResources) {
				c.enqueueSyncTarget(obj, logger, "")
			}
		},
		DeleteFunc: func(obj interface{}) { c.enqueueSyncTarget(obj, logger, "") },
	})

	apiResourceSchemaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger) },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIResourceSchema(obj, logger) },
	})

	apiExportInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		UpdateFunc: func(_, obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
		DeleteFunc: func(obj interface{}) { c.enqueueAPIExport(obj, logger, "") },
	})

	return c, nil
}

// APIReconciler is a controller watching APIExports, APIResourceSchemas and SyncTargets, and updates the
// API definitions driving the virtual workspace.
type APIReconciler struct {
	kcpClusterClient kcpclient.ClusterInterface

	syncTargetLister  workloadlisters.SyncTargetLister
	syncTargetIndexer cache.Indexer

	apiResourceSchemaLister apislisters.APIResourceSchemaLister

	apiExportLister  apislisters.APIExportLister
	apiExportIndexer cache.Indexer

	queue workqueue.RateLimitingInterface

	createAPIDefinition CreateAPIDefinitionFunc

	mutex   sync.RWMutex // protects the map, not the values!
	apiSets map[dynamiccontext.APIDomainKey]apidefinition.APIDefinitionSet
}

func (c *APIReconciler) enqueueSyncTarget(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := kcpcache.MetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	logging.WithQueueKey(logger, key).V(2).Info(fmt.Sprintf("queueing SyncTarget%s", logSuffix))
	c.queue.Add(key)
}

func (c *APIReconciler) enqueueAPIExport(obj interface{}, logger logr.Logger, logSuffix string) {
	key, err := kcpcache.DeletionHandlingMetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	synctargets, err := c.syncTargetIndexer.ByIndex(indexSyncTargetsByExport, key)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	for _, obj := range synctargets {
		logger := logging.WithObject(logger, obj.(*workloadv1alpha1.SyncTarget))
		c.enqueueSyncTarget(obj, logger, " because of APIExport")
	}
}

// enqueueAPIResourceSchema maps an APIResourceSchema to APIExports for enqueuing.
func (c *APIReconciler) enqueueAPIResourceSchema(obj interface{}, logger logr.Logger) {
	key, err := kcpcache.DeletionHandlingMetaClusterNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	apiExports, err := c.apiExportIndexer.ByIndex(indexAPIExportsByAPIResourceSchema, key)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	for _, obj := range apiExports {
		logger := logging.WithObject(logger, obj.(*apisv1alpha1.APIExport))
		c.enqueueAPIExport(obj, logger, " because of APIResourceSchema")
	}
}

func (c *APIReconciler) startWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *APIReconciler) Start(ctx context.Context) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	logger := logging.WithReconciler(klog.FromContext(ctx), ControllerName)
	ctx = klog.NewContext(ctx, logger)
	logger.Info("Starting controller")
	defer logger.Info("Shutting down controller")

	go wait.Until(func() { c.startWorker(ctx) }, time.Second, ctx.Done())

	// stop all watches if the controller is stopped
	defer func() {
		c.mutex.Lock()
		defer c.mutex.Unlock()
		for _, sets := range c.apiSets {
			for _, v := range sets {
				v.TearDown()
			}
		}
	}()

	<-ctx.Done()
}

func (c *APIReconciler) ShutDown() {
	c.queue.ShutDown()
}

func (c *APIReconciler) processNextWorkItem(ctx context.Context) bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	if err := c.process(ctx, key); err != nil {
		runtime.HandleError(fmt.Errorf("%s: failed to sync %q, err: %w", ControllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)
	return true
}

func (c *APIReconciler) process(ctx context.Context, key string) error {
	apiDomainKey := dynamiccontext.APIDomainKey(key)

	logger := logging.WithQueueKey(klog.FromContext(ctx), key)
	ctx = klog.NewContext(ctx, logger)

	syncTarget, err := c.syncTargetLister.Get(key)
	if apierrors.IsNotFound(err) {
		c.removeAPIDefinitionSet(apiDomainKey)
		return nil
	}
	if err != nil {
		return err
	}

	if err := c.reconcile(ctx, apiDomainKey, syncTarget); err != nil {
		return err
	}

	return nil
}

func (c *APIReconciler) GetAPIDefinitionSet(_ context.Context, key dynamiccontext.APIDomainKey) (apidefinition.APIDefinitionSet, bool, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	apiSet, ok := c.apiSets[key]
	return apiSet, ok, nil
}

func (c *APIReconciler) removeAPIDefinitionSet(key dynamiccontext.APIDomainKey) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	delete(c.apiSets, key)
}
