/*
Copyright (c) Amazon Web Services
Distributed under the terms of the MIT license
*/

package extensionapi

import (
	"context"

	workspacev1alpha1 "github.com/jupyter-infra/jupyter-k8s/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getAccessStrategy fetches the AccessStrategy for the workspace
func (s *ExtensionServer) getAccessStrategy(workspace *workspacev1alpha1.Workspace) (*workspacev1alpha1.WorkspaceAccessStrategy, error) {
	if workspace.Spec.AccessStrategy == nil {
		return nil, nil // No AccessStrategy configured
	}

	// Determine namespace for the AccessStrategy (same logic as controller)
	accessStrategyNamespace := workspace.Namespace
	if workspace.Spec.AccessStrategy.Namespace != "" {
		accessStrategyNamespace = workspace.Spec.AccessStrategy.Namespace
	}

	var accessStrategy workspacev1alpha1.WorkspaceAccessStrategy
	err := s.k8sClient.Get(context.Background(),
		client.ObjectKey{
			Name:      workspace.Spec.AccessStrategy.Name,
			Namespace: accessStrategyNamespace,
		},
		&accessStrategy)
	if err != nil {
		return nil, err
	}
	return &accessStrategy, nil
}
