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
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/elb"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// AWSMachineReconciler reconciles a AwsMachine object
type AWSMachineReconciler struct {
	client.Client
	Log            logr.Logger
	Recorder       record.EventRecorder
	serviceFactory func(*scope.ClusterScope) services.EC2MachineInterface
}

func (r *AWSMachineReconciler) getEC2Service(scope *scope.ClusterScope) services.EC2MachineInterface {
	if r.serviceFactory != nil {
		return r.serviceFactory(scope)
	}

	return ec2.NewService(scope)
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

func (r *AWSMachineReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx := context.TODO()
	logger := r.Log.WithValues("namespace", req.Namespace, "awsMachine", req.Name)

	// Fetch the AWSMachine instance.
	awsMachine := &infrav1.AWSMachine{}
	err := r.Get(ctx, req.NamespacedName, awsMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Fetch the Machine.
	machine, err := util.GetOwnerMachine(ctx, r.Client, awsMachine.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}
	if machine == nil {
		logger.Info("Machine Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("machine", machine.Name)

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		logger.Info("Machine is missing cluster label or cluster does not exist")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("cluster", cluster.Name)

	awsCluster := &infrav1.AWSCluster{}

	awsClusterName := client.ObjectKey{
		Namespace: awsMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, awsClusterName, awsCluster); err != nil {
		logger.Info("AWSCluster is not available yet")
		return reconcile.Result{}, nil
	}

	logger = logger.WithValues("awsCluster", awsCluster.Name)

	// Create the cluster scope
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:     r.Client,
		Logger:     logger,
		Cluster:    cluster,
		AWSCluster: awsCluster,
	})
	if err != nil {
		return reconcile.Result{}, err
	}

	// Create the machine scope
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Logger:     logger,
		Client:     r.Client,
		Cluster:    cluster,
		Machine:    machine,
		AWSCluster: awsCluster,
		AWSMachine: awsMachine,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any AWSMachine changes.
	defer func() {
		if err := machineScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted machines
	if !awsMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(machineScope, clusterScope)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, machineScope, clusterScope)
}

func (r *AWSMachineReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1.AWSMachine{}).
		Watches(
			&source.Kind{Type: &clusterv1.Machine{}},
			&handler.EnqueueRequestsFromMapFunc{
				ToRequests: util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("AWSMachine")),
			},
		).
		Watches(
			&source.Kind{Type: &infrav1.AWSCluster{}},
			&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.AWSClusterToAWSMachines)},
		).
		Complete(r)
}

func (r *AWSMachineReconciler) reconcileDelete(machineScope *scope.MachineScope, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	machineScope.Info("Handling deleted AWSMachine")

	ec2Service := r.getEC2Service(clusterScope)

	instance, err := r.findInstance(machineScope, ec2Service)
	if err != nil {
		return reconcile.Result{}, err
	}

	if instance == nil {
		// The machine was never created or was deleted by some other entity
		// One way to reach this state:
		// 1. Scale deployment to 0
		// 2. Rename EC2 machine, and delete ProviderID from spec of both Machine
		// and AWSMachine
		// 3. Issue a delete
		// 4. Scale controller deployment to 1
		machineScope.V(2).Info("Unable to locate EC2 instance by ID or tags")
		r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeWarning, "NoInstanceFound", "Unable to find matching EC2 instance")
		machineScope.AWSMachine.Finalizers = util.Filter(machineScope.AWSMachine.Finalizers, infrav1.MachineFinalizer)
		return reconcile.Result{}, nil
	}

	machineScope.V(3).Info("EC2 instance found matching deleted AWSMachine", "instance-id", instance.ID)

	// Check the instance state. If it's already shutting down or terminated,
	// do nothing. Otherwise attempt to delete it.
	// This decision is based on the ec2-instance-lifecycle graph at
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html
	switch instance.State {
	case infrav1.InstanceStateShuttingDown, infrav1.InstanceStateTerminated:
		machineScope.Info("EC2 instance is shutting down or already terminated", "instance-id", instance.ID)
	default:
		machineScope.Info("Terminating EC2 instance", "instance-id", instance.ID)
		if err := ec2Service.TerminateInstanceAndWait(instance.ID); err != nil {
			r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeWarning, "FailedTerminate", "Failed to terminate instance %q: %v", instance.ID, err)
			return reconcile.Result{}, errors.Wrap(err, "failed to terminate instance")
		}

		// If the AWSMachine specifies Network Interfaces, detach the cluster's core Security Groups from them as part of deletion.
		if len(machineScope.AWSMachine.Spec.NetworkInterfaces) > 0 {
			core, err := ec2Service.GetCoreSecurityGroups(machineScope)
			if err != nil {
				return reconcile.Result{}, errors.Wrap(err, "failed to get core security groups to detach from instance's network interfaces")
			}

			machineScope.V(3).Info(
				"Detaching security groups from provided network interface",
				"groups", core,
				"instanceID", instance.ID,
			)

			for _, id := range machineScope.AWSMachine.Spec.NetworkInterfaces {
				if err := ec2Service.DetachSecurityGroupsFromNetworkInterface(core, id); err != nil {
					return reconcile.Result{}, errors.Wrap(err, "failed to detach security groups from instance's network interfaces")
				}
			}
		}

		machineScope.Info("EC2 instance successfully terminated", "instance-id", instance.ID)
		r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeNormal, "SuccessfulTerminate", "Terminated instance %q", instance.ID)
	}

	// Instance is deleted so remove the finalizer.
	machineScope.AWSMachine.Finalizers = util.Filter(machineScope.AWSMachine.Finalizers, infrav1.MachineFinalizer)

	return reconcile.Result{}, nil
}

// findInstance queries the EC2 apis and retrieves the instance if it exists, returns nil otherwise.
func (r *AWSMachineReconciler) findInstance(scope *scope.MachineScope, ec2svc services.EC2MachineInterface) (*infrav1.Instance, error) {
	// Parse the ProviderID.
	pid, err := noderefutil.NewProviderID(scope.GetProviderID())
	if err != nil && err != noderefutil.ErrEmptyProviderID {
		return nil, errors.Wrapf(err, "failed to parse Spec.ProviderID")
	}

	// If the ProviderID is populated, describe the instance using the ID.
	if err == nil {
		instance, err := ec2svc.InstanceIfExists(pointer.StringPtr(pid.ID()))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to query AWSMachine instance")
		}
		return instance, nil
	}

	// If the ProviderID is empty, try to query the instance using tags.
	instance, err := ec2svc.GetRunningInstanceByTags(scope)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to query AWSMachine instance by tags")
	}

	return instance, nil
}

func (r *AWSMachineReconciler) reconcileNormal(ctx context.Context, machineScope *scope.MachineScope, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	machineScope.Info("Reconciling AWSMachine")
	// If the AWSMachine is in an error state, return early.
	if machineScope.AWSMachine.Status.FailureReason != nil || machineScope.AWSMachine.Status.FailureMessage != nil {
		machineScope.Info("Error state detected, skipping reconciliation")
		return reconcile.Result{}, nil
	}

	// If the AWSMachine doesn't have our finalizer, add it.
	if !util.Contains(machineScope.AWSMachine.Finalizers, infrav1.MachineFinalizer) {
		machineScope.V(1).Info("Adding Cluster API Provider AWS finalizer")
		machineScope.AWSMachine.Finalizers = append(machineScope.AWSMachine.Finalizers, infrav1.MachineFinalizer)
	}

	if !machineScope.Cluster.Status.InfrastructureReady {
		machineScope.Info("Cluster infrastructure is not ready yet")
		return reconcile.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	if machineScope.Machine.Spec.Bootstrap.DataSecretName == nil {
		machineScope.Info("Bootstrap data secret reference is not yet available")
		return reconcile.Result{}, nil
	}

	ec2svc := r.getEC2Service(clusterScope)

	// Get or create the instance.
	instance, err := r.getOrCreate(machineScope, ec2svc)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Set an failure message if we couldn't find the instance.
	if instance == nil {
		machineScope.Info("EC2 instance cannot be found")
		machineScope.SetFailureReason(capierrors.UpdateMachineError)
		machineScope.SetFailureMessage(errors.New("EC2 instance cannot be found"))
		return reconcile.Result{}, nil
	}

	// Make sure Spec.ProviderID is always set.
	machineScope.SetProviderID(fmt.Sprintf("aws:////%s", instance.ID))

	// See https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html

	existingInstanceState := machineScope.GetInstanceState()
	machineScope.SetInstanceState(instance.State)

	// Proceed to reconcile the AWSMachine state.
	if existingInstanceState == nil || *existingInstanceState != instance.State {
		machineScope.Info("EC2 instance state changed", "state", instance.State, "instance-id", *machineScope.GetInstanceID())
	}

	machineScope.SetAddresses(instance.Addresses)

	switch instance.State {
	case infrav1.InstanceStatePending, infrav1.InstanceStateStopping, infrav1.InstanceStateStopped:
		machineScope.SetNotReady()
	case infrav1.InstanceStateRunning:
		machineScope.SetReady()
	case infrav1.InstanceStateShuttingDown, infrav1.InstanceStateTerminated:
		machineScope.SetNotReady()
		machineScope.Info("Unexpected EC2 instance termination", "state", instance.State, "instance-id", *machineScope.GetInstanceID())
		r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeWarning, "InstanceUnexpectedTermination", "Unexpected EC2 instance termination")
	default:
		machineScope.SetNotReady()
		machineScope.Info("EC2 instance state is undefined", "state", instance.State, "instance-id", *machineScope.GetInstanceID())
		r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeWarning, "InstanceUnhandledState", "EC2 instance state is undefined")
		machineScope.SetFailureReason(capierrors.UpdateMachineError)
		machineScope.SetFailureMessage(errors.Errorf("EC2 instance state %q is undefined", instance.State))
	}

	if instance.State == infrav1.InstanceStateTerminated {
		machineScope.SetFailureReason(capierrors.UpdateMachineError)
		machineScope.SetFailureMessage(errors.Errorf("EC2 instance state %q is unexpected", instance.State))
	}

	if err := r.reconcileLBAttachment(machineScope, clusterScope, instance); err != nil {
		return reconcile.Result{}, errors.Errorf("failed to reconcile LB attachment: %+v", err)
	}

	// TODO(vincepri): Remove this annotation when clusterctl is no longer relevant.
	machineScope.SetAnnotation("cluster-api-provider-aws", "true")

	existingSecurityGroups, err := ec2svc.GetInstanceSecurityGroups(*machineScope.GetInstanceID())
	if err != nil {
		return reconcile.Result{}, err
	}

	// Ensure that the security groups are correct.
	_, err = r.ensureSecurityGroups(ec2svc, machineScope, machineScope.AWSMachine.Spec.AdditionalSecurityGroups, existingSecurityGroups)
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to apply security groups: %+v", err)
	}

	// Ensure that the tags are correct.
	_, err = r.ensureTags(ec2svc, machineScope.AWSMachine, machineScope.GetInstanceID(), machineScope.AdditionalTags())
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to ensure tags: %+v", err)
	}

	return reconcile.Result{}, nil
}

func (r *AWSMachineReconciler) getOrCreate(scope *scope.MachineScope, ec2svc services.EC2MachineInterface) (*infrav1.Instance, error) {
	instance, err := r.findInstance(scope, ec2svc)
	if err != nil {
		return nil, err
	}

	if instance == nil {
		scope.Info("Creating EC2 instance")
		// Create a new AWSMachine instance if we couldn't find a running instance.
		instance, err = ec2svc.CreateInstance(scope)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create EC2 instance for AWSMachine %s/%s", scope.Namespace(), scope.Name())
		}
	}

	return instance, nil
}

func (r *AWSMachineReconciler) reconcileLBAttachment(machineScope *scope.MachineScope, clusterScope *scope.ClusterScope, i *infrav1.Instance) error {
	if !machineScope.IsControlPlane() {
		return nil
	}

	elbsvc := elb.NewService(clusterScope)
	if err := elbsvc.RegisterInstanceWithAPIServerELB(i); err != nil {
		r.Recorder.Eventf(machineScope.AWSMachine, corev1.EventTypeWarning, "FailedAttachControlPlaneELB",
			"Failed to register control plane instance %q with load balancer: %v", i.ID, err)
		return errors.Wrapf(err, "could not register control plane instance %q with load balancer", i.ID)
	}
	return nil
}

// AWSClusterToAWSMachine is a handler.ToRequestsFunc to be used to enqeue requests for reconciliation
// of AWSMachines.
func (r *AWSMachineReconciler) AWSClusterToAWSMachines(o handler.MapObject) []ctrl.Request {
	result := []ctrl.Request{}

	c, ok := o.Object.(*infrav1.AWSCluster)
	if !ok {
		r.Log.Error(errors.Errorf("expected a AWSCluster but got a %T", o.Object), "failed to get AWSMachine for AWSCluster")
		return nil
	}
	log := r.Log.WithValues("AWSCluster", c.Name, "Namespace", c.Namespace)

	cluster, err := util.GetOwnerCluster(context.TODO(), r.Client, c.ObjectMeta)
	switch {
	case apierrors.IsNotFound(err) || cluster == nil:
		return result
	case err != nil:
		log.Error(err, "failed to get owning cluster")
		return result
	}

	labels := map[string]string{clusterv1.ClusterLabelName: cluster.Name}
	machineList := &clusterv1.MachineList{}
	if err := r.List(context.TODO(), machineList, client.InNamespace(c.Namespace), client.MatchingLabels(labels)); err != nil {
		log.Error(err, "failed to list Machines")
		return nil
	}
	for _, m := range machineList.Items {
		if m.Spec.InfrastructureRef.Name == "" {
			continue
		}
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.InfrastructureRef.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}

	return result
}
