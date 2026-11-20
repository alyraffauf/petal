package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"
)

type promptMessage struct {
	text    string
	content []map[string]any
}

func attachment(cwd, path string, caps acp.PromptCapabilities) (promptMessage, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return promptMessage{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return promptMessage{}, err
	}
	// Leave room for the ACP envelope and base64 expansion in the SDK's 10 MiB frame.
	const maxBytes = 7 << 20
	if !info.Mode().IsRegular() {
		return promptMessage{}, fmt.Errorf("choose a regular file")
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	link := func() promptMessage {
		return promptMessage{"Linked " + filepath.Base(path), []map[string]any{{
			"type": "resource_link", "uri": uri, "name": filepath.Base(path),
		}}}
	}
	if info.Size() > maxBytes || !(caps.Image || caps.Audio || caps.EmbeddedContext) {
		return link(), nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return promptMessage{}, err
	}
	if len(data) > maxBytes {
		return link(), nil
	}
	typ := mime.TypeByExtension(filepath.Ext(path))
	if typ == "" {
		typ = http.DetectContentType(data)
	}
	block := map[string]any{"mimeType": typ}
	switch {
	case strings.HasPrefix(typ, "image/") && caps.Image:
		block["type"] = "image"
		block["data"] = base64.StdEncoding.EncodeToString(data)
	case strings.HasPrefix(typ, "audio/") && caps.Audio:
		block["type"] = "audio"
		block["data"] = base64.StdEncoding.EncodeToString(data)
	case caps.EmbeddedContext:
		resource := map[string]any{"uri": uri, "mimeType": typ}
		if utf8.Valid(data) && !strings.ContainsRune(string(data), 0) {
			text := string(data)
			// Quotes, controls, and HTML escapes can expand text beyond the
			// wire budget even when the original file fits.
			if len(data) > maxBytes/6 {
				wire, err := json.Marshal(text)
				if err != nil {
					return promptMessage{}, err
				}
				if len(wire) > 9<<20 {
					return link(), nil
				}
			}
			resource["text"] = text
		} else {
			resource["blob"] = base64.StdEncoding.EncodeToString(data)
		}
		block = map[string]any{"type": "resource", "resource": resource}
	default:
		return link(), nil
	}
	return promptMessage{"Attached " + filepath.Base(path), []map[string]any{block}}, nil
}
