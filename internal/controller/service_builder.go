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
// access strategy may listen on their own ports, so those are exposed alongside the Jupyter port;
// a Service only forwards traffic on ports it explicitly declares. accessStrategy may be nil,
// in which case only the Jupyter port is exposed.
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
	ports := []corev1.ServicePort{
		{
			Name:       httpScheme,
			Port:       JupyterPort,
			TargetPort: intstr.FromInt(JupyterPort),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	return corev1.ServiceSpec{
		Type:     corev1.ServiceTypeClusterIP,
		Selector: GenerateLabels(workspace.Name),
		Ports:    append(ports, sidecarServicePorts(accessStrategy)...),
	}
}

// sidecarServicePorts derives the Service ports for containers the access strategy injects
// into the workspace pod. Each container port a sidecar declares becomes routable; sidecars
// that declare no ports (for example an agent that only dials outbound) contribute nothing.
//
// Ports colliding with the Jupyter port, or duplicated across sidecars, are skipped and logged.
// Nothing rejects a duplicate containerPort number upstream — core Pod validation only checks
// hostPort conflicts, and CRD structural schemas do not validate embedded container ports at all
// — so an access strategy may legitimately carry one. Skipping keeps the emitted Service valid;
// the API server rejects a Service with a duplicate (protocol, port) pair.
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
// A name is required, not cosmetic: the API server rejects an unnamed port on any Service that
// declares more than one (validateServicePort is called with requireName = len(ports) > 1). So
// every derived port must get one.
//
// Both possible sources are already valid ServicePort names — a ContainerPort name is validated
// as a port name (at most 15 chars, [-a-z0-9]) and a container name as a DNS-1123 label (at most
// 63) — while ServicePort.Name allows a full 63-char DNS-1123 label. Neither can therefore be too
// long or contain an illegal character, so the name is used as-is.
//
// Uniqueness does need handling: ContainerPort names are only unique within a single container,
// so two sidecars may each legally declare the same port name (or default to the same container
// name). A colliding name is qualified with its port number, which the caller has already made
// unique across the Service.
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
