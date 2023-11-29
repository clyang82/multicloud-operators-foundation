package app

import (
	"context"
	"fmt"

	openshiftclientset "github.com/openshift/client-go/config/clientset/versioned"
	openshiftoauthclientset "github.com/openshift/client-go/oauth/clientset/versioned"
	routev1 "github.com/openshift/client-go/route/clientset/versioned"
	"github.com/spf13/pflag"
	actionv1beta1 "github.com/stolostron/cluster-lifecycle-api/action/v1beta1"
	clusterv1beta1 "github.com/stolostron/cluster-lifecycle-api/clusterinfo/v1beta1"
	viewv1beta1 "github.com/stolostron/cluster-lifecycle-api/view/v1beta1"
	"github.com/stolostron/multicloud-operators-foundation/cmd/agent/app/options"
	actionctrl "github.com/stolostron/multicloud-operators-foundation/pkg/klusterlet/action"
	clusterclaimctl "github.com/stolostron/multicloud-operators-foundation/pkg/klusterlet/clusterclaim"
	clusterinfoctl "github.com/stolostron/multicloud-operators-foundation/pkg/klusterlet/clusterinfo"
	"github.com/stolostron/multicloud-operators-foundation/pkg/klusterlet/nodecollector"
	viewctrl "github.com/stolostron/multicloud-operators-foundation/pkg/klusterlet/view"
	"github.com/stolostron/multicloud-operators-foundation/pkg/utils"
	restutils "github.com/stolostron/multicloud-operators-foundation/pkg/utils/rest"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	clusterclientset "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterinformers "open-cluster-management.io/api/client/cluster/informers/externalversions"
	operatorv1 "open-cluster-management.io/api/client/operator/clientset/versioned/typed/operator/v1"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func NewKlusterletFeature(ctx context.Context,
	scheme *runtime.Scheme,
	managedClusterConfig *rest.Config,
	managedClusterDynamicClient dynamic.Interface,
	managedClusterKubeClient *kubernetes.Clientset,
	managementClusterKubeClient *kubernetes.Clientset,
	routeV1Client *routev1.Clientset,
	openshiftClient *openshiftclientset.Clientset,
	osOauthClient *openshiftoauthclientset.Clientset,
	discoveryClient *discovery.DiscoveryClient,
	managedClusterClusterClient *clusterclientset.Clientset,
	kubeInformerFactory informers.SharedInformerFactory,
	clusterInformerFactory clusterinformers.SharedInformerFactory,
	o *options.AgentOptions, //TODO: remove this parameter
) (*operatorv1.KlusterletFeature, error) {

	_ = clientgoscheme.AddToScheme(scheme)

	_ = actionv1beta1.AddToScheme(scheme)
	_ = viewv1beta1.AddToScheme(scheme)
	_ = clusterv1beta1.AddToScheme(scheme)
	_ = clusterv1alpha1.AddToScheme(scheme)

	reloadMapper := restutils.NewReloadMapper(restmapper.NewDeferredDiscoveryRESTMapper(
		cacheddiscovery.NewMemCacheClient(discoveryClient)))

	// Add controller into manager
	actionReconciler := &actionctrl.ActionReconciler{
		Log:                 ctrl.Log.WithName("controllers").WithName("ManagedClusterAction"),
		Scheme:              scheme,
		KubeControl:         restutils.NewKubeControl(reloadMapper, managedClusterConfig),
		EnableImpersonation: o.EnableImpersonation,
	}

	viewReconciler := &viewctrl.ViewReconciler{
		Log:                         ctrl.Log.WithName("controllers").WithName("ManagedClusterView"),
		Scheme:                      scheme,
		ManagedClusterDynamicClient: managedClusterDynamicClient,
		Mapper:                      reloadMapper,
	}

	var err error
	componentNamespace := o.ComponentNamespace
	if len(componentNamespace) == 0 {
		componentNamespace, err = utils.GetComponentNamespace()
		if err != nil {
			return nil, fmt.Errorf("failed to get pod namespace %v", err)
		}
	}

	// run agent server
	agent, err := AgentServerRun(o, managedClusterKubeClient)
	if err != nil {
		return nil, fmt.Errorf("unable to run agent server %v", err)
	}

	clusterInfoReconciler := clusterinfoctl.ClusterInfoReconciler{
		Log:                      ctrl.Log.WithName("controllers").WithName("ManagedClusterInfo"),
		Scheme:                   scheme,
		NodeInformer:             kubeInformerFactory.Core().V1().Nodes(),
		ClaimInformer:            clusterInformerFactory.Cluster().V1alpha1().ClusterClaims(),
		ClaimLister:              clusterInformerFactory.Cluster().V1alpha1().ClusterClaims().Lister(),
		ManagedClusterClient:     managedClusterKubeClient,
		ManagementClusterClient:  managementClusterKubeClient,
		DisableLoggingInfoSyncer: o.DisableLoggingInfoSyncer,
		ClusterName:              o.ClusterName,
		AgentName:                o.AgentName,
		AgentNamespace:           componentNamespace,
		AgentPort:                int32(o.AgentPort),
		RouteV1Client:            routeV1Client,
		ConfigV1Client:           openshiftClient,
		Agent:                    agent,
	}

	clusterClaimer := clusterclaimctl.ClusterClaimer{
		KubeClient:     managedClusterKubeClient,
		ConfigV1Client: openshiftClient,
		OauthV1Client:  osOauthClient,
		Mapper:         reloadMapper,
	}

	clusterClaimReconciler, err := clusterclaimctl.NewClusterClaimReconciler(
		ctrl.Log.WithName("controllers").WithName("ClusterClaim"),
		o.ClusterName,
		managedClusterClusterClient,
		clusterInformerFactory.Cluster().V1alpha1().ClusterClaims(),
		clusterClaimer.GenerateExpectClusterClaims,
		o.EnableSyncLabelsToClusterClaims,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create ClusterClaim controller %v", err)
	}

	// Need to consider how to start
	resourceCollector := nodecollector.NewCollector(
		kubeInformerFactory.Core().V1().Nodes(),
		managedClusterKubeClient,
		o.ClusterName,
		componentNamespace,
		o.EnableNodeCapacity)

	return (&operatorv1.KlusterletFeature{
		Name: "workmgr",
	}).
		WithControllerManagerOptions(manager.Options{
			Scheme: scheme,
		}).
		WithReconciler(actionReconciler).
		WithReconciler(viewReconciler).
		WithReconciler(&clusterInfoReconciler).
		WithReconciler(clusterClaimReconciler).
		WithRoutine(resourceCollector).
		WithFlags(pflag.CommandLine), nil
}
