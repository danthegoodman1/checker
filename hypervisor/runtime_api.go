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
	Token           string `json:"token" validate:"required"` // Idempotency token for retry after restore
}

type CheckpointJobResponse struct {
	Job *Job `json:"job"`
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

	// Use background context for checkpoint - we don't want HTTP connection closure
	// to cancel the checkpoint mid-operation (especially for suspend checkpoints
	// where stopping the container kills the worker's connection).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	job, err := h.CheckpointJob(ctx, req.ID, suspendDuration, req.Token)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	return c.JSON(http.StatusOK, CheckpointJobResponse{Job: job})
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
