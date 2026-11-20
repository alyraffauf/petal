package acphost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	acp "github.com/coder/acp-go-sdk"
)

// Leave room for JSON-RPC metadata within the SDK's 10 MiB frame limit.
const acpPayloadBudget = 9 << 20

func (h *Host) readText(ctx context.Context, path string, p acp.ReadTextFileRequest) (any, error) {
	start := int64(1)
	if p.Line != nil {
		start = *p.Line
	}
	if start < 1 || (p.Limit != nil && *p.Limit < 0) {
		return nil, acp.NewInvalidParams("expected a positive line and non-negative limit")
	}
	file, err := h.root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, acp.NewRequestError(-32002, "File not found", map[string]string{"path": p.Path})
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, acp.NewInvalidParams("expected a regular file")
	}
	if p.Limit != nil && *p.Limit == 0 {
		return acp.ReadTextFileResponse{}, nil
	}
	// Return a protocol error while the connection can still carry it. The
	// SDK's 10 MiB limit applies to the encoded frame, including JSON escapes.
	tooLarge := func() error {
		return acp.NewInvalidParams("read exceeds the ACP frame budget; request a smaller line range")
	}
	r := bufio.NewReader(file)
	var content strings.Builder
	for line := int64(1); p.Limit == nil || line < start || line-start < *p.Limit; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := r.ReadSlice('\n')
		if line >= start {
			if content.Len()+len(chunk) > acpPayloadBudget {
				return nil, tooLarge()
			}
			content.Write(chunk)
		}
		switch err {
		case io.EOF:
			goto done
		case bufio.ErrBufferFull:
		case nil:
			line++
		default:
			return nil, err
		}
	}
done:
	value := content.String()
	if len(value) > acpPayloadBudget/6 {
		wire, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if len(wire) > acpPayloadBudget {
			return nil, tooLarge()
		}
	}
	return acp.ReadTextFileResponse{Content: value}, nil
}
