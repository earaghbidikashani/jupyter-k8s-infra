/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package controller

import (
	"context"
	"fmt"

	workspacev1alpha1 "github.com/jupyter-infra/jupyter-k8s/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ServiceBuilder handles creation of Service resources for Workspace
type ServiceBuilder struct {
	scheme *runtime.Scheme
}

// NewServiceBuilder creates a new ServiceBuilder
func NewServiceBuilder(scheme *runtime.Scheme) *ServiceBuilder {
	return &ServiceBuilder{
		scheme: scheme,
	}
}

// BuildService creates a Service resource for the given Workspace.
//
// The Service describes the ports the workspace pod actually serves. Sidecars injected by the
// access strategy may listen on their own ports, so those are exposed alongside the application
// port; a Service only forwards traffic on ports it explicitly declares. accessStrategy may be
// nil, in which case only the application port is exposed.
func (sb *ServiceBuilder) BuildService(
	workspace *workspacev1alpha1.Workspace,
	accessStrategy *workspacev1alpha1.WorkspaceAccessStrategy,
) (*corev1.Service, error) {
	service := &corev1.Service{
		ObjectMeta: sb.buildObjectMeta(workspace),
		Spec:       sb.buildServiceSpec(workspace, accessStrategy),
	}

	// Set owner reference for garbage collection
	if err := controllerutil.SetControllerReference(workspace, service, sb.scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	return service, nil
}

// buildObjectMeta creates the metadata for the Service
func (sb *ServiceBuilder) buildObjectMeta(workspace *workspacev1alpha1.Workspace) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      GenerateServiceName(workspace.Name),
		Namespace: workspace.Namespace,
		Labels:    GenerateLabels(workspace.Name),
	}
}

// buildServiceSpec creates the service specification
func (sb *ServiceBuilder) buildServiceSpec(
	workspace *workspacev1alpha1.Workspace,
	accessStrategy *workspacev1alpha1.WorkspaceAccessStrategy,
) corev1.ServiceSpec {
	sidecarPorts := sidecarServicePorts(accessStrategy)
	ports := make([]corev1.ServicePort, 0, 1+len(sidecarPorts))
	ports = append(ports, corev1.ServicePort{
		Name:       httpScheme,
		Port:       JupyterPort,
		TargetPort: intstr.FromInt(JupyterPort),
		Protocol:   corev1.ProtocolTCP,
	})
	ports = append(ports, sidecarPorts...)

	return corev1.ServiceSpec{
		Type:     corev1.ServiceTypeClusterIP,
		Selector: GenerateLabels(workspace.Name),
		Ports:    ports,
	}
}

// sidecarServicePorts derives the Service ports for containers the access strategy injects
// into the workspace pod. Each container port a sidecar declares becomes routable; sidecars
// that declare no ports (for example an agent that only dials outbound) contribute nothing.
//
// Ports colliding with the application port, or duplicated across sidecars, are skipped and
// logged: nothing upstream rejects a duplicate containerPort, but the API server does reject a
// Service with a duplicate (protocol, port) pair.
func sidecarServicePorts(accessStrategy *workspacev1alpha1.WorkspaceAccessStrategy) []corev1.ServicePort {
	if accessStrategy == nil ||
		accessStrategy.Spec.DeploymentModifications == nil ||
		accessStrategy.Spec.DeploymentModifications.PodModifications == nil {
		return nil
	}

	containers := accessStrategy.Spec.DeploymentModifications.PodModifications.AdditionalContainers
	if len(containers) == 0 {
		return nil
	}

	var ports []corev1.ServicePort
	usedPorts := map[int32]bool{JupyterPort: true}
	usedNames := map[string]bool{httpScheme: true}

	for _, container := range containers {
		for _, containerPort := range container.Ports {
			if usedPorts[containerPort.ContainerPort] {
				logf.Log.V(1).Info("Skipping sidecar container port already exposed on the workspace service",
					"accessStrategy", accessStrategy.Name,
					"container", container.Name,
					"port", containerPort.ContainerPort)
				continue
			}
			usedPorts[containerPort.ContainerPort] = true

			protocol := containerPort.Protocol
			if protocol == "" {
				protocol = corev1.ProtocolTCP
			}

			name := servicePortName(containerPort, container.Name, usedNames)
			usedNames[name] = true

			ports = append(ports, corev1.ServicePort{
				Name:       name,
				Port:       containerPort.ContainerPort,
				TargetPort: intstr.FromInt32(containerPort.ContainerPort),
				Protocol:   protocol,
			})

			logf.Log.V(1).Info("Exposing sidecar container port on the workspace service",
				"accessStrategy", accessStrategy.Name,
				"container", container.Name,
				"port", containerPort.ContainerPort,
				"portName", name)
		}
	}

	return ports
}

// servicePortName picks a unique ServicePort name for a sidecar container port.
//
// The API server requires a name on every port once a Service declares more than one, and
// ContainerPort.Name is optional, so it falls back to the container name. Both are already valid
// ServicePort names. Container port names are unique only within a container, so a name already
// taken is qualified with its port number — unique across the Service by construction.
func servicePortName(containerPort corev1.ContainerPort, containerName string, usedNames map[string]bool) string {
	candidate := containerPort.Name
	if candidate == "" {
		candidate = containerName
	}

	if !usedNames[candidate] {
		return candidate
	}

	// The caller skips duplicate port numbers, so every emitted port has a distinct number:
	// qualifying with it yields a distinct name without searching for a free one.
	return fmt.Sprintf("%s-%d", candidate, containerPort.ContainerPort)
}

// NeedsUpdate checks if the existing service needs to be updated based on workspace changes.
//
// Only the fields the operator owns are compared. The rest of a live ServiceSpec is populated
// by the API server (ClusterIP, ClusterIPs, IPFamilies, IPFamilyPolicy, SessionAffinity,
// InternalTrafficPolicy), and a freshly built desired spec leaves all of those empty —
// comparing whole specs would therefore always report a difference.
func (sb *ServiceBuilder) NeedsUpdate(
	ctx context.Context,
	existingService *corev1.Service,
	workspace *workspacev1alpha1.Workspace,
	accessStrategy *workspacev1alpha1.WorkspaceAccessStrategy,
) (bool, error) {
	// Build the desired service spec
	desiredService, err := sb.BuildService(workspace, accessStrategy)
	if err != nil {
		return false, fmt.Errorf("failed to build desired service: %w", err)
	}

	return !serviceSpecMatches(&existingService.Spec, &desiredService.Spec), nil
}

// UpdateServiceSpec updates the existing service with the desired spec.
//
// Only the operator-owned fields are assigned. Overwriting the whole spec would clear
// server-populated immutable fields such as ClusterIP, which the API server then rejects.
func (sb *ServiceBuilder) UpdateServiceSpec(
	ctx context.Context,
	existingService *corev1.Service,
	workspace *workspacev1alpha1.Workspace,
	accessStrategy *workspacev1alpha1.WorkspaceAccessStrategy,
) error {
	// Build the desired service spec
	desiredService, err := sb.BuildService(workspace, accessStrategy)
	if err != nil {
		return fmt.Errorf("failed to build desired service: %w", err)
	}

	applyServiceSpec(&existingService.Spec, &desiredService.Spec)

	return nil
}

// serviceSpecMatches reports whether the operator-owned fields of existing already match desired.
func serviceSpecMatches(existing, desired *corev1.ServiceSpec) bool {
	return existing.Type == desired.Type &&
		equality.Semantic.DeepEqual(existing.Selector, desired.Selector) &&
		equality.Semantic.DeepEqual(existing.Ports, desired.Ports)
}

// applyServiceSpec copies the operator-owned fields of desired onto existing,
// leaving every server-populated field untouched.
func applyServiceSpec(existing, desired *corev1.ServiceSpec) {
	existing.Type = desired.Type
	existing.Selector = desired.Selector
	existing.Ports = desired.Ports
}
