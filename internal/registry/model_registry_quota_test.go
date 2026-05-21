package registry

import "testing"

func TestRegisterClientPreservesQuotaAndSuspensionForExistingModel(t *testing.T) {
	r := newTestModelRegistry()
	model := "gpt-5.5"

	r.RegisterClient("client-1", "codex", []*ModelInfo{{ID: model, DisplayName: "GPT 5.5"}})
	r.SetModelQuotaExceeded("client-1", model)
	r.SuspendClientModel("client-1", model, "quota")

	r.RegisterClient("client-1", "codex", []*ModelInfo{{ID: model, DisplayName: "GPT 5.5 updated"}})

	registration := r.models[model]
	if registration == nil {
		t.Fatalf("expected model registration to be present")
	}
	if _, ok := registration.QuotaExceededClients["client-1"]; !ok {
		t.Fatalf("expected quota exceeded marker to survive model re-registration")
	}
	if reason, ok := registration.SuspendedClients["client-1"]; !ok || reason != "quota" {
		t.Fatalf("expected suspension marker to survive model re-registration, got reason=%q ok=%v", reason, ok)
	}
}
