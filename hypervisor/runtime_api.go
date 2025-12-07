package hypervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danthegoodman1/checker/http_server"
	"github.com/labstack/echo/v4"
)

func (h *Hypervisor) RegisterRuntimeAPI(e *echo.Echo) {
	e.POST("/jobs/:id/checkpoint", h.handleCheckpointJob)
	e.POST("/jobs/:id/lock", h.handleTakeJobLock)
	e.DELETE("/jobs/:id/lock/:lockId", h.handleReleaseJobLock)
	e.POST("/jobs/:id/exit", h.handleExit)
	e.GET("/jobs/:id/params", h.handleGetParams)
	e.GET("/jobs/:id/metadata", h.handleGetMetadata)
}

type CheckpointJobRequest struct {
	ID              string `param:"id" validate:"required"`
	SuspendDuration string `json:"suspend_duration"`
}

type CheckpointJobResponse struct {
	Job           *Job  `json:"job"`
	GracePeriodMs int64 `json:"grace_period_ms,omitempty"`
}

func (h *Hypervisor) handleCheckpointJob(c echo.Context) error {
	var req CheckpointJobRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	var suspendDuration time.Duration
	if req.SuspendDuration != "" {
		var err error
		suspendDuration, err = time.ParseDuration(req.SuspendDuration)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid suspend_duration: "+err.Error())
		}
	}

	// Get the grace period before checkpointing
	gracePeriodMs, err := h.GetCheckpointGracePeriod(req.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	// Send the response first with the grace period.
	// The worker will wait for this grace period, ensuring it's idle when we checkpoint.
	if err := c.JSON(http.StatusOK, CheckpointJobResponse{GracePeriodMs: gracePeriodMs}); err != nil {
		return err
	}

	// Small delay to ensure the response is fully flushed over the network
	// before we stop the container
	time.Sleep(50 * time.Millisecond)

	// Now perform the actual checkpoint (this may stop the container on darwin).
	// Use a background context since the request context will be cancelled after we return.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = h.CheckpointJob(ctx, req.ID, suspendDuration)
	if err != nil {
		// Log the error but don't return it - response already sent
		logger.Error().Err(err).Str("job_id", req.ID).Msg("checkpoint failed after response sent")
	}

	return nil
}

type TakeJobLockRequest struct {
	ID string `param:"id" validate:"required"`
}

type TakeJobLockResponse struct {
	LockID string `json:"lock_id"`
}

func (h *Hypervisor) handleTakeJobLock(c echo.Context) error {
	var req TakeJobLockRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	lockID, err := h.TakeJobLock(c.Request().Context(), req.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, TakeJobLockResponse{LockID: lockID})
}

type ReleaseJobLockRequest struct {
	ID     string `param:"id" validate:"required"`
	LockID string `param:"lockId" validate:"required"`
}

func (h *Hypervisor) handleReleaseJobLock(c echo.Context) error {
	var req ReleaseJobLockRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	if err := h.ReleaseJobLock(c.Request().Context(), req.ID, req.LockID); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.NoContent(http.StatusNoContent)
}

type ExitRequest struct {
	ID       string          `param:"id" validate:"required"`
	ExitCode int             `json:"exit_code"`
	Output   json.RawMessage `json:"output"`
}

func (h *Hypervisor) handleExit(c echo.Context) error {
	var req ExitRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	if err := h.Exit(c.Request().Context(), req.ID, req.ExitCode, req.Output); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.NoContent(http.StatusNoContent)
}

type GetParamsRequest struct {
	ID string `param:"id" validate:"required"`
}

func (h *Hypervisor) handleGetParams(c echo.Context) error {
	var req GetParamsRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	params, err := h.GetParams(c.Request().Context(), req.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, params)
}

type GetMetadataRequest struct {
	ID string `param:"id" validate:"required"`
}

func (h *Hypervisor) handleGetMetadata(c echo.Context) error {
	var req GetMetadataRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	metadata, err := h.GetJobMetadata(c.Request().Context(), req.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, metadata)
}
