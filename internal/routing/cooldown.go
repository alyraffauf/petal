package routing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/paths"
)

// An adapter can reject a request before its configured quota probe catches up.
// File timestamps let simultaneous Petal processes share this short backoff.
const rateLimitBackoff = 5 * time.Minute

func limitPath(account string) (string, error) {
	return paths.XDG("STATE", fmt.Sprintf("quota/%x", sha256.Sum256([]byte(account))))
}
func RememberRateLimit(account string, err error) {
	var rpc *acp.RequestError
	if !errors.As(err, &rpc) {
		return
	}
	var data struct{ ErrorKind string }
	_ = json.Unmarshal(rpc.Data, &data)
	if data.ErrorKind != "rate_limit" && rpc.Code != 429 {
		return
	}
	p, e := limitPath(account)
	if e != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0700) == nil {
		_ = os.WriteFile(p, []byte("limited\n"), 0600)
	}
}
func RemainingBackoff(account string) time.Duration {
	p, err := limitPath(account)
	if err != nil {
		return 0
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return max(0, time.Until(info.ModTime().Add(rateLimitBackoff)))
}
