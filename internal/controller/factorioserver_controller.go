/*
Copyright 2026.

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

// Package controller implements the reconciliation logic for the koujouo API.
package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	koujouoanqounetv1 "koujouo.anqou.net/project/api/v1"
)

// FactorioServerReconciler reconciles a FactorioServer object.
type FactorioServerReconciler struct {
	client.Client

	Scheme *runtime.Scheme
}

//nolint:lll // Kubebuilder markers must stay on one line.
// +kubebuilder:rbac:groups=koujouo.anqou.net.koujouo.anqou.net,resources=factorioservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=koujouo.anqou.net.koujouo.anqou.net,resources=factorioservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=koujouo.anqou.net.koujouo.anqou.net,resources=factorioservers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Compare the state specified by the FactorioServer object against the actual
// cluster state, and then perform operations to make the cluster state reflect
// the state specified by the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *FactorioServerReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	return ctrl.Result{}, nil //nolint:exhaustruct_v5 // An empty result requires no requeue.
}

// SetupWithManager sets up the controller with the Manager.
func (r *FactorioServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&koujouoanqounetv1.FactorioServer{}). //nolint:exhaustruct_v5 // The builder only needs the type.
		Named("factorioserver").
		Complete(r)
	if err != nil {
		return fmt.Errorf("failed to build controller for FactorioServer: %w", err)
	}

	return nil
}
