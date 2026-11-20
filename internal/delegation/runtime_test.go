package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"petal/internal/sessions"
)

type echoAdapter struct {
	version uint16
	receive Receive
	done    chan struct{}
	prompt  func(context.Context, string) (json.RawMessage, error)
	cancel  chan string
}

func (a *echoAdapter) Version() uint16       { return a.version }
func (a *echoAdapter) Done() <-chan struct{} { return a.done }
func (a *echoAdapter) Close()                { close(a.done) }

func (a *echoAdapter) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "session/new":
		return json.RawMessage(`{"sessionId":"child"}`), nil
	case "session/prompt":
		fields := params.(map[string]any)
		if fields["sessionId"] != "child" {
			return nil, errors.New("prompt addressed to the wrong session")
		}
		return a.prompt(ctx, fields["prompt"].([]map[string]string)[0]["text"])
	default:
		return nil, fmt.Errorf("unexpected request %s", method)
	}
}

func (a *echoAdapter) Notify(_ context.Context, method string, params any) error {
	if method != "session/cancel" {
		return fmt.Errorf("unexpected notification %s", method)
	}
	a.cancel <- params.(map[string]string)["sessionId"]
	return nil
}

func (a *echoAdapter) update(ctx context.Context, id string, update string) error {
	params := json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"update":%s}`, id, update))
	_, err := a.receive(ctx, "session/update", params, true)
	return err
}

func testProject(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	for _, id := range []string{"parent", "other-parent"} {
		if _, err := sessions.Create(cwd, sessions.Record{ID: id, Model: "parent-model"}); err != nil {
			t.Fatal(err)
		}
	}
	return cwd
}

func TestDelegateEchoReceipt(t *testing.T) {
	for _, version := range []uint16{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			cwd := testProject(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			release := make(chan struct{})
			acknowledged := make(chan struct{})
			adapter := &echoAdapter{version: version, done: make(chan struct{}), cancel: make(chan string, 1)}
			runtime := New(ctx, func(_ context.Context, model, _ string, receive Receive, setup func(Connection) error) (Connection, error) {
				if model != "child-model" {
					return nil, fmt.Errorf("incorrect routing model %q", model)
				}
				adapter.receive = receive
				return adapter, setup(adapter)
			}, nil)
			adapter.prompt = func(ctx context.Context, task string) (json.RawMessage, error) {
				if version == 2 {
					go func() {
						// The v2 acknowledgement can arrive before the child's output.
						close(acknowledged)
						select {
						case <-release:
						case <-ctx.Done():
							return
						}
						_ = adapter.update(ctx, "child", `{"sessionUpdate":"state_update","state":"running"}`)
						_ = adapter.update(ctx, "child", fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","messageId":"reply","content":{"type":"text","text":%q}}`, "receipt: "+task))
						_ = adapter.update(ctx, "child", `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)
					}()
					return json.RawMessage(`{"messageId":"accepted"}`), nil
				}
				close(acknowledged)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				_ = adapter.update(ctx, "parent", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"parent-only"}}`)
				_ = adapter.update(ctx, "child", `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"user-echo"}}`)
				_ = adapter.update(ctx, "child", fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}`, "receipt: "+task))
				return json.RawMessage(`{"stopReason":"end_turn"}`), nil
			}

			child, err := runtime.Delegate(request, "parent", cwd, "child-model", "child-only task")
			if err != nil {
				t.Fatal(err)
			}
			cancelRequest()
			select {
			case <-acknowledged:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			page, err := runtime.Wait(ctx, "parent", cwd, child.ID, 0, 25*time.Millisecond)
			if err != nil || len(page.Activities) != 0 || page.Result != nil {
				t.Fatalf("wait before output = %+v, %v", page, err)
			}
			close(release)
			page, err = runtime.Wait(ctx, "parent", cwd, child.ID, 0, time.Second)
			if err != nil || len(page.Activities) != 1 || page.NextAfter != 1 ||
				!strings.Contains(string(page.Activities[0].Data), "receipt: child-only task") {
				t.Fatalf("child receipt = %+v, %v", page, err)
			}

			if _, err := runtime.Wait(ctx, "other-parent", cwd, child.ID, 0, time.Second); !errors.Is(err, sessions.ErrChildNotFound) {
				t.Fatalf("foreign parent read = %v", err)
			}
			reconnected := New(ctx, nil, nil)
			page, err = reconnected.Wait(ctx, "parent", cwd, child.ID, page.NextAfter, time.Second)
			if err != nil || len(page.Activities) != 0 || page.Result == nil || page.Result.Status != "completed" || !strings.Contains(string(page.Result.Data), `"answer":"receipt: child-only task"`) {
				t.Fatalf("durable terminal result = %+v, %v", page, err)
			}
		})
	}
}

func TestParentCancelRevokesChildPermission(t *testing.T) {
	for _, version := range []uint16{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			cwd := testProject(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			adapter := &echoAdapter{version: version, done: make(chan struct{}), cancel: make(chan string, 1)}
			permission := make(chan string, 1)
			runtime := New(ctx, func(_ context.Context, _, _ string, receive Receive, setup func(Connection) error) (Connection, error) {
				adapter.receive = receive
				return adapter, setup(adapter)
			}, func(ctx context.Context, parent, child string, family uint16, _ json.RawMessage) (json.RawMessage, error) {
				permission <- fmt.Sprintf("%s/%s/v%d", parent, child, family)
				<-ctx.Done()
				// An approval reaching the bridge after cancellation must be ignored.
				return json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"yes"}}`), nil
			})
			outcome := make(chan any, 1)
			adapter.prompt = func(ctx context.Context, _ string) (json.RawMessage, error) {
				data := json.RawMessage(`{"sessionId":"child","options":[]}`)
				response, err := adapter.receive(ctx, "session/request_permission", data, false)
				if err != nil {
					return nil, err
				}
				outcome <- response
				return nil, ctx.Err()
			}

			child, err := runtime.Delegate(ctx, "parent", cwd, "child-model", "permission task")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-permission:
				if got != fmt.Sprintf("parent/child/v%d", version) {
					t.Fatalf("permission scope = %s", got)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			runtime.CancelParent("other-parent", cwd)
			runtime.CancelParent("parent", cwd)
			select {
			case got := <-outcome:
				if string(got.(json.RawMessage)) != string(cancelledPermission()) {
					t.Fatalf("permission outcome = %s", got)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case id := <-adapter.cancel:
				if id != child.ID {
					t.Fatalf("cancel addressed to %s", id)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			page, err := runtime.Wait(ctx, "parent", cwd, child.ID, 1, time.Second)
			if err != nil || page.Result == nil || page.Result.Status != "cancelled" {
				t.Fatalf("durable cancellation = %+v, %v", page, err)
			}
		})
	}
}
