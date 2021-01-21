/*
Copyright 2019 The Kubernetes Authors.

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
	"github.com/go-logr/logr"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeecv1 "kubeec/api/v1"
)

// ECServiceReconciler reconciles a ECService object
type ECServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubeec.inspursoft.com,resources=ecservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeec.inspursoft.com,resources=ecservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ECServiceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("ecservice", req.NamespacedName)

	// your logic here
	log.Info("fetching ECService resource")
	ecService := kubeecv1.ECService{}
	fmt.Println("***********req: ", req)
	if err := r.Client.Get(ctx, req.NamespacedName, &ecService); err != nil {
		log.Error(err, "failed to get ECService resource")
		// Ignore NotFound errors as they will be retried automatically if the
		// resource is created in future.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.cleanupOwnedResources(ctx, log, &ecService); err != nil {
		log.Error(err, "failed to clean up old Deployment resources for this ECService")
		return ctrl.Result{}, err
	}

	log = log.WithValues("deployment_name", ecService.Spec.DeploymentName)

	log.Info("checking if an existing Deployment exists for this resource")
	deployment := apps.Deployment{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: ecService.Namespace, Name: ecService.Spec.DeploymentName}, &deployment)
	if apierrors.IsNotFound(err) {
		log.Info("could not find existing Deployment for ECService, creating one...")

		deployment = *buildDeployment(ecService)
		if err := r.Client.Create(ctx, &deployment); err != nil {
			log.Error(err, "failed to create Deployment resource")
			return ctrl.Result{}, err
		}

		fmt.Println("***********core.EventTypeNormal: ", core.EventTypeNormal)
		r.Recorder.Eventf(&ecService, core.EventTypeNormal, "Created", "Created deployment %s", deployment.Name)
		log.Info("created Deployment resource for ECService")
		return ctrl.Result{}, nil
	}
	if err != nil {
		log.Error(err, "failed to get Deployment for ECService resource")
		return ctrl.Result{}, err
	}

	log.Info("existing Deployment resource already exists for ECService, checking replica count")

	expectedReplicas := int32(1)
	if ecService.Spec.Replicas != nil {
		expectedReplicas = *ecService.Spec.Replicas
	}
	if *deployment.Spec.Replicas != expectedReplicas {
		log.Info("updating replica count", "old_count", *deployment.Spec.Replicas, "new_count", expectedReplicas)

		deployment.Spec.Replicas = &expectedReplicas
		if err := r.Client.Update(ctx, &deployment); err != nil {
			log.Error(err, "failed to Deployment update replica count")
			return ctrl.Result{}, err
		}

		r.Recorder.Eventf(&ecService, core.EventTypeNormal, "Scaled", "Scaled deployment %q to %d replicas", deployment.Name, expectedReplicas)

		return ctrl.Result{}, nil
	}

	log.Info("replica count up to date", "replica_count", *deployment.Spec.Replicas)

	log.Info("updating ECService resource status")
	ecService.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	if r.Client.Status().Update(ctx, &ecService); err != nil {
		log.Error(err, "failed to update ECService status")
		return ctrl.Result{}, err
	}

	log.Info("resource status synced")

	return ctrl.Result{}, nil
}

// cleanupOwnedResources will Delete any existing Deployment resources that
// were created for the given ECService that no longer match the
// ecService.spec.deploymentName field.
func (r *ECServiceReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, ecService *kubeecv1.ECService) error {
	log.Info("finding existing Deployments for ECService resource")

	// List all deployment resources owned by this ECService
	var deployments apps.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(ecService.Namespace), client.MatchingField(deploymentOwnerKey, ecService.Name)); err != nil {
		return err
	}

	deleted := 0
	for _, depl := range deployments.Items {
		if depl.Name == ecService.Spec.DeploymentName {
			// If this deployment's name matches the one on the ECService resource
			// then do not delete it.
			continue
		}

		if err := r.Client.Delete(ctx, &depl); err != nil {
			log.Error(err, "failed to delete Deployment resource")
			return err
		}

		r.Recorder.Eventf(ecService, core.EventTypeNormal, "Deleted", "Deleted deployment %q", depl.Name)
		deleted++
	}

	log.Info("finished cleaning up old Deployment resources", "number_deleted", deleted)

	return nil
}

func buildDeployment(ecService kubeecv1.ECService) *apps.Deployment {
	deployment := apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ecService.Spec.DeploymentName,
			Namespace:       ecService.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&ecService, kubeecv1.GroupVersion.WithKind("ECService"))},
		},
		Spec: apps.DeploymentSpec{
			Replicas: ecService.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"kubeec.inspursoft.com/deployment-name": ecService.Spec.DeploymentName,
				},
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kubeec.inspursoft.com/deployment-name": ecService.Spec.DeploymentName,
					},
				},
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Name:  "nginx",
							Image: "nginx:latest",
						},
					},
				},
			},
		},
	}
	return &deployment
}

var (
	deploymentOwnerKey = ".metadata.controller"
)

func (r *ECServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(&apps.Deployment{}, deploymentOwnerKey, func(rawObj runtime.Object) []string {
		// grab the Deployment object, extract the owner...
		depl := rawObj.(*apps.Deployment)
		owner := metav1.GetControllerOf(depl)
		if owner == nil {
			return nil
		}
		// ...make sure it's a ECService...
		if owner.APIVersion != kubeecv1.GroupVersion.String() || owner.Kind != "ECService" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeecv1.ECService{}).
		Owns(&apps.Deployment{}).
		Complete(r)
}
