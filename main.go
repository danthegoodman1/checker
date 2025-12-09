package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/pg"
	"github.com/danthegoodman1/checker/runtime/podman"
)

func main() {
	// Connect to the database
	if err := pg.ConnectToDB(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
		os.Exit(1)
	}

	// Create the hypervisor
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "0.0.0.0:8080",
		RuntimeHTTPAddress: "0.0.0.0:8081",
		Pool:               pg.PGPool,
	})

	// Register the Podman runtime (only works on Linux)
	podmanRuntime, err := podman.NewRuntime()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to register podman runtime: %v\n", err)
	} else {
		if err := h.RegisterRuntime(podmanRuntime); err != nil {
			fmt.Fprintf(os.Stderr, "failed to register podman runtime: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Registered runtime: podman")
	}

	// Recover state from database (job definitions and jobs)
	ctx := context.Background()
	if err := h.RecoverState(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to recover state: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Checker hypervisor initialized")
	fmt.Println("Caller API listening on :8080")
	fmt.Println("Runtime API listening on :8081")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Shutdown complete")
}
