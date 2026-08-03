/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	workspacev1alpha1 "github.com/jupyter-infra/jupyter-k8s/api/v1alpha1"
)

var _ = Describe("ServiceBuilder", func() {
	var (
		ctx            context.Context
		serviceBuilder *ServiceBuilder
		scheme         *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(workspacev1alpha1.AddToScheme(scheme)).To(Succeed())

		serviceBuilder = NewServiceBuilder(scheme)
	})

	Context("Service Updates", func() {
		var (
			workspace       *workspacev1alpha1.Workspace
			existingService *corev1.Service
		)

		BeforeEach(func() {
			workspace = &workspacev1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testWorkspaceName,
					Namespace: testNamespace,
				},
				Spec: workspacev1alpha1.WorkspaceSpec{
					Image: imageBaseNotebook,
				},
			}

			// Create existing service
			var err error
			existingService, err = serviceBuilder.BuildService(workspace, nil)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not detect update when nothing changed", func() {
			needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(needsUpdate).To(BeFalse())
		})

		It("should update service spec correctly", func() {
			err := serviceBuilder.UpdateServiceSpec(ctx, existingService, workspace, nil)
			Expect(err).NotTo(HaveOccurred())

			// Verify the service spec is still correct
			Expect(existingService.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(existingService.Spec.Ports).To(HaveLen(1))
			Expect(existingService.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))
		})

		// A Service read back from the API server carries fields the operator never sets.
		// Comparing or overwriting whole specs would report a spurious diff on every
		// reconcile and then clear immutable fields like ClusterIP on update.
		Context("when the existing service has been defaulted by the API server", func() {
			BeforeEach(func() {
				applyAPIServerDefaults(existingService)
			})

			It("should not detect an update", func() {
				needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(needsUpdate).To(BeFalse())
			})

			It("should preserve server-populated fields when updating", func() {
				err := serviceBuilder.UpdateServiceSpec(ctx, existingService, workspace, nil)
				Expect(err).NotTo(HaveOccurred())

				Expect(existingService.Spec.ClusterIP).To(Equal(testClusterIP))
				Expect(existingService.Spec.ClusterIPs).To(Equal([]string{testClusterIP}))
				Expect(existingService.Spec.SessionAffinity).To(Equal(corev1.ServiceAffinityNone))
				Expect(existingService.Spec.IPFamilies).To(Equal([]corev1.IPFamily{corev1.IPv4Protocol}))
				Expect(existingService.Spec.IPFamilyPolicy).NotTo(BeNil())
				Expect(existingService.Spec.InternalTrafficPolicy).NotTo(BeNil())

				// Operator-owned fields are still reconciled.
				Expect(existingService.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
				Expect(existingService.Spec.Ports).To(HaveLen(1))
				Expect(existingService.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))
			})

			It("should still detect drift in an operator-owned field", func() {
				existingService.Spec.Ports[0].Port = 9999

				needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(needsUpdate).To(BeTrue())

				Expect(serviceBuilder.UpdateServiceSpec(ctx, existingService, workspace, nil)).To(Succeed())
				Expect(existingService.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))
				Expect(existingService.Spec.ClusterIP).To(Equal(testClusterIP))
			})
		})
	})
})

var _ = Describe("ServiceBuilder sidecar ports", func() {
	var (
		ctx            context.Context
		serviceBuilder *ServiceBuilder
		workspace      *workspacev1alpha1.Workspace
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme := runtime.NewScheme()
		Expect(workspacev1alpha1.AddToScheme(scheme)).To(Succeed())
		serviceBuilder = NewServiceBuilder(scheme)

		workspace = &workspacev1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testWorkspaceName,
				Namespace: testNamespace,
			},
			Spec: workspacev1alpha1.WorkspaceSpec{
				Image: imageBaseNotebook,
			},
		}
	})

	// accessStrategyWithContainers builds an access strategy whose pod modifications inject
	// the given sidecar containers.
	accessStrategyWithContainers := func(containers ...corev1.Container) *workspacev1alpha1.WorkspaceAccessStrategy {
		return &workspacev1alpha1.WorkspaceAccessStrategy{
			ObjectMeta: metav1.ObjectMeta{Name: "websocket-access-strategy", Namespace: testNamespace},
			Spec: workspacev1alpha1.WorkspaceAccessStrategySpec{
				DeploymentModifications: &workspacev1alpha1.DeploymentModifications{
					PodModifications: &workspacev1alpha1.PodModifications{
						AdditionalContainers: containers,
					},
				},
			},
		}
	}

	wsProxyContainer := func() corev1.Container {
		return corev1.Container{
			Name:  nameWSProxy,
			Image: "ghcr.io/jupyter-infra/workspace-websocket-proxy:v0.1.0-rc.1",
			Ports: []corev1.ContainerPort{{Name: nameWSProxy, ContainerPort: 8080}},
		}
	}

	Context("when no sidecar ports are declared", func() {
		It("exposes only the application port with a nil access strategy", func() {
			service, err := serviceBuilder.BuildService(workspace, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Name).To(Equal(httpScheme))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))
		})

		It("exposes only the application port when the access strategy has no deployment modifications", func() {
			service, err := serviceBuilder.BuildService(workspace, &workspacev1alpha1.WorkspaceAccessStrategy{})
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(1))
		})

		// The SSM sidecar is exactly this shape: it dials outbound, so it declares no ports.
		It("exposes only the application port when a sidecar declares no ports", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  "ssm-agent-sidecar",
				Image: "example.com/ssm-agent:latest",
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))
		})
	})

	Context("when a sidecar declares a port", func() {
		It("exposes it alongside the application port", func() {
			service, err := serviceBuilder.BuildService(workspace, accessStrategyWithContainers(wsProxyContainer()))
			Expect(err).NotTo(HaveOccurred())

			Expect(service.Spec.Ports).To(HaveLen(2))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(JupyterPort)))

			wsPort := service.Spec.Ports[1]
			Expect(wsPort.Name).To(Equal(nameWSProxy))
			Expect(wsPort.Port).To(Equal(int32(8080)))
			Expect(wsPort.TargetPort).To(Equal(intstr.FromInt32(8080)))
			Expect(wsPort.Protocol).To(Equal(corev1.ProtocolTCP))
		})

		It("exposes every port declared across multiple sidecars", func() {
			accessStrategy := accessStrategyWithContainers(
				wsProxyContainer(),
				corev1.Container{
					Name:  nameMetrics,
					Ports: []corev1.ContainerPort{{Name: nameMetrics, ContainerPort: 9090}},
				},
			)

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(3))
			Expect(service.Spec.Ports[2].Name).To(Equal(nameMetrics))
			Expect(service.Spec.Ports[2].Port).To(Equal(int32(9090)))
		})

		It("preserves a non-TCP protocol", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  "dns-sidecar",
				Ports: []corev1.ContainerPort{{Name: "dns", ContainerPort: 5353, Protocol: corev1.ProtocolUDP}},
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports[1].Protocol).To(Equal(corev1.ProtocolUDP))
		})
	})

	Context("port naming", func() {
		It("falls back to the container name when the port is unnamed", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  nameWSProxy,
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports[1].Name).To(Equal(nameWSProxy))
		})

		// ServicePort.Name is a DNS-1123 label (up to 63 chars), and a container name is
		// validated as one too, so a long container name needs no truncation. Truncating
		// would corrupt a legal name into a different string.
		It("uses a long container name verbatim", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  "an-extremely-long-sidecar-container-name",
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports[1].Name).To(Equal("an-extremely-long-sidecar-container-name"))
		})

		It("disambiguates colliding names deterministically", func() {
			accessStrategy := accessStrategyWithContainers(
				corev1.Container{Name: "proxy", Ports: []corev1.ContainerPort{{Name: "shared", ContainerPort: 8080}}},
				corev1.Container{Name: "other", Ports: []corev1.ContainerPort{{Name: "shared", ContainerPort: 9090}}},
			)

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(3))
			Expect(service.Spec.Ports[1].Name).To(Equal("shared"))
			Expect(service.Spec.Ports[2].Name).To(Equal("shared-9090"))
		})

		// The API server requires a name on every port of a multi-port Service
		// (validateServicePort's requireName is len(spec.ports) > 1), so an unnamed
		// derived port would make the whole Service invalid.
		It("names every port whenever more than one is exposed", func() {
			accessStrategy := accessStrategyWithContainers(
				corev1.Container{Name: nameWSProxy, Ports: []corev1.ContainerPort{{ContainerPort: 8080}}},
				corev1.Container{Name: nameMetrics, Ports: []corev1.ContainerPort{{ContainerPort: 9090}}},
			)

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(3))
			for _, port := range service.Spec.Ports {
				Expect(port.Name).NotTo(BeEmpty())
			}
		})

		It("does not collide with the application port name", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  "sidecar",
				Ports: []corev1.ContainerPort{{Name: httpScheme, ContainerPort: 8080}},
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports[1].Name).To(Equal("http-8080"))
		})
	})

	Context("duplicate port numbers", func() {
		It("skips a sidecar port that collides with the application port", func() {
			accessStrategy := accessStrategyWithContainers(corev1.Container{
				Name:  "shadow",
				Ports: []corev1.ContainerPort{{Name: "shadow", ContainerPort: JupyterPort}},
			})

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Name).To(Equal(httpScheme))
		})

		It("skips a port number already claimed by another sidecar", func() {
			accessStrategy := accessStrategyWithContainers(
				corev1.Container{Name: "first", Ports: []corev1.ContainerPort{{Name: "first", ContainerPort: 8080}}},
				corev1.Container{Name: "second", Ports: []corev1.ContainerPort{{Name: "second", ContainerPort: 8080}}},
			)

			service, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(service.Spec.Ports).To(HaveLen(2))
			Expect(service.Spec.Ports[1].Name).To(Equal("first"))
		})
	})

	Context("reconciling an existing service", func() {
		It("detects that a single-port service must gain the sidecar port", func() {
			existingService, err := serviceBuilder.BuildService(workspace, nil)
			Expect(err).NotTo(HaveOccurred())
			applyAPIServerDefaults(existingService)

			accessStrategy := accessStrategyWithContainers(wsProxyContainer())

			needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(needsUpdate).To(BeTrue())

			Expect(serviceBuilder.UpdateServiceSpec(ctx, existingService, workspace, accessStrategy)).To(Succeed())
			Expect(existingService.Spec.Ports).To(HaveLen(2))
			Expect(existingService.Spec.Ports[1].Port).To(Equal(int32(8080)))
			// The update must not clobber server-populated immutable fields.
			Expect(existingService.Spec.ClusterIP).To(Equal(testClusterIP))
		})

		It("detects no update once the sidecar port is already exposed", func() {
			accessStrategy := accessStrategyWithContainers(wsProxyContainer())
			existingService, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			applyAPIServerDefaults(existingService)

			needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(needsUpdate).To(BeFalse())
		})

		It("removes the sidecar port when the access strategy no longer declares it", func() {
			accessStrategy := accessStrategyWithContainers(wsProxyContainer())
			existingService, err := serviceBuilder.BuildService(workspace, accessStrategy)
			Expect(err).NotTo(HaveOccurred())
			applyAPIServerDefaults(existingService)

			needsUpdate, err := serviceBuilder.NeedsUpdate(ctx, existingService, workspace, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(needsUpdate).To(BeTrue())

			Expect(serviceBuilder.UpdateServiceSpec(ctx, existingService, workspace, nil)).To(Succeed())
			Expect(existingService.Spec.Ports).To(HaveLen(1))
		})
	})
})

const (
	// testClusterIP is the ClusterIP the fake API server "assigns" in tests.
	testClusterIP = "10.100.42.7"
	// nameWSProxy is the WebSocket proxy sidecar container / port name used across tests.
	nameWSProxy = "ws-proxy"
	// nameMetrics is a second sidecar container / port name used across tests.
	nameMetrics = "metrics"
)

// applyAPIServerDefaults mutates a freshly built Service to look like one read back from
// the API server, which populates these fields on create.
func applyAPIServerDefaults(service *corev1.Service) {
	ipFamilyPolicy := corev1.IPFamilyPolicySingleStack
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster

	service.Spec.ClusterIP = testClusterIP
	service.Spec.ClusterIPs = []string{testClusterIP}
	service.Spec.SessionAffinity = corev1.ServiceAffinityNone
	service.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	service.Spec.IPFamilyPolicy = &ipFamilyPolicy
	service.Spec.InternalTrafficPolicy = &internalTrafficPolicy
}
