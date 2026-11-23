//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	acp "github.com/coder/acp-go-sdk"
)

type pendingQuestion struct {
	question Question
	choices  []string
	reply    chan Answer
}

func sortQuestions(questions []Question) {
	slices.SortFunc(questions, func(a, b Question) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
}

func (s *session) approveHost(ctx context.Context, title string) error {
	raw := bytesOf(map[string]string{"title": title})
	answer, err := s.ask(ctx, "host/approve", raw, []string{"allow", "deny"})
	if err != nil {
		return err
	}
	if answer.OptionID != "allow" {
		return errors.New("workspace action declined")
	}
	return nil
}

func (s *session) askAdapter(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	var choices []string
	if method == "session/request_permission" {
		var options struct {
			Options []struct{ OptionID string }
		}
		if json.Unmarshal(raw, &options) != nil {
			return nil, acp.NewInvalidParams("invalid permission options")
		}
		for _, option := range options.Options {
			if option.OptionID != "" {
				choices = append(choices, option.OptionID)
			}
		}
		if len(choices) == 0 {
			return nil, acp.NewInvalidParams("permission requires at least one option")
		}
	}
	answer, _ := s.ask(ctx, method, raw, choices)
	if method == "elicitation/create" {
		if len(answer.Result) == 0 {
			return map[string]string{"action": "cancel"}, nil
		}
		return answer.Result, nil
	}
	outcome := map[string]string{"outcome": "cancelled"}
	if answer.OptionID != "" {
		outcome = map[string]string{"outcome": "selected", "optionId": answer.OptionID}
	}
	return map[string]any{"outcome": outcome}, nil
}

func (s *session) ask(ctx context.Context, method string, raw json.RawMessage, choices []string) (Answer, error) {
	if len(raw) > maxQuestion {
		return Answer{}, errors.New("callback payload exceeds daemon limit")
	}
	s.mu.Lock()
	if s.closed || s.canceling || ctx.Err() != nil || s.ctx.Err() != nil || len(s.pending) >= maxQuestions {
		s.mu.Unlock()
		return Answer{}, errors.New("session cannot accept more callbacks")
	}
	s.questionID++
	question := &pendingQuestion{
		question: Question{ID: s.questionID, Method: method, Params: raw},
		choices:  choices, reply: make(chan Answer, 1),
	}
	id := question.question.ID
	s.pending[id] = question
	s.publish(Event{Kind: QuestionOpened, Question: &question.question})
	s.mu.Unlock()

	select {
	case reply := <-question.reply:
		if ctx.Err() == nil && s.ctx.Err() == nil {
			return reply, nil
		}
	case <-ctx.Done():
	case <-s.ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishQuestion(id)
	return Answer{}, context.Canceled
}

func (s *session) finishQuestion(id uint64) {
	if s.pending[id] != nil {
		delete(s.pending, id)
		if !s.closed {
			s.publish(Event{Kind: QuestionClosed, QuestionID: id})
		}
	}
}

func (s *session) cancelQuestions() {
	for id, question := range s.pending {
		s.finishQuestion(id)
		question.reply <- Answer{}
	}
}

func (s *session) answer(p *peer, answer Answer) *Error {
	hash := fingerprint(answer)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireControl(p); err != nil {
		return err
	}
	if previous, ok := s.answered[answer.QuestionID]; ok {
		if previous != hash {
			return failure(Conflict, "question already has a different answer")
		}
		return nil
	}
	if s.canceling {
		return failure(Conflict, "turn cancellation is in progress")
	}
	question := s.pending[answer.QuestionID]
	if question == nil {
		return failure(NotFound, "question is no longer pending")
	}
	switch question.question.Method {
	case "elicitation/create":
		var result struct{ Action string }
		if answer.OptionID != "" || len(answer.Result) > maxQuestion || len(answer.Result) != 0 &&
			(json.Unmarshal(answer.Result, &result) != nil || (result.Action != "accept" && result.Action != "decline" && result.Action != "cancel")) {
			return failure(Invalid, "expected an ACP elicitation response")
		}
	default:
		if len(answer.Result) != 0 || answer.OptionID != "" && !slices.Contains(question.choices, answer.OptionID) {
			return failure(Invalid, "answer must select an offered option or cancel")
		}
	}
	s.answered[answer.QuestionID] = hash
	s.answerOrder = append(s.answerOrder, answer.QuestionID)
	if len(s.answerOrder) > receiptCount {
		delete(s.answered, s.answerOrder[0])
		s.answerOrder = s.answerOrder[1:]
	}
	s.finishQuestion(answer.QuestionID)
	question.reply <- answer
	return nil
}
