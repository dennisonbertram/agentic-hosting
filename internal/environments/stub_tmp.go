package environments

import "context"

const maxPreviewsPerEnv = 5

func (m *Manager) StopAllForTenant(ctx context.Context, tenantID string) {}
