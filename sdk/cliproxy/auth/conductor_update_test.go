package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManager_Update_PreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v"},
		ModelStates: map[string]*ModelState{
			model: {
				Quota: QuotaState{BackoffLevel: backoffLevel},
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	if _, errUpdate := m.Update(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v2"},
	}); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected ModelStates to be preserved")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}

func TestSelectorOptionsWithAvailabilityScope(t *testing.T) {
	t.Parallel()

	opts := selectorOptionsWithAvailabilityScope(cliproxyexecutor.Options{}, "", nil, "qwen")
	if got, _ := opts.Metadata[selectorAvailabilityCacheScopeKey].(string); got != "qwen" {
		t.Fatalf("scope metadata = %q, want %q", got, "qwen")
	}
}

func TestSelectorOptionsWithAvailabilityScope_SkipsPinnedOrRetried(t *testing.T) {
	t.Parallel()

	pinned := selectorOptionsWithAvailabilityScope(cliproxyexecutor.Options{}, "auth-1", nil, "qwen")
	if len(pinned.Metadata) != 0 {
		t.Fatalf("pinned Metadata = %#v, want empty", pinned.Metadata)
	}

	retried := selectorOptionsWithAvailabilityScope(cliproxyexecutor.Options{}, "", map[string]struct{}{"auth-1": {}}, "qwen")
	if len(retried.Metadata) != 0 {
		t.Fatalf("retried Metadata = %#v, want empty", retried.Metadata)
	}
}

func TestProviderSetCacheScope(t *testing.T) {
	t.Parallel()

	scope := providerSetCacheScope(map[string]struct{}{
		"zeta":  {},
		"alpha": {},
	})
	if scope != "mixed:alpha,zeta" {
		t.Fatalf("providerSetCacheScope() = %q, want %q", scope, "mixed:alpha,zeta")
	}
}

func TestManager_RegisterAndUpdate_MaintainsProviderIndex(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, nil, nil)

	if _, err := m.Register(context.Background(), &Auth{ID: "auth-1", Provider: "qwen"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	m.mu.RLock()
	if _, ok := m.authIDsByProvider["qwen"]["auth-1"]; !ok {
		m.mu.RUnlock()
		t.Fatalf("provider index missing initial auth")
	}
	m.mu.RUnlock()

	if _, err := m.Update(context.Background(), &Auth{ID: "auth-1", Provider: "claude"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.authIDsByProvider["qwen"]["auth-1"]; ok {
		t.Fatalf("provider index retained stale provider entry")
	}
	if _, ok := m.authIDsByProvider["claude"]["auth-1"]; !ok {
		t.Fatalf("provider index missing updated provider entry")
	}
}
