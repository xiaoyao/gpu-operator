/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/NVIDIA/k8s-operator-libs/pkg/consts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/v1alpha1"
	"github.com/NVIDIA/gpu-operator/controllers/clusterinfo"
	"github.com/NVIDIA/gpu-operator/internal/state"
)

// NVIDIADriverReconciler reconciles a NVIDIADriver object
type NVIDIADriverReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	ClusterInfo clusterinfo.Interface

	stateManager state.Manager
}

//+kubebuilder:rbac:groups=nvidia.com,resources=nvidiadrivers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nvidia.com,resources=nvidiadrivers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=nvidia.com,resources=nvidiadrivers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NVIDIADriver object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *NVIDIADriverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(consts.LogLevelInfo).Info("Reconciling NVIDIADriver")

	// Get the NvidiaDriver instance from this request
	instance := &nvidiav1alpha1.NVIDIADriver{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		logger.V(consts.LogLevelError).Error(err, "Error getting NVIDIADriver object")
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Get the singleton NVIDIA ClusterPolicy object in the cluster.
	clusterPolicyList := &gpuv1.ClusterPolicyList{}
	err = r.Client.List(ctx, clusterPolicyList)
	if err != nil {
		logger.V(consts.LogLevelError).Error(err, "error getting ClusterPolicyList")
		return reconcile.Result{}, fmt.Errorf("error getting ClusterPolicyList: %v", err)
	}

	if len(clusterPolicyList.Items) == 0 {
		logger.V(consts.LogLevelError).Error(nil, "no ClusterPolicy object found in the cluster")
		return reconcile.Result{}, fmt.Errorf("no ClusterPolicy object found in the cluster")
	}
	clusterPolicyInstance := clusterPolicyList.Items[0]

	// Create a new InfoCatalog which is a generic interface for passing information to state managers
	infoCatalog := state.NewInfoCatalog()

	// Add an entry for ClusterInfo, which was collected before the NVIDIADriver controller was started
	infoCatalog.Add(state.InfoTypeClusterInfo, r.ClusterInfo)

	// Add an entry for Clusterpolicy, which is needed to deploy the driver daemonset
	infoCatalog.Add(state.InfoTypeClusterPolicyCR, clusterPolicyInstance)

	// Sync state and update status
	managerStatus := r.stateManager.SyncState(ctx, instance, infoCatalog)

	// TODO: update CR status
	// r.updateCrStatus(ctx, instance, managerStatus)

	if managerStatus.Status != state.SyncStateReady {
		logger.Info("NVIDIADriver instance is not ready")
		return reconcile.Result{RequeueAfter: time.Second * 5}, nil
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NVIDIADriverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create state manager
	stateManager, err := state.NewManager(
		nvidiav1alpha1.NVIDIADriverCRDName,
		mgr.GetClient(),
		mgr.GetScheme())
	if err != nil {
		return fmt.Errorf("error creating state manager: %v", err)
	}
	r.stateManager = stateManager

	// Create a new NVIDIADriver controller
	c, err := controller.New("nvidia-driver-controller", mgr, controller.Options{
		Reconciler:              r,
		MaxConcurrentReconciles: 1,
		RateLimiter:             workqueue.NewItemExponentialFailureRateLimiter(minDelayCR, maxDelayCR),
	})
	if err != nil {
		return err
	}

	// Watch for changes to the primary resource NVIDIaDriver
	err = c.Watch(source.Kind(mgr.GetCache(), &nvidiav1alpha1.NVIDIADriver{}), &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to ClusterPolicy. Whenever an event is generated for ClusterPolicy, enqueue
	// a reconcile request for all NVIDIADriver instances.
	mapFn := func(ctx context.Context, a client.Object) []reconcile.Request {
		logger := log.FromContext(ctx)
		opts := []client.ListOption{}
		list := &nvidiav1alpha1.NVIDIADriverList{}

		err := mgr.GetClient().List(ctx, list, opts...)
		if err != nil {
			logger.Error(err, "Unable to list NVIDIADriver resources")
			return []reconcile.Request{}
		}

		reconcileRequests := []reconcile.Request{}
		for _, nvidiaDriver := range list.Items {
			reconcileRequests = append(reconcileRequests,
				reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      nvidiaDriver.ObjectMeta.GetName(),
						Namespace: nvidiaDriver.ObjectMeta.GetNamespace(),
					},
				})
		}

		return reconcileRequests
	}

	err = c.Watch(source.Kind(mgr.GetCache(), &gpuv1.ClusterPolicy{}), handler.EnqueueRequestsFromMapFunc(mapFn))
	if err != nil {
		return err
	}

	// Watch for changes to secondary resources which each state manager manages
	watchSources := stateManager.GetWatchSources(mgr)
	for _, watchSource := range watchSources {
		err = c.Watch(watchSource, handler.EnqueueRequestForOwner(
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&nvidiav1alpha1.NVIDIADriver{},
			handler.OnlyControllerOwner()))

		if err != nil {
			return fmt.Errorf("error setting up Watch for source type%v: %w", watchSource, err)
		}
	}

	return nil
}
