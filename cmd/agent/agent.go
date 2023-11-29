// Copyright (c) 2020 Red Hat, Inc.

package main

import (
	"context"
	"os"
	"time"

	"github.com/stolostron/multicloud-operators-foundation/cmd/agent/app"
	"github.com/stolostron/multicloud-operators-foundation/cmd/agent/app/options"
	"github.com/stolostron/multicloud-operators-foundation/pkg/utils"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"

	openshiftclientset "github.com/openshift/client-go/config/clientset/versioned"
	openshiftoauthclientset "github.com/openshift/client-go/oauth/clientset/versioned"
	routev1 "github.com/openshift/client-go/route/clientset/versioned"
	"open-cluster-management.io/addon-framework/pkg/lease"
	clusterclientset "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterinformers "open-cluster-management.io/api/client/cluster/informers/externalversions"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // Needed for misc auth.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	AddonName               = "work-manager"
	leaseUpdateJitterFactor = 0.25
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	o := options.NewAgentOptions()
	o.AddFlags(pflag.CommandLine)
	klog.InitFlags(nil)
	flag.InitFlags()

	logs.InitLogs()
	defer logs.FlushLogs()

	ctx := signals.SetupSignalHandler()
	startManager(o, ctx)
}

func startManager(o *options.AgentOptions, ctx context.Context) {
	hubConfig, err := clientcmd.BuildConfigFromFlags("", o.HubKubeConfig)
	if err != nil {
		setupLog.Error(err, "Unable to get hub kube config.")
		os.Exit(1)
	}

	// create management kube config
	managementKubeConfig, err := clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	if err != nil {
		setupLog.Error(err, "Unable to get management cluster kube config.")
		os.Exit(1)
	}
	managementKubeConfig.QPS = o.QPS
	managementKubeConfig.Burst = o.Burst

	managementClusterKubeClient, err := kubernetes.NewForConfig(managementKubeConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create management cluster kube client.")
		os.Exit(1)
	}

	// load managed client config, the work manager agent may not running in the managed cluster.
	managedClusterConfig := managementKubeConfig
	if o.ManagedKubeConfig != "" {
		managedClusterConfig, err = clientcmd.BuildConfigFromFlags("", o.ManagedKubeConfig)
		if err != nil {
			setupLog.Error(err, "Unable to get managed cluster kube config.")
			os.Exit(1)
		}
		managedClusterConfig.QPS = o.QPS
		managedClusterConfig.Burst = o.Burst
	}
	managedClusterDynamicClient, err := dynamic.NewForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create managed cluster dynamic client.")
		os.Exit(1)
	}
	managedClusterKubeClient, err := kubernetes.NewForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create managed cluster kube client.")
		os.Exit(1)
	}
	routeV1Client, err := routev1.NewForConfig(managementKubeConfig)
	if err != nil {
		setupLog.Error(err, "New route client config error:")
	}

	openshiftClient, err := openshiftclientset.NewForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create managed cluster openshift config clientset.")
		os.Exit(1)
	}

	osOauthClient, err := openshiftoauthclientset.NewForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create managed cluster openshift oauth clientset.")
		os.Exit(1)
	}

	managedClusterClusterClient, err := clusterclientset.NewForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Unable to create managed cluster cluster clientset.")
		os.Exit(1)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(managedClusterConfig)
	if err != nil {
		setupLog.Error(err, "Failed to create discovery client")
		os.Exit(1)
	}

	cc, err := addonutils.NewConfigChecker("work manager", o.HubKubeConfig)
	if err != nil {
		setupLog.Error(err, "unable to setup a configChecker")
		os.Exit(1)
	}

	// run healthProbes server before newManager, because it may take a long time to discover the APIs of Hub cluster in newManager.
	// the agent pods will be restarted if failed to check healthz/liveness in 30s.
	go app.ServeHealthProbes(ctx.Done(), ":8000", cc.Check)

	run := func(ctx context.Context) {

		kubeInformerFactory := informers.NewSharedInformerFactory(managedClusterKubeClient, 10*time.Minute)
		clusterInformerFactory := clusterinformers.NewSharedInformerFactory(managedClusterClusterClient, 10*time.Minute)

		KlusterletFeature, err := app.NewKlusterletFeature(ctx, scheme, managedClusterConfig, managedClusterDynamicClient, managedClusterKubeClient,
			managementClusterKubeClient, routeV1Client, openshiftClient, osOauthClient, discoveryClient,
			managedClusterClusterClient, kubeInformerFactory, clusterInformerFactory, o)
		if err != nil {
			setupLog.Error(err, "unable to create klusterlet plugin")
			os.Exit(1)
		}

		mgr, err := ctrl.NewManager(hubConfig, ctrl.Options{
			Scheme:             KlusterletFeature.GetOptions().Scheme,
			MetricsBindAddress: o.MetricsAddr,
			Namespace:          o.ClusterName,
		})
		if err != nil {
			setupLog.Error(err, "unable to start manager")
			os.Exit(1)
		}

		KlusterletFeature.Complete(ctx, mgr)

		componentNamespace := o.ComponentNamespace
		if len(componentNamespace) == 0 {
			componentNamespace, err = utils.GetComponentNamespace()
			if err != nil {
				setupLog.Error(err, "Failed to get pod namespace")
				os.Exit(1)
			}
		}
		leaseUpdater := lease.NewLeaseUpdater(managementClusterKubeClient, AddonName,
			componentNamespace, lease.CheckManagedClusterHealthFunc(managedClusterKubeClient.Discovery())).
			WithHubLeaseConfig(hubConfig, o.ClusterName)
		go leaseUpdater.Start(ctx)

		go kubeInformerFactory.Start(ctx.Done())
		go clusterInformerFactory.Start(ctx.Done())

		setupLog.Info("starting manager")
		if err := mgr.Start(ctx); err != nil {
			setupLog.Error(err, "problem running manager")
			os.Exit(1)
		}
	}
	run(context.TODO())
	panic("unreachable")
}
