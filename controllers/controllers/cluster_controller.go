package controllers

import (
	"context"
	"time"

	eksdv1alpha1 "github.com/aws/eks-distro-build-tooling/release/api/v1alpha1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/eks-anywhere/controllers/controllers/clusters"
	"github.com/aws/eks-anywhere/controllers/controllers/reconciler"
	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	c "github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/constants"
	"github.com/aws/eks-anywhere/pkg/executables"
	"github.com/aws/eks-anywhere/pkg/networking/cilium"
	"github.com/aws/eks-anywhere/pkg/networkutils"
	"github.com/aws/eks-anywhere/pkg/providers/vsphere"
	releasev1alpha1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

const (
	defaultRequeueTime   = time.Minute
	clusterFinalizerName = "clusters.anywhere.eks.amazonaws.com/finalizer"

	controlPlaneReadyCondition            clusterv1.ConditionType = "ControlPlaneReady"
	extraObjectsSpecPlaneAppliedCondition clusterv1.ConditionType = "ExtraObjectsSpecApplied"
	cniSpecAppliedCondition               clusterv1.ConditionType = "CNISpecApplied"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client    client.Client
	log       logr.Logger
	validator *vsphere.Validator
	defaulter *vsphere.Defaulter
	tracker   *remote.ClusterCacheTracker
}

func NewClusterReconciler(client client.Client, log logr.Logger, scheme *runtime.Scheme, govc *executables.Govc, tracker *remote.ClusterCacheTracker) *ClusterReconciler {
	validator := vsphere.NewValidator(govc, &networkutils.DefaultNetClient{})
	defaulter := vsphere.NewDefaulter(govc)

	return &ClusterReconciler{
		client:    client,
		log:       log,
		validator: validator,
		defaulter: defaulter,
		tracker:   tracker,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&anywherev1.Cluster{}).
		// Watches(&source.Kind{Type: &anywherev1.VSphereDatacenterConfig{}}, &handler.EnqueueRequestForObject{}).
		// Watches(&source.Kind{Type: &anywherev1.VSphereMachineConfig{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=anywhere.eks.amazonaws.com,resources=clusters;vspheredatacenterconfigs;vspheremachineconfigs;dockerdatacenterconfigs;bundles;awsiamconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=anywhere.eks.amazonaws.com,resources=oidcconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=anywhere.eks.amazonaws.com,resources=clusters/status;vspheredatacenterconfigs/status;vspheremachineconfigs/status;dockerdatacenterconfigs/status;bundles/status;awsiamconfigs/status,verbs=;get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=anywhere.eks.amazonaws.com,resources=clusters/finalizers;vspheredatacenterconfigs/finalizers;vspheremachineconfigs/finalizers;dockerdatacenterconfigs/finalizers;bundles/finalizers;awsiamconfigs/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=*,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=test,resources=test,verbs=get;list;watch;create;update;patch;delete;kill
// +kubebuilder:rbac:groups=distro.eks.amazonaws.com,resources=releases,verbs=get;list;watch
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := r.log.WithValues("cluster", req.NamespacedName)
	// Fetch the Cluster object
	cluster := &anywherev1.Cluster{}
	log.Info("Reconciling cluster", "name", req.NamespacedName)
	if err := r.client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(cluster, r.client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		// Always attempt to patch the object and status after each reconciliation.
		if err := patchHelper.Patch(ctx, cluster); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if cluster.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cluster, clusterFinalizerName) {
			controllerutil.AddFinalizer(cluster, clusterFinalizerName)
		}
	} else {
		return r.reconcileDelete(ctx, cluster)
	}

	// If the cluster is paused, return without any further processing.
	if cluster.IsReconcilePaused() {
		log.Info("Cluster reconciliation is paused")
		return ctrl.Result{}, nil
	}

	if cluster.IsSelfManaged() {
		log.Info("Ignoring self managed cluster")
		return ctrl.Result{}, nil
	}

	result, err := r.reconcile(ctx, cluster, log)
	if err != nil {
		failureMessage := err.Error()
		cluster.Status.FailureMessage = &failureMessage
		log.Error(err, "Failed to reconcile Cluster")
	}
	return result, err
}

func (r *ClusterReconciler) reconcile(ctx context.Context, cluster *anywherev1.Cluster, log logr.Logger) (ctrl.Result, error) {
	capiCluster := &clusterv1.Cluster{}
	capiClusterName := types.NamespacedName{Namespace: constants.EksaSystemNamespace, Name: cluster.Name}
	log.Info("Searching for CAPI cluster", "name", cluster.Name)
	if err := r.client.Get(ctx, capiClusterName, capiCluster); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: defaultRequeueTime,
		}, err
	}

	clusterProviderReconciler, err := clusters.BuildProviderReconciler(cluster.Spec.DatacenterRef.Kind, r.client, r.log, r.validator, r.defaulter, r.tracker)
	if err != nil {
		return ctrl.Result{}, err
	}

	reconcileResult, err := clusterProviderReconciler.Reconcile(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !conditions.IsTrue(capiCluster, controlPlaneReadyCondition) {
		log.Info("waiting for control plane to be ready", "cluster", capiCluster.Name, "kind", capiCluster.Kind)
		return ctrl.Result{
			RequeueAfter: defaultRequeueTime,
		}, err
	}

	bundles, err := r.bundles(ctx, cluster.Spec.ManagementCluster.Name, "default")
	if err != nil {
		return ctrl.Result{}, err
	}
	versionsBundle, err := c.GetVersionsBundle(cluster, bundles)
	if err != nil {
		return ctrl.Result{}, err
	}
	log.V(4).Info("Fetching eks-d manifest", "release name", versionsBundle.EksD.Name)
	eksd, err := r.eksdRelease(ctx, versionsBundle.EksD.Name, constants.EksaSystemNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	specWithBundles, _ := c.BuildSpecFromBundles(cluster, bundles, c.WithEksdRelease(eksd))
	if result, err := r.reconcileExtraObjects(ctx, cluster, capiCluster, specWithBundles); err != nil {
		return *result.Result, err
	}

	if result, err := r.reconcileCNI(ctx, cluster, capiCluster, specWithBundles); err != nil {
		return *result.Result, err
	}

	return reconcileResult.ToCtrlResult(), nil
}

func (r *ClusterReconciler) reconcileDelete(ctx context.Context, cluster *anywherev1.Cluster) (ctrl.Result, error) {
	capiCluster := &clusterv1.Cluster{}
	capiClusterName := types.NamespacedName{Namespace: constants.EksaSystemNamespace, Name: cluster.Name}
	r.log.Info("Deleting", "name", cluster.Name)
	err := r.client.Get(ctx, capiClusterName, capiCluster)

	switch {
	case err == nil:
		r.log.Info("Deleting CAPI cluster", "name", capiCluster.Name)
		if err := r.client.Delete(ctx, capiCluster); err != nil {
			r.log.Info("Error deleting CAPI cluster", "name", capiCluster.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
	case apierrors.IsNotFound(err):
		r.log.Info("Deleting EKS Anywhere cluster", "name", capiCluster.Name, "cluster.DeletionTimestamp", cluster.DeletionTimestamp, "finalizer", cluster.Finalizers)

		// TODO delete GitOps,Datacenter and MachineConfig objects
		controllerutil.RemoveFinalizer(cluster, clusterFinalizerName)
	default:
		return ctrl.Result{}, err

	}
	return ctrl.Result{}, nil
}

func (r *ClusterReconciler) bundles(ctx context.Context, name, namespace string) (*releasev1alpha1.Bundles, error) {
	clusterBundle := &releasev1alpha1.Bundles{}
	bundleName := types.NamespacedName{Namespace: namespace, Name: name}

	if err := r.client.Get(ctx, bundleName, clusterBundle); err != nil {
		return nil, err
	}

	return clusterBundle, nil
}

func (r *ClusterReconciler) eksdRelease(ctx context.Context, name, namespace string) (*eksdv1alpha1.Release, error) {
	eksd := &eksdv1alpha1.Release{}
	releaseName := types.NamespacedName{Namespace: namespace, Name: name}

	if err := r.client.Get(ctx, releaseName, eksd); err != nil {
		return nil, err
	}

	return eksd, nil
}

func (r *ClusterReconciler) reconcileExtraObjects(ctx context.Context, cluster *anywherev1.Cluster, capiCluster *clusterv1.Cluster, specWithBundles *c.Spec) (reconciler.Result, error) {
	if !conditions.IsTrue(capiCluster, extraObjectsSpecPlaneAppliedCondition) {
		extraObjects := c.BuildExtraObjects(specWithBundles)

		for _, spec := range extraObjects.Values() {
			if err := reconciler.ReconcileYaml(ctx, r.client, spec); err != nil {
				return reconciler.Result{}, err
			}
		}
		conditions.MarkTrue(cluster, extraObjectsSpecPlaneAppliedCondition)
	}
	return reconciler.Result{}, nil
}

func (r *ClusterReconciler) reconcileCNI(ctx context.Context, cluster *anywherev1.Cluster, capiCluster *clusterv1.Cluster, specWithBundles *c.Spec) (reconciler.Result, error) {
	if !conditions.Has(cluster, cniSpecAppliedCondition) || conditions.IsFalse(capiCluster, cniSpecAppliedCondition) {
		r.log.Info("Getting remote client", "client for cluster", capiCluster.Name)
		key := client.ObjectKey{
			Namespace: capiCluster.Namespace,
			Name:      capiCluster.Name,
		}
		remoteClient, err := r.tracker.GetClient(ctx, key)
		if err != nil {
			return reconciler.Result{}, err
		}

		r.log.Info("About to apply CNI")

		helm := executables.NewHelm(executables.NewExecutable("helm"))
		cilium := cilium.NewCilium(nil, helm)

		if err != nil {
			return reconciler.Result{}, err
		}
		ciliumSpec, err := cilium.GenerateManifest(ctx, specWithBundles, []string{constants.CapvSystemNamespace})
		if err != nil {
			return reconciler.Result{}, err
		}
		if err := reconciler.ReconcileYaml(ctx, remoteClient, ciliumSpec); err != nil {
			return reconciler.Result{}, err
		}
		conditions.MarkTrue(cluster, cniSpecAppliedCondition)
	}
	return reconciler.Result{}, nil
}
