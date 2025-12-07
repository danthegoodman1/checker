package http_server

import (
	"context"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
)

const ReqIDKey = "reqID"

func CreateReqContext(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		reqID := uuid.NewString()
		ctx := context.WithValue(c.Request().Context(), ReqIDKey, reqID)
		zerolog.Ctx(ctx).UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("reqID", reqID)
		})
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}
