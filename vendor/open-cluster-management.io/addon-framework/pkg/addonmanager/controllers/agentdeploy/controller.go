package agentdeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	addoninformerv1alpha1 "open-cluster-management.io/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "open-cluster-management.io/api/client/addon/listers/addon/v1alpha1"
	clusterinformers "open-cluster-management.io/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlister "open-cluster-management.io/api/client/cluster/listers/cluster/v1"
	workv1client "open-cluster-management.io/api/client/work/clientset/versioned"
	workinformers "open-cluster-management.io/api/client/work/informers/externalversions/work/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/api/utils/work/v1/workapplier"
	"open-cluster-management.io/api/utils/work/v1/workbuilder"
	workapiv1 "open-cluster-management.io/api/work/v1"

	"open-cluster-management.io/addon-framework/pkg/addonmanager/constants"
	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/basecontroller/factory"
	"open-cluster-management.io/addon-framework/pkg/index"
)

// addonDeployController deploy addon agent resources on the managed cluster.
type addonDeployController struct {
	workApplier               *workapplier.WorkApplier
	workBuilder               *workbuilder.WorkBuilder
	addonClient               addonv1alpha1client.Interface
	managedClusterLister      clusterlister.ManagedClusterLister
	managedClusterAddonLister addonlisterv1alpha1.ManagedClusterAddOnLister
	workIndexer               cache.Indexer
	agentAddons               map[string]agent.AgentAddon
}

func NewAddonDeployController(
	workClient workv1client.Interface,
	addonClient addonv1alpha1client.Interface,
	clusterInformers clusterinformers.ManagedClusterInformer,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	workInformers workinformers.ManifestWorkInformer,
	agentAddons map[string]agent.AgentAddon,
) factory.Controller {
	c := &addonDeployController{
		workApplier: workapplier.NewWorkApplierWithTypedClient(workClient, workInformers.Lister()),
		// the default manifest limit in a work is 500k
		// TODO: make the limit configurable
		workBuilder:               workbuilder.NewWorkBuilder().WithManifestsLimit(500 * 1024),
		addonClient:               addonClient,
		managedClusterLister:      clusterInformers.Lister(),
		managedClusterAddonLister: addonInformers.Lister(),
		workIndexer:               workInformers.Informer().GetIndexer(),
		agentAddons:               agentAddons,
	}

	return factory.New().WithFilteredEventsInformersQueueKeysFunc(
		func(obj runtime.Object) []string {
			key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			return []string{key}
		},
		func(obj interface{}) bool {
			accessor, _ := meta.Accessor(obj)
			if _, ok := c.agentAddons[accessor.GetName()]; !ok {
				return false
			}

			return true
		},
		addonInformers.Informer()).
		WithFilteredEventsInformersQueueKeysFunc(
			func(obj runtime.Object) []string {
				accessor, _ := meta.Accessor(obj)
				// in hosted mode, need get the addon namespace from the AddonNamespaceLabel, because
				// the namespaces of manifestWork and addon may be different.
				// in default mode, the addon and manifestWork are in the cluster namespace.
				if addonNamespace, ok := accessor.GetLabels()[addonapiv1alpha1.AddonNamespaceLabelKey]; ok {
					return []string{fmt.Sprintf("%s/%s", addonNamespace, accessor.GetLabels()[addonapiv1alpha1.AddonLabelKey])}
				}
				return []string{fmt.Sprintf("%s/%s", accessor.GetNamespace(), accessor.GetLabels()[addonapiv1alpha1.AddonLabelKey])}
			},
			func(obj interface{}) bool {
				accessor, _ := meta.Accessor(obj)
				if accessor.GetLabels() == nil {
					return false
				}

				// only watch the addon deploy/hook manifestWorks here.
				addonName, ok := accessor.GetLabels()[addonapiv1alpha1.AddonLabelKey]
				if !ok {
					return false
				}

				if _, ok := c.agentAddons[addonName]; !ok {
					return false
				}

				if strings.HasPrefix(accessor.GetName(), constants.DeployWorkNamePrefix(addonName)) ||
					strings.HasPrefix(accessor.GetName(), constants.PreDeleteHookWorkName(addonName)) {
					return true
				}
				return false
			},
			workInformers.Informer(),
		).
		WithSync(c.sync).ToController("addon-deploy-controller")
}

type addonDeploySyncer interface {
	sync(ctx context.Context, syncCtx factory.SyncContext,
		cluster *clusterv1.ManagedCluster,
		addon *addonapiv1alpha1.ManagedClusterAddOn) (*addonapiv1alpha1.ManagedClusterAddOn, error)
}

func (c *addonDeployController) getWorksByAddonFn(index string) func(addonName, addonNamespace string) ([]*workapiv1.ManifestWork, error) {
	return func(addonName, addonNamespace string) ([]*workapiv1.ManifestWork, error) {
		items, err := c.workIndexer.ByIndex(index, fmt.Sprintf("%s/%s", addonNamespace, addonName))
		if err != nil {
			return nil, err
		}
		ret := make([]*workapiv1.ManifestWork, 0, len(items))
		for _, item := range items {
			ret = append(ret, item.(*workapiv1.ManifestWork))
		}

		return ret, nil
	}
}

func (c *addonDeployController) sync(ctx context.Context, syncCtx factory.SyncContext, key string) error {
	clusterName, addonName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// ignore addon whose key is not in format: namespace/name
		return nil
	}

	agentAddon, ok := c.agentAddons[addonName]
	if !ok {
		return nil
	}

	addon, err := c.managedClusterAddonLister.ManagedClusterAddOns(clusterName).Get(addonName)
	if errors.IsNotFound(err) {
		// need to find a way to clean up cache by addon
		return nil
	}
	if err != nil {
		return err
	}

	// to deploy agents if there is RegistrationApplied condition.
	if meta.FindStatusCondition(addon.Status.Conditions, addonapiv1alpha1.ManagedClusterAddOnRegistrationApplied) == nil {
		return nil
	}

	cluster, err := c.managedClusterLister.Get(clusterName)
	if errors.IsNotFound(err) {
		// the managedCluster is nil in this case,and sync cannot handle nil managedCluster.
		// TODO: consider to force delete the addon and its deploy manifestWorks.
		return nil
	}
	if err != nil {
		return err
	}

	syncers := []addonDeploySyncer{
		&defaultSyncer{
			buildWorks:     c.buildDeployManifestWorks,
			applyWork:      c.applyWork,
			getWorkByAddon: c.getWorksByAddonFn(index.ManifestWorkByAddon),
			deleteWork:     c.workApplier.Delete,
			agentAddon:     agentAddon,
		},
		&hostedSyncer{
			buildWorks:     c.buildDeployManifestWorks,
			applyWork:      c.applyWork,
			deleteWork:     c.workApplier.Delete,
			getCluster:     c.managedClusterLister.Get,
			getWorkByAddon: c.getWorksByAddonFn(index.ManifestWorkByHostedAddon),
			agentAddon:     agentAddon},
		&defaultHookSyncer{
			buildWorks: c.buildHookManifestWork,
			applyWork:  c.applyWork,
			agentAddon: agentAddon},
		&hostedHookSyncer{
			buildWorks:     c.buildHookManifestWork,
			applyWork:      c.applyWork,
			deleteWork:     c.workApplier.Delete,
			getCluster:     c.managedClusterLister.Get,
			getWorkByAddon: c.getWorksByAddonFn(index.ManifestWorkHookByHostedAddon),
			agentAddon:     agentAddon},
		&healthCheckSyncer{
			getWorkByAddon: c.getWorksByAddonFn(index.ManifestWorkByAddon),
			agentAddon:     agentAddon,
		},
	}

	oldAddon := addon
	addon = addon.DeepCopy()
	var errs []error
	for _, s := range syncers {
		var err error
		addon, err = s.sync(ctx, syncCtx, cluster, addon)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if err = c.updateAddon(ctx, addon, oldAddon); err != nil {
		return err
	}
	return errorsutil.NewAggregate(errs)
}

// updateAddon updates finalizers and conditions of addon.
// to avoid conflict updateAddon updates finalizers firstly if finalizers has change.
func (c *addonDeployController) updateAddon(ctx context.Context, new, old *addonapiv1alpha1.ManagedClusterAddOn) error {
	if !equality.Semantic.DeepEqual(new.GetFinalizers(), old.GetFinalizers()) {
		_, err := c.addonClient.AddonV1alpha1().ManagedClusterAddOns(new.Namespace).Update(ctx, new, metav1.UpdateOptions{})
		return err
	}

	if equality.Semantic.DeepEqual(new.Status.HealthCheck, old.Status.HealthCheck) &&
		equality.Semantic.DeepEqual(new.Status.Conditions, old.Status.Conditions) {
		return nil
	}

	oldData, err := json.Marshal(&addonapiv1alpha1.ManagedClusterAddOn{
		Status: addonapiv1alpha1.ManagedClusterAddOnStatus{
			HealthCheck: old.Status.HealthCheck,
			Conditions:  old.Status.Conditions,
		},
	})
	if err != nil {
		return err
	}

	newData, err := json.Marshal(&addonapiv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			UID:             new.UID,
			ResourceVersion: new.ResourceVersion,
		},
		Status: addonapiv1alpha1.ManagedClusterAddOnStatus{
			HealthCheck: new.Status.HealthCheck,
			Conditions:  new.Status.Conditions,
		},
	})
	if err != nil {
		return err
	}

	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return fmt.Errorf("failed to create patch for addon %s: %w", new.Name, err)
	}

	klog.V(2).Infof("Patching addon %s/%s condition with %s", new.Namespace, new.Name, string(patchBytes))
	_, err = c.addonClient.AddonV1alpha1().ManagedClusterAddOns(new.Namespace).Patch(
		ctx, new.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	return err
}

func (c *addonDeployController) applyWork(ctx context.Context, appliedType string,
	work *workapiv1.ManifestWork, addon *addonapiv1alpha1.ManagedClusterAddOn) (*workapiv1.ManifestWork, error) {

	work, err := c.workApplier.Apply(ctx, work)
	if err != nil {
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonWorkApplyFailed,
			Message: fmt.Sprintf("failed to apply manifestWork: %v", err),
		})
		return work, err
	}

	// Update addon status based on work's status
	WorkAppliedCond := meta.FindStatusCondition(work.Status.Conditions, workapiv1.WorkApplied)
	switch {
	case WorkAppliedCond == nil:
		return work, nil
	case WorkAppliedCond.Status == metav1.ConditionTrue:
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionTrue,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonManifestsApplied,
			Message: "manifests of addon are applied successfully",
		})
	default:
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonManifestsApplyFailed,
			Message: "failed to apply the manifests of addon",
		})
	}

	return work, nil
}

func (c *addonDeployController) buildDeployManifestWorks(installMode, workNamespace string,
	cluster *clusterv1.ManagedCluster, existingWorks []*workapiv1.ManifestWork,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (appliedWorks, deleteWorks []*workapiv1.ManifestWork, err error) {
	var appliedType string
	var addonWorkBuilder *addonWorksBuilder

	agentAddon := c.agentAddons[addon.Name]
	if agentAddon == nil {
		return nil, nil, fmt.Errorf("failed to get agentAddon")
	}

	switch installMode {
	case constants.InstallModeHosted:
		appliedType = addonapiv1alpha1.ManagedClusterAddOnHostingManifestApplied
		addonWorkBuilder = newHostingAddonWorksBuilder(agentAddon.GetAgentAddonOptions().HostedModeEnabled, c.workBuilder)
	case constants.InstallModeDefault:
		appliedType = addonapiv1alpha1.ManagedClusterAddOnManifestApplied
		addonWorkBuilder = newAddonWorksBuilder(agentAddon.GetAgentAddonOptions().HostedModeEnabled, c.workBuilder)
	default:
		return nil, nil, fmt.Errorf("invalid install mode %v", installMode)
	}

	objects, err := agentAddon.Manifests(cluster, addon)
	if err != nil {
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonWorkApplyFailed,
			Message: fmt.Sprintf("failed to get manifest from agent interface: %v", err),
		})
		return nil, nil, err
	}
	if len(objects) == 0 {
		return nil, nil, nil
	}

	manifestOptions := getManifestConfigOption(agentAddon, cluster, addon)
	existingWorksCopy := []workapiv1.ManifestWork{}
	for _, work := range existingWorks {
		existingWorksCopy = append(existingWorksCopy, *work)
	}
	appliedWorks, deleteWorks, err = addonWorkBuilder.BuildDeployWorks(workNamespace, addon, existingWorksCopy, objects, manifestOptions)
	if err != nil {
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonWorkApplyFailed,
			Message: fmt.Sprintf("failed to build manifestwork: %v", err),
		})
		return nil, nil, err
	}
	return appliedWorks, deleteWorks, nil
}
func (c *addonDeployController) buildHookManifestWork(installMode, workNamespace string,
	cluster *clusterv1.ManagedCluster, addon *addonapiv1alpha1.ManagedClusterAddOn) (*workapiv1.ManifestWork, error) {
	var appliedType string
	var addonWorkBuilder *addonWorksBuilder

	agentAddon := c.agentAddons[addon.Name]
	if agentAddon == nil {
		return nil, fmt.Errorf("failed to get agentAddon")
	}

	switch installMode {
	case constants.InstallModeHosted:
		appliedType = addonapiv1alpha1.ManagedClusterAddOnHostingManifestApplied
		addonWorkBuilder = newHostingAddonWorksBuilder(agentAddon.GetAgentAddonOptions().HostedModeEnabled, c.workBuilder)
	case constants.InstallModeDefault:
		appliedType = addonapiv1alpha1.ManagedClusterAddOnManifestApplied
		addonWorkBuilder = newAddonWorksBuilder(agentAddon.GetAgentAddonOptions().HostedModeEnabled, c.workBuilder)
	default:
		return nil, fmt.Errorf("invalid install mode %v", installMode)
	}

	objects, err := agentAddon.Manifests(cluster, addon)
	if err != nil {
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonWorkApplyFailed,
			Message: fmt.Sprintf("failed to get manifest from agent interface: %v", err),
		})
		return nil, err
	}
	if len(objects) == 0 {
		return nil, nil
	}

	hookWork, err := addonWorkBuilder.BuildHookWork(workNamespace, addon, objects)
	if err != nil {
		meta.SetStatusCondition(&addon.Status.Conditions, metav1.Condition{
			Type:    appliedType,
			Status:  metav1.ConditionFalse,
			Reason:  addonapiv1alpha1.AddonManifestAppliedReasonWorkApplyFailed,
			Message: fmt.Sprintf("failed to build manifestwork: %v", err),
		})
		return nil, err
	}
	return hookWork, nil
}
