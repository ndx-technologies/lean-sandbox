package sdk

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/ndx-technologies/lean-sandbox/api"
)

type ErrHTTP struct {
	Status int
	Body   string
}

func (e *ErrHTTP) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

func httpError(ctx context.Context, resp *http.Response) error {
	var e api.Error
	if err := json.UnmarshalRead(resp.Body, &e); err != nil {
		slog.ErrorContext(ctx, "cannot decode error", "error", err)
	}
	if e.Error == "" {
		e.Error = resp.Status
	}
	return &ErrHTTP{Status: resp.StatusCode, Body: e.Error}
}
