package core

import "context"

type questionKey struct{}

// WithQuestion returns a context that carries the question of an inbox
// item: its type and its recommended option. Engine.RequestHuman reads it
// for a question item. Without it, the item has one question with the
// title and the options of the request.
func WithQuestion(ctx context.Context, q Question) context.Context {
	return context.WithValue(ctx, questionKey{}, q)
}

// QuestionFrom returns the question stored in ctx, if any.
func QuestionFrom(ctx context.Context) (Question, bool) {
	q, ok := ctx.Value(questionKey{}).(Question)
	return q, ok
}
