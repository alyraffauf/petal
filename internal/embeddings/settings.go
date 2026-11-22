package embeddings

import (
	"fmt"
	"net/url"
	"strings"
)

type Settings struct {
	Provider  string `mapstructure:"provider" json:"provider"`
	BaseURL   string `mapstructure:"base_url" json:"base_url"`
	Model     string `mapstructure:"model" json:"model"`
	APIKeyEnv string `mapstructure:"api_key_env" json:"api_key_env"`
}

func (e Settings) Resolve() (Settings, error) {
	if e.Provider == "" {
		e.Provider = "auto"
	}
	switch e.Provider {
	case "auto", "off":
		if e.BaseURL != "" || e.Model != "" || e.APIKeyEnv != "" {
			return e, fmt.Errorf("select an explicit embedding provider to configure a URL, model or credential")
		}
		if e.Provider == "off" {
			return e, nil
		}
		e.Provider = "ollama"
	case "ollama", "openai-compatible":
	default:
		return e, fmt.Errorf("unknown history embedding provider %q", e.Provider)
	}
	if e.Provider == "ollama" {
		if e.BaseURL == "" {
			e.BaseURL = "http://127.0.0.1:11434"
		}
		if e.Model == "" {
			e.Model = "all-minilm:latest"
		} else if !strings.Contains(e.Model, ":") {
			e.Model += ":latest"
		}
	}
	e.BaseURL = strings.TrimRight(e.BaseURL, "/")
	u, err := url.Parse(e.BaseURL)
	if e.Model == "" {
		return e, fmt.Errorf("history.embeddings.model is required")
	}
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return e, fmt.Errorf("invalid history.embeddings.base_url")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return e, fmt.Errorf("history.embeddings.base_url requires HTTP or HTTPS")
	}
	return e, nil
}
