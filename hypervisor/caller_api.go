package hypervisor

import (
	"encoding/json"
	"net/http"

	"github.com/danthegoodman1/checker/http_server"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/labstack/echo/v4"
)

func (h *Hypervisor) RegisterCallerAPI(e *echo.Echo) {
	e.POST("/definitions", h.handleRegisterJobDefinition)
	e.DELETE("/definitions/:name/:version", h.handleUnregisterJobDefinition)
	e.GET("/definitions/:name/:version", h.handleGetJobDefinition)
	e.GET("/definitions", h.handleListJobDefinitions)

	e.POST("/jobs", h.handleSpawn)
	e.GET("/jobs", h.handleListJobs)
	e.GET("/jobs/:id", h.handleGetJob)
	e.DELETE("/jobs/:id", h.handleKillJob)
	e.GET("/jobs/:id/result", h.handleGetResult)
}

type RegisterJobDefinitionRequest struct {
	Name        string              `json:"name" validate:"required"`
	Version     string              `json:"version" validate:"required"`
	RuntimeType runtime.RuntimeType `json:"runtime_type" validate:"required"`
	Config      json.RawMessage     `json:"config" validate:"required"`
	Metadata    map[string]string   `json:"metadata"`
}

func (h *Hypervisor) handleRegisterJobDefinition(c echo.Context) error {
	var req RegisterJobDefinitionRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	rt, exists := h.runtimes[req.RuntimeType]
	if !exists {
		return echo.NewHTTPError(http.StatusBadRequest, "unknown runtime type")
	}

	config, err := rt.ParseConfig(req.Config)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid config: "+err.Error())
	}

	jd := &JobDefinition{
		Name:        req.Name,
		Version:     req.Version,
		RuntimeType: req.RuntimeType,
		Config:      config,
		Metadata:    req.Metadata,
	}

	if err := h.RegisterJobDefinition(jd); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	return c.JSON(http.StatusCreated, jd)
}

type UnregisterJobDefinitionRequest struct {
	Name    string `param:"name" validate:"required"`
	Version string `param:"version" validate:"required"`
}

func (h *Hypervisor) handleUnregisterJobDefinition(c echo.Context) error {
	var req UnregisterJobDefinitionRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	if err := h.UnregisterJobDefinition(req.Name, req.Version); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.NoContent(http.StatusNoContent)
}

type GetJobDefinitionRequest struct {
	Name    string `param:"name" validate:"required"`
	Version string `param:"version" validate:"required"`
}

func (h *Hypervisor) handleGetJobDefinition(c echo.Context) error {
	var req GetJobDefinitionRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	jd, err := h.GetJobDefinition(req.Name, req.Version)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, jd)
}

func (h *Hypervisor) handleListJobDefinitions(c echo.Context) error {
	definitions := h.ListJobDefinitions()
	return c.JSON(http.StatusOK, definitions)
}

type SpawnRequest struct {
	DefinitionName    string            `json:"definition_name" validate:"required"`
	DefinitionVersion string            `json:"definition_version" validate:"required"`
	Params            json.RawMessage   `json:"params"`
	Env               map[string]string `json:"env"`
	Metadata          map[string]string `json:"metadata"`
}

type SpawnResponse struct {
	JobID string `json:"job_id"`
}

func (h *Hypervisor) handleSpawn(c echo.Context) error {
	var req SpawnRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	jobID, err := h.Spawn(c.Request().Context(), SpawnOptions{
		DefinitionName:    req.DefinitionName,
		DefinitionVersion: req.DefinitionVersion,
		Params:            req.Params,
		Env:               req.Env,
		Metadata:          req.Metadata,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	return c.JSON(http.StatusCreated, SpawnResponse{JobID: jobID})
}

func (h *Hypervisor) handleListJobs(c echo.Context) error {
	jobs, err := h.ListJobs(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, jobs)
}

type GetJobRequest struct {
	ID string `param:"id" validate:"required"`
}

func (h *Hypervisor) handleGetJob(c echo.Context) error {
	var req GetJobRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	job, err := h.GetJob(c.Request().Context(), req.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, job)
}

type KillJobRequest struct {
	ID string `param:"id" validate:"required"`
}

func (h *Hypervisor) handleKillJob(c echo.Context) error {
	var req KillJobRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	if err := h.KillJob(c.Request().Context(), req.ID); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.NoContent(http.StatusNoContent)
}

type GetResultRequest struct {
	ID   string `param:"id" validate:"required"`
	Wait bool   `query:"wait"`
}

func (h *Hypervisor) handleGetResult(c echo.Context) error {
	var req GetResultRequest
	if err := http_server.ValidateRequest(c, &req); err != nil {
		return err
	}

	result, err := h.GetResult(c.Request().Context(), req.ID, req.Wait)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}

	return c.JSON(http.StatusOK, result)
}
