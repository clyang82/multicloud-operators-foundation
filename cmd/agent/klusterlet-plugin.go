package main

import (
	"embed"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type KlusterletPluginController interface {
	// SetupWithManager sets up the controller with the Manager.
	SetupWithManager(mgr ctrl.Manager) error
	SetClient(c client.Client)
	//SetRecorder(r record.EventRecorder)
}

type KlusterletPlugin struct {

	// the name of KlusterletPlugin
	Name string

	reconcilers []reconcile.Reconciler

	options ctrl.Options

	// the crds and permissions should not be created in klusterlet
	// it should be in klusterlet operator or handle by the import controller
	permissions embed.FS
	crds        embed.FS

	// need carry over the flags
	// flags
}

// WithScheme is to register schemes required by this component.
// problem: how to inject scheme after manager is created?
// func (k *KlusterletPlugin) WithScheme(s *runtime.Scheme) *KlusterletPlugin {
// 	k.Scheme = s
// 	_ = scheme.AddToScheme(k.Scheme)
// 	return k
// }

// WithPermissions is to register permissions required by this component.
func (k *KlusterletPlugin) WithPermissions(fs embed.FS) *KlusterletPlugin {
	k.permissions = fs
	return k
}

func (k *KlusterletPlugin) WithCRDs(fs embed.FS) *KlusterletPlugin {
	k.crds = fs
	return k
}

// WithControllerManagerOptions sets the controller manager options.
// problem: the manager is create yet. how to set the options?
func (k *KlusterletPlugin) WithControllerManagerOptions(options manager.Options) *KlusterletPlugin {
	//TODO
	k.options = options
	return k
}

func (k *KlusterletPlugin) WithReconciler(r reconcile.Reconciler) *KlusterletPlugin {
	//TODO
	if k.reconcilers == nil {
		k.reconcilers = make([]reconcile.Reconciler, 0)
	}
	k.reconcilers = append(k.reconcilers, r)
	return k
}

func (k *KlusterletPlugin) Complete(mgr ctrl.Manager) (*KlusterletPlugin, error) {
	//TODO
	for _, r := range k.reconcilers {
		addon := r.(KlusterletPluginController)
		addon.SetClient(mgr.GetClient())
		//addon.SetRecorder(mgr.GetEventRecorderFor(k.Name))
		if err := addon.SetupWithManager(mgr); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func (k *KlusterletPlugin) GetOptions() manager.Options {
	return k.options
}

func (k *KlusterletPlugin) GetCRDs() embed.FS {
	return k.crds
}

func (k *KlusterletPlugin) GetPermissions() embed.FS {
	return k.permissions
}
