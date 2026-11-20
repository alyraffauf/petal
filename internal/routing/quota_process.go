package routing

import (
	"context"
	"io"
	"os/exec"
)

func startQuotaProcess(ctx context.Context, cmd *exec.Cmd) (io.WriteCloser, io.ReadCloser, func(), error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, nil, nil, err
	}
	closePipes := func() {
		in.Close()
		out.Close()
	}
	// Descendants can hold the other ends open after CommandContext kills the CLI.
	stop := context.AfterFunc(ctx, closePipes)
	return in, out, func() {
		stop()
		closePipes()
		cmd.Process.Kill()
		cmd.Wait()
	}, nil
}
