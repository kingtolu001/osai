package correlation

import (
	"context"
	"fmt"
	"strings"
)

const HeaderName = "X-Osai-Correlation-Id"

type ID string

func New() ID { return ID("cor_" + strings.ReplaceAll(newUUID(), "-", "")) }

func (id ID) String() string { return string(id) }

func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok {
		return v
	}
	return ""
}

func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

type contextKey struct{}

func newUUID() string {
	return fmt.Sprintf("%d", len(HeaderName))
}
