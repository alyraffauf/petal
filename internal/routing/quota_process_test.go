package routing

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestQuotaProcessCancellationInterruptsReadFromInheritedPipe(t *testing.T) {
	keepOpen, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keepOpen.Close()
	defer release.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The child announces readiness and holds stdout open until the test releases it.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `(printf r; exec /bin/cat <&3) & wait`)
	cmd.ExtraFiles = []*os.File{keepOpen}
	_, out, cleanup, err := startQuotaProcess(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, err := io.ReadFull(out, marker[:])
		ready <- err
		_, err = out.Read(marker[:])
		done <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inherited stdout kept the quota read blocked")
	}
}

func TestScoresReturnsWhenProbeCleanupOutlivesDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Model: "model", Accounts: map[string]Account{
		"stalled": {
			Models: map[string][]string{"model": {"agent"}},
			Usage:  []string{"/bin/sh", "-c", "/bin/sleep 2 & wait"},
		},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		cfg.Scores(ctx)
		close(done)
	}()
	<-ctx.Done()
	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("Scores waited for subprocess cleanup after the deadline")
	}
}
