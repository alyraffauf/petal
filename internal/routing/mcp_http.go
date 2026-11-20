package routing

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

type MCPHeader struct {
	Name     string
	Value    *string
	FromEnv  string `mapstructure:"from_env"`
	FromFile string `mapstructure:"from_file"`
}

// HTTP field names use the token grammar from RFC 9110, section 5.6.2.
var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

func (s MCPServer) validateHTTP() error {
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("MCP server %q needs an HTTP or HTTPS URL without userinfo or fragment", s.Name)
	}
	if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 {
		return fmt.Errorf("HTTP MCP server %q cannot set command, args, or env", s.Name)
	}
	names := map[string]bool{}
	for _, h := range s.Headers {
		key := http.CanonicalHeaderKey(h.Name)
		if !headerName.MatchString(h.Name) || names[key] {
			return fmt.Errorf("invalid or duplicate header on MCP server %q", s.Name)
		}
		names[key] = true
		count := 0
		for _, set := range []bool{h.Value != nil, h.FromEnv != "", h.FromFile != ""} {
			if set {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("MCP header %q needs exactly one of value, from_env, or from_file", h.Name)
		}
	}
	return nil
}

// WireDefinition reads credentials only for servers actually sent to an adapter.
func (s MCPServer) WireDefinition() (any, error) {
	if s.URL == "" {
		if s.Env == nil {
			s.Env = []MCPEnvVariable{}
		}
		if s.Args == nil {
			s.Args = []string{}
		}
		return s, nil
	}
	headers := make([]acp.HttpHeader, 0, len(s.Headers))
	for _, h := range s.Headers {
		var v string
		switch {
		case h.Value != nil:
			v = *h.Value
		case h.FromEnv != "":
			var ok bool
			if v, ok = os.LookupEnv(h.FromEnv); !ok {
				return nil, fmt.Errorf("MCP server %q: environment variable %q is unset", s.Name, h.FromEnv)
			}
		default:
			b, err := os.ReadFile(h.FromFile)
			if err != nil {
				return nil, fmt.Errorf("MCP server %q: read header %q: %w", s.Name, h.Name, err)
			}
			v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		}
		if strings.IndexFunc(v, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("invalid value for MCP header %q", h.Name)
		}
		headers = append(headers, acp.HttpHeader{Name: h.Name, Value: v})
	}
	return acp.McpServerHttpInline{Name: s.Name, Type: "http", Url: s.URL, Headers: headers}, nil
}
