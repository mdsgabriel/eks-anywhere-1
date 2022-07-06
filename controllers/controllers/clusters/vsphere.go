package clusters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/eks-anywhere/controllers/controllers/reconciler"
	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	eksacluster "github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/constants"
	"github.com/aws/eks-anywhere/pkg/executables"
	"github.com/aws/eks-anywhere/pkg/networking/cilium"
	"github.com/aws/eks-anywhere/pkg/providers"
	"github.com/aws/eks-anywhere/pkg/providers/common"
	"github.com/aws/eks-anywhere/pkg/providers/vsphere"
	releasev1alpha1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

const defaultRequeueTime = time.Minute

// TODO move these constants
const (
	managedEtcdReadyCondition             clusterv1.ConditionType = "ManagedEtcdReady"
	controlSpecPlaneAppliedCondition      clusterv1.ConditionType = "ControlPlaneSpecApplied"
	workerNodeSpecPlaneAppliedCondition   clusterv1.ConditionType = "WorkerNodeSpecApplied"
	extraObjectsSpecPlaneAppliedCondition clusterv1.ConditionType = "ExtraObjectsSpecApplied"
	cniSpecAppliedCondition               clusterv1.ConditionType = "CNISpecApplied"
	controlPlaneReadyCondition            clusterv1.ConditionType = "ControlPlaneReady"
)

// Struct that holds common methods and properties
type VSphereReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Validator *vsphere.Validator
	Defaulter *vsphere.Defaulter
	tracker   *remote.ClusterCacheTracker
}

type VSphereClusterReconciler struct {
	VSphereReconciler
	*providerClusterReconciler
}

func NewVSphereReconciler(client client.Client, log logr.Logger, validator *vsphere.Validator, defaulter *vsphere.Defaulter, tracker *remote.ClusterCacheTracker) *VSphereClusterReconciler {
	return &VSphereClusterReconciler{
		VSphereReconciler: VSphereReconciler{
			Client:    client,
			Log:       log,
			Validator: validator,
			Defaulter: defaulter,
			tracker:   tracker,
		},
		providerClusterReconciler: &providerClusterReconciler{
			providerClient: client,
		},
	}
}

func VsphereCredentials(ctx context.Context, cli client.Client) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: "eksa-system",
		Name:      vsphere.CredentialsObjectName,
	}
	if err := cli.Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func SetupEnvVars(ctx context.Context, vsphereDatacenter *anywherev1.VSphereDatacenterConfig, cli client.Client) error {
	secret, err := VsphereCredentials(ctx, cli)
	if err != nil {
		return fmt.Errorf("failed getting vsphere credentials secret: %v", err)
	}

	vsphereUsername := secret.Data["username"]
	vspherePassword := secret.Data["password"]

	if err := os.Setenv(vsphere.EksavSphereUsernameKey, string(vsphereUsername)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", vsphere.EksavSphereUsernameKey, err)
	}

	if err := os.Setenv(vsphere.EksavSpherePasswordKey, string(vspherePassword)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", vsphere.EksavSpherePasswordKey, err)
	}

	if err := vsphere.SetupEnvVars(vsphereDatacenter); err != nil {
		return fmt.Errorf("failed setting env vars: %v", err)
	}

	return nil
}

func (v *VSphereClusterReconciler) bundles(ctx context.Context, name, namespace string) (*releasev1alpha1.Bundles, error) {
	clusterBundle := &releasev1alpha1.Bundles{}
	bundleName := types.NamespacedName{Namespace: namespace, Name: name}

	if err := v.Client.Get(ctx, bundleName, clusterBundle); err != nil {
		return nil, err
	}

	return clusterBundle, nil
}

func (v *VSphereClusterReconciler) FetchAppliedSpec(ctx context.Context, cs *anywherev1.Cluster) (*eksacluster.Spec, error) {
	return eksacluster.BuildSpecForCluster(ctx, cs, v.bundles, v.eksdRelease, nil, nil, nil)
}

func (v *VSphereClusterReconciler) Reconcile(ctx context.Context, cluster *anywherev1.Cluster) (reconciler.Result, error) {
	v.Log.Info("Inside vsphere reconciler")

	dataCenterConfig := &anywherev1.VSphereDatacenterConfig{}
	dataCenterName := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.DatacenterRef.Name}
	if err := v.Client.Get(ctx, dataCenterName, dataCenterConfig); err != nil {
		return reconciler.Result{}, err
	}
	// Set up envs for executing Govc cmd and default values for datacenter config
	if err := SetupEnvVars(ctx, dataCenterConfig, v.Client); err != nil {
		v.Log.Error(err, "Failed to set up env vars and default values for VsphereDatacenterConfig")
		return reconciler.Result{}, err
	}
	if !dataCenterConfig.Status.SpecValid {
		v.Log.Info("Skipping cluster reconciliation because data center config is invalid", "data center", dataCenterConfig.Name)
		return reconciler.Result{
			Result: &ctrl.Result{
				Requeue:      true,
				RequeueAfter: defaultRequeueTime,
			},
		}, nil
	}

	machineConfigMap := map[string]*anywherev1.VSphereMachineConfig{}

	for _, ref := range cluster.MachineConfigRefs() {
		machineConfig := &anywherev1.VSphereMachineConfig{}
		machineConfigName := types.NamespacedName{Namespace: cluster.Namespace, Name: ref.Name}
		if err := v.Client.Get(ctx, machineConfigName, machineConfig); err != nil {
			return reconciler.Result{}, err
		}
		machineConfigMap[ref.Name] = machineConfig
	}

	v.Log.V(4).Info("Fetching bundle", "cluster name", cluster.Spec.ManagementCluster.Name)
	bundles, err := v.bundles(ctx, "bundles-1", "eksa-system")
	if err != nil {
		return reconciler.Result{}, err
	}
	versionsBundle, err := eksacluster.GetVersionsBundle(cluster, bundles)
	if err != nil {
		return reconciler.Result{}, err
	}
	v.Log.V(4).Info("Fetching eks-d manifest", "release name", versionsBundle.EksD.Name)
	eksd, err := v.eksdRelease(ctx, versionsBundle.EksD.Name, constants.EksaSystemNamespace)
	if err != nil {
		return reconciler.Result{}, err
	}

	specWithBundles, err := eksacluster.BuildSpecFromBundles(cluster, bundles, eksacluster.WithEksdRelease(eksd))

	vsphereClusterSpec := vsphere.NewSpec(specWithBundles, machineConfigMap, dataCenterConfig)

	if err := v.Validator.ValidateClusterMachineConfigs(ctx, vsphereClusterSpec); err != nil {
		return reconciler.Result{}, err
	}

	workerNodeGroupMachineSpecs := make(map[string]anywherev1.VSphereMachineConfigSpec, len(cluster.Spec.WorkerNodeGroupConfigurations))
	for _, wnConfig := range cluster.Spec.WorkerNodeGroupConfigurations {
		workerNodeGroupMachineSpecs[wnConfig.MachineGroupRef.Name] = machineConfigMap[wnConfig.MachineGroupRef.Name].Spec
	}

	cp := machineConfigMap[specWithBundles.Cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name]
	var etcdSpec *anywherev1.VSphereMachineConfigSpec
	if specWithBundles.Cluster.Spec.ExternalEtcdConfiguration != nil {
		etcd := machineConfigMap[specWithBundles.Cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name]
		etcdSpec = &etcd.Spec
	}

	templateBuilder := vsphere.NewVsphereTemplateBuilder(&dataCenterConfig.Spec, &cp.Spec, etcdSpec, workerNodeGroupMachineSpecs, time.Now, true)
	clusterName := cluster.ObjectMeta.Name

	kubeadmconfigTemplateNames := make(map[string]string, len(cluster.Spec.WorkerNodeGroupConfigurations))
	workloadTemplateNames := make(map[string]string, len(cluster.Spec.WorkerNodeGroupConfigurations))

	for _, wnConfig := range cluster.Spec.WorkerNodeGroupConfigurations {
		kubeadmconfigTemplateNames[wnConfig.Name] = common.KubeadmConfigTemplateName(cluster.Name, wnConfig.MachineGroupRef.Name, time.Now)
		workloadTemplateNames[wnConfig.Name] = common.WorkerMachineTemplateName(cluster.Name, wnConfig.Name, time.Now)
		templateBuilder.WorkerNodeGroupMachineSpecs[wnConfig.MachineGroupRef.Name] = workerNodeGroupMachineSpecs[wnConfig.MachineGroupRef.Name]
	}

	cpOpt := func(values map[string]interface{}) {
		values["controlPlaneTemplateName"] = common.CPMachineTemplateName(clusterName, time.Now)
		controlPlaneUser := machineConfigMap[cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name].Spec.Users[0]
		values["vsphereControlPlaneSshAuthorizedKey"] = controlPlaneUser.SshAuthorizedKeys[0]

		if cluster.Spec.ExternalEtcdConfiguration != nil {
			etcdUser := machineConfigMap[cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name].Spec.Users[0]
			values["vsphereEtcdSshAuthorizedKey"] = etcdUser.SshAuthorizedKeys[0]
		}

		values["etcdTemplateName"] = common.EtcdMachineTemplateName(clusterName, time.Now)
	}
	v.Log.Info("cluster", "name", cluster.Name)

	if result, err := v.reconcileControlPlaneSpec(ctx, cluster, templateBuilder, specWithBundles, cpOpt); err != nil {
		return result, err
	}

	if result, err := v.reconcileWorkerNodeSpec(ctx, cluster, templateBuilder, specWithBundles, workloadTemplateNames, kubeadmconfigTemplateNames); err != nil {
		return result, err
	}

	capiCluster, result, errCAPICLuster := v.getCAPICluster(ctx, cluster)
	if errCAPICLuster != nil {
		return result, errCAPICLuster
	}

	// wait for etcd if necessary
	if cluster.Spec.ExternalEtcdConfiguration != nil {
		if !conditions.Has(capiCluster, managedEtcdReadyCondition) || conditions.IsFalse(capiCluster, managedEtcdReadyCondition) {
			v.Log.Info("Waiting for etcd to be ready", "cluster", cluster.Name)
			return reconciler.Result{Result: &ctrl.Result{
				RequeueAfter: defaultRequeueTime,
			}}, nil
		}
	}

	if !conditions.IsTrue(capiCluster, controlPlaneReadyCondition) {
		v.Log.Info("waiting for control plane to be ready", "cluster", capiCluster.Name, "kind", capiCluster.Kind)
		return reconciler.Result{Result: &ctrl.Result{
			RequeueAfter: defaultRequeueTime,
		}}, err
	}

	if result, err := v.reconcileExtraObjects(ctx, cluster, capiCluster, specWithBundles); err != nil {
		return result, err
	}

	if result, err := v.reconcileCNI(ctx, cluster, capiCluster, specWithBundles); err != nil {
		return result, err
	}

	return reconciler.Result{}, nil
}

func (v *VSphereClusterReconciler) reconcileCNI(ctx context.Context, cluster *anywherev1.Cluster, capiCluster *clusterv1.Cluster, specWithBundles *eksacluster.Spec) (reconciler.Result, error) {
	if !conditions.Has(cluster, cniSpecAppliedCondition) || conditions.IsFalse(capiCluster, cniSpecAppliedCondition) {
		v.Log.Info("Getting remote client", "client for cluster", capiCluster.Name)
		key := client.ObjectKey{
			Namespace: capiCluster.Namespace,
			Name:      capiCluster.Name,
		}
		remoteClient, err := v.tracker.GetClient(ctx, key)
		if err != nil {
			return reconciler.Result{}, err
		}

		//install cilium if needed
		// wait for preflight
		ciliumDS := &v1.DaemonSet{}
		ciliumDSName := types.NamespacedName{Namespace: "kube-system", Name: "cilium"}
		err = remoteClient.Get(ctx, ciliumDSName, ciliumDS)
		if err != nil {
			if apierrors.IsNotFound(err) {
				v.Log.Info("Cilium DS not found, installing it")
				helm := executables.NewHelm(executables.NewExecutable("helm"), executables.WithInsecure())

				ci := cilium.NewCilium(nil, helm)

				ciliumSpec, err := ci.GenerateManifest(ctx, specWithBundles, []string{constants.CapvSystemNamespace})
				if err != nil {
					return reconciler.Result{}, err
				}
				if err := reconciler.ReconcileYaml(ctx, remoteClient, ciliumSpec); err != nil {
					return reconciler.Result{}, err
				}
				return reconciler.Result{}, err
			}

			return reconciler.Result{}, err
		}

		//upgrade cilium
		v.Log.Info("Upgrading Cilium!!!")
		needsUpgrade, err := ciliumNeedsUpgrade(ctx, v.Log, remoteClient, specWithBundles)
		if err != nil {
			return reconciler.Result{}, err
		}

		if !needsUpgrade {
			v.Log.Info("Cilium already updated")
			return reconciler.Result{}, nil
		}

		v.Log.Info("About to apply CNI")

		helm := executables.NewHelm(executables.NewExecutable("helm"))
		templater := cilium.NewTemplater(helm)
		preflight, err := templater.GenerateUpgradePreflightManifest(ctx, specWithBundles)
		if err != nil {
			return reconciler.Result{}, err
		}

		v.Log.Info("Installing Cilium upgrade preflight manifest")
		if err := reconciler.ReconcileYaml(ctx, remoteClient, preflight); err != nil {
			return reconciler.Result{}, err
		}
		//v.Log.Info("preflight manifest installed", "spec", string(preflight))

		// wait for preflight
		ciliumDS = &v1.DaemonSet{}
		ciliumDSName = types.NamespacedName{Namespace: "kube-system", Name: "cilium"}
		if err := remoteClient.Get(ctx, ciliumDSName, ciliumDS); err != nil {
			v.Log.V(2).Info("Cilium DS not found")
			return reconciler.Result{}, err
		}

		if err := checkDaemonSetReady(ciliumDS); err != nil {
			v.Log.V(2).Info("Cilium DS not ready")
			return reconciler.Result{Result: &ctrl.Result{
				Requeue:      true,
				RequeueAfter: defaultRequeueTime,
			}}, nil
		}

		preFlightCiliumDS := &v1.DaemonSet{}
		preFlightCiliumDSName := types.NamespacedName{Namespace: "kube-system", Name: "cilium-pre-flight-check"}
		if err := v.Client.Get(ctx, preFlightCiliumDSName, preFlightCiliumDS); err != nil {
			v.Log.Info("Preflight Cilium DS not found")
			return reconciler.Result{}, err
		}

		if err := checkPreflightDaemonSetReady(ciliumDS, preFlightCiliumDS); err != nil {
			v.Log.V(2).Info("Preflight DS not ready ")
			return reconciler.Result{Result: &ctrl.Result{
				Requeue:      true,
				RequeueAfter: defaultRequeueTime,
			}}, nil
		}

		v.Log.V(2).Info("Deleting Preflight Cilium objects")
		if err := reconciler.DeleteYaml(ctx, remoteClient, preflight); err != nil {
			v.Log.V(2).Info("Error deleting Preflight Cilium objects")
			return reconciler.Result{}, err
		}

		v.Log.V(3).Info("Generating Cilium upgrade manifest")
		upgradeManifest, err := templater.GenerateUpgradeManifest(ctx, specWithBundles, specWithBundles)
		if err != nil {
			return reconciler.Result{}, err
		}

		if err := reconciler.ReconcileYaml(ctx, remoteClient, upgradeManifest); err != nil {
			return reconciler.Result{}, err
		}

		/*cilium := cilium.NewCilium(nil, helm)

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
		conditions.MarkTrue(cluster, cniSpecAppliedCondition)*/
	}
	return reconciler.Result{}, nil
}

// TODO dedup
func checkPreflightDaemonSetReady(ciliumDaemonSet, preflightDaemonSet *v1.DaemonSet) error {
	if err := checkDaemonSetObservedGeneration(ciliumDaemonSet); err != nil {
		return err
	}
	if err := checkDaemonSetObservedGeneration(preflightDaemonSet); err != nil {
		return err
	}

	if ciliumDaemonSet.Status.NumberReady != preflightDaemonSet.Status.NumberReady {
		return fmt.Errorf("cilium preflight check DS is not ready: %d want and %d ready", ciliumDaemonSet.Status.NumberReady, preflightDaemonSet.Status.NumberReady)
	}
	return nil
}

// TODO dedup
func checkDaemonSetReady(daemonSet *v1.DaemonSet) error {
	if err := checkDaemonSetObservedGeneration(daemonSet); err != nil {
		return err
	}

	if daemonSet.Status.DesiredNumberScheduled != daemonSet.Status.NumberReady {
		return fmt.Errorf("daemonSet %s is not ready: %d/%d ready", daemonSet.Name, daemonSet.Status.NumberReady, daemonSet.Status.DesiredNumberScheduled)
	}
	return nil
}

// TODO dedup
func checkDaemonSetObservedGeneration(daemonSet *v1.DaemonSet) error {
	observedGeneration := daemonSet.Status.ObservedGeneration
	generation := daemonSet.Generation
	if observedGeneration != generation {
		return fmt.Errorf("daemonSet %s status needs to be refreshed: observed generation is %d, want %d", daemonSet.Name, observedGeneration, generation)
	}

	return nil
}

func (v *VSphereClusterReconciler) reconcileExtraObjects(ctx context.Context, cluster *anywherev1.Cluster, capiCluster *clusterv1.Cluster, specWithBundles *eksacluster.Spec) (reconciler.Result, error) {
	if !conditions.IsTrue(capiCluster, extraObjectsSpecPlaneAppliedCondition) {
		extraObjects := eksacluster.BuildExtraObjects(specWithBundles)

		for _, spec := range extraObjects.Values() {
			if err := reconciler.ReconcileYaml(ctx, v.Client, spec); err != nil {
				return reconciler.Result{}, err
			}
		}
		conditions.MarkTrue(cluster, extraObjectsSpecPlaneAppliedCondition)
	}
	return reconciler.Result{}, nil
}

func (v *VSphereClusterReconciler) getCAPICluster(ctx context.Context, cluster *anywherev1.Cluster) (*clusterv1.Cluster, reconciler.Result, error) {
	capiCluster := &clusterv1.Cluster{}
	capiClusterName := types.NamespacedName{Namespace: constants.EksaSystemNamespace, Name: cluster.Name}
	v.Log.Info("Searching for CAPI cluster", "name", cluster.Name)
	if err := v.Client.Get(ctx, capiClusterName, capiCluster); err != nil {
		return nil, reconciler.Result{Result: &ctrl.Result{
			Requeue:      true,
			RequeueAfter: defaultRequeueTime,
		}}, err
	}
	return capiCluster, reconciler.Result{}, nil
}

func (v *VSphereClusterReconciler) reconcileWorkerNodeSpec(
	ctx context.Context, cluster *anywherev1.Cluster, templateBuilder providers.TemplateBuilder,
	specWithBundles *eksacluster.Spec, workloadTemplateNames, kubeadmconfigTemplateNames map[string]string,
) (reconciler.Result, error) {
	if !conditions.IsTrue(cluster, workerNodeSpecPlaneAppliedCondition) {
		workersSpec, err := templateBuilder.GenerateCAPISpecWorkers(specWithBundles, workloadTemplateNames, kubeadmconfigTemplateNames)
		if err != nil {
			return reconciler.Result{}, err
		}

		if err := reconciler.ReconcileYaml(ctx, v.Client, workersSpec); err != nil {
			return reconciler.Result{}, err
		}

		conditions.MarkTrue(cluster, workerNodeSpecPlaneAppliedCondition)
	}
	return reconciler.Result{}, nil
}

func (v *VSphereClusterReconciler) reconcileControlPlaneSpec(ctx context.Context, cluster *anywherev1.Cluster, templateBuilder providers.TemplateBuilder, specWithBundles *eksacluster.Spec, cpOpt func(values map[string]interface{})) (reconciler.Result, error) {
	if !conditions.IsTrue(cluster, controlSpecPlaneAppliedCondition) {
		v.Log.Info("Applying control plane spec", "name", cluster.Name)
		controlPlaneSpec, err := templateBuilder.GenerateCAPISpecControlPlane(specWithBundles, cpOpt)
		if err != nil {
			return reconciler.Result{}, err
		}
		if err := reconciler.ReconcileYaml(ctx, v.Client, controlPlaneSpec); err != nil {
			return reconciler.Result{Result: &ctrl.Result{
				RequeueAfter: defaultRequeueTime,
			}}, err
		}
		conditions.MarkTrue(cluster, controlSpecPlaneAppliedCondition)
	}
	return reconciler.Result{}, nil
}

func ciliumNeedsUpgrade(ctx context.Context, log logr.Logger, client client.Client, clusterSpec *eksacluster.Spec) (bool, error) {
	log.Info("Checking if Cilium DS needs upgrade")
	needsUpgrade, err := ciliumDSNeedsUpgrade(ctx, log, client, clusterSpec)
	if err != nil {
		return false, err
	}

	if needsUpgrade {
		log.Info("Cilium DS needs upgrade")
		return true, nil
	}

	log.Info("Checking if Cilium operator deployment needs upgrade")
	needsUpgrade, err = ciliumOperatorNeedsUpgrade(ctx, log, client, clusterSpec)
	if err != nil {
		return false, err
	}

	if needsUpgrade {
		log.Info("Cilium operator deployment needs upgrade")
		return true, nil
	}

	return false, nil
}

func ciliumDSNeedsUpgrade(ctx context.Context, log logr.Logger, client client.Client, clusterSpec *eksacluster.Spec) (bool, error) {
	ds, err := getCiliumDS(ctx, client)
	if err != nil {
		return false, err
	}

	if ds == nil {
		log.Info("Cilium DS doesn't exist")
		return true, nil
	}

	dsImage := clusterSpec.VersionsBundle.Cilium.Cilium.VersionedImage()
	containers := make([]corev1.Container, 0, len(ds.Spec.Template.Spec.Containers)+len(ds.Spec.Template.Spec.InitContainers))
	for _, c := range containers {
		if c.Image != dsImage {
			log.Info("Cilium DS container needs upgrade", "container", c.Name)
			return true, nil
		}
	}

	return false, nil
}

func ciliumOperatorNeedsUpgrade(ctx context.Context, log logr.Logger, client client.Client, clusterSpec *eksacluster.Spec) (bool, error) {
	operator, err := getCiliumDeployment(ctx, client)
	if err != nil {
		return false, err
	}

	if operator == nil {
		log.Info("Cilium operator deployment doesn't exist")
		return true, nil
	}

	operatorImage := clusterSpec.VersionsBundle.Cilium.Operator.VersionedImage()
	if len(operator.Spec.Template.Spec.Containers) == 0 {
		return false, errors.New("cilium-operator deployment doesn't have any containers")
	}

	if operator.Spec.Template.Spec.Containers[0].Image != operatorImage {
		return true, nil
	}

	return false, nil
}

func getCiliumDS(ctx context.Context, client client.Client) (*v1.DaemonSet, error) {
	ds := &v1.DaemonSet{}
	err := client.Get(ctx, types.NamespacedName{Name: "cilium", Namespace: "kube-system"}, ds)
	if err != nil {
		return nil, err
	}

	return ds, nil
}

func getCiliumDeployment(ctx context.Context, client client.Client) (*v1.Deployment, error) {
	deployment := &v1.Deployment{}
	err := client.Get(ctx, types.NamespacedName{Name: "cilium-operator", Namespace: "kube-system"}, deployment)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return deployment, nil
}
