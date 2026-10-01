package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wyw14/cry-97/internal/api"
	"github.com/wyw14/cry-97/internal/config"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/process"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}
	clock := func() time.Time { return time.Now().UTC() }
	lineIDs := []model.LineID{"line-1", "line-2", "line-3"}
	flowConfig := discharge.FlowConfig{
		MaxFlow:        cfg.DischargeFlow.MaxFlow,
		ContactTimeout: cfg.DischargeFlow.ContactTimeout,
		TelemetryTTL:   cfg.DischargeFlow.TelemetryTTL,
	}
	plant, err := process.NewPlant(cfg.DataDir, lineIDs, clock, flowConfig)
	if err != nil {
		return err
	}
	for _, lineID := range lineIDs {
		if _, err := plant.RecoverLine(context.Background(), lineID); err != nil {
			return err
		}
	}
	serverAPI, err := api.NewServer(plant, os.DirFS(cfg.WebDir))
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.Address, Handler: serverAPI.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second,
	}
	serveErrors := make(chan error, 1)
	maintenanceContext, stopMaintenance := context.WithCancel(context.Background())
	defer stopMaintenance()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-maintenanceContext.Done():
				return
			case <-ticker.C:
				plant.SweepDischargeFlow()
				if _, runErr := plant.RunCompensations(maintenanceContext); runErr != nil && !errors.Is(runErr, context.Canceled) {
					log.Printf("compensation maintenance: %v", runErr)
				}
			}
		}
	}()
	go func() { serveErrors <- server.ListenAndServe() }()
	log.Printf("BioTreat listening on http://%s", cfg.Address)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
		stopMaintenance()
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return server.Shutdown(ctx)
	case serveErr := <-serveErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}
