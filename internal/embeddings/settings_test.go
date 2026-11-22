package embeddings

import "testing"

func TestResolveRemoteHTTP(t *testing.T) {
	const endpoint = "http://embeddings.example:11434"
	settings, err := (Settings{Provider: "ollama", BaseURL: endpoint}).Resolve()
	if err != nil || settings.BaseURL != endpoint {
		t.Fatalf("Resolve() returned URL %q, error %v", settings.BaseURL, err)
	}
}
