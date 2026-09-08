//go:build windows

// Package platform integrates the foreground monitor with operating system services.
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "TokenResetsMonitor"

type handler struct {
	parent context.Context
	run    func(context.Context) error
}

// Run delegates to SCM only when launched as a service. A console invocation
// stays a foreground process, including invocations from an elevated terminal.
func Run(ctx context.Context, run func(context.Context) error) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false, errors.New("cannot determine Windows service context")
	}
	if !isService {
		return false, nil
	}
	err = svc.Run(serviceName, &handler{parent: ctx, run: run})
	if err != nil {
		reportServiceError()
		return true, errors.New("Windows service dispatcher failed; see Windows Event Viewer")
	}
	return true, nil
}

func reportServiceError() {
	// Do not pass arbitrary errors to Event Log: configuration or HTTP errors
	// can contain credentials. The application writes sanitized detail to logs.
	log, err := eventlog.Open(serviceName)
	if err == nil {
		defer log.Close()
		_ = log.Error(1, "TokenResetsMonitor could not start or stopped unexpectedly. Check its configuration, directory permissions, and application logs.")
	}
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	ctx, cancel := context.WithCancel(h.parent)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	current := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- current
	for {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				reportServiceError()
				return true, 1
			}
			return false, 0
		case <-h.parent.Done():
			cancel()
			return waitForStop(done, statuses)
		case request, ok := <-requests:
			if !ok {
				cancel()
				return waitForStop(done, statuses)
			}
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- current
			case svc.Stop, svc.Shutdown:
				cancel()
				return waitForStop(done, statuses)
			}
		}
	}
}

func waitForStop(done <-chan error, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StopPending, WaitHint: 40000, CheckPoint: 1}
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	checkpoint := uint32(1)
	for {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				reportServiceError()
				return true, 1
			}
			return false, 0
		case <-tick.C:
			checkpoint++
			statuses <- svc.Status{State: svc.StopPending, WaitHint: 40000, CheckPoint: checkpoint}
		case <-deadline.C:
			reportServiceError()
			return true, 1
		}
	}
}

func Service(action, configPath, executablePath string) error {
	switch action {
	case "install", "start", "stop", "status", "uninstall":
	default:
		return errors.New("service action must be install, start, stop, status, or uninstall")
	}
	manager, err := mgr.Connect()
	if err != nil {
		return errors.New("cannot access Windows Service Control Manager; use an Administrator terminal")
	}
	defer manager.Disconnect()
	if action == "install" {
		return install(manager, configPath, executablePath)
	}
	service, err := manager.OpenService(serviceName)
	if err != nil {
		return errors.New("TokenResetsMonitor service is not installed or access was denied")
	}
	defer service.Close()
	switch action {
	case "start":
		state, err := service.Query()
		if err != nil {
			return errors.New("cannot query Windows service")
		}
		if state.State != svc.Running {
			if err := service.Start(); err != nil {
				return errors.New("cannot start Windows service; inspect Windows Event Viewer and application logs")
			}
			if err := waitState(service, svc.Running); err != nil {
				return err
			}
		}
		fmt.Println("TokenResetsMonitor: running")
	case "stop":
		if err := stop(service); err != nil {
			return err
		}
		fmt.Println("TokenResetsMonitor: stopped")
	case "status":
		state, err := service.Query()
		if err != nil {
			return errors.New("cannot query Windows service")
		}
		fmt.Printf("TokenResetsMonitor: %s\n", stateName(state.State))
	case "uninstall":
		if err := stop(service); err != nil {
			return err
		}
		if err := service.Delete(); err != nil {
			return errors.New("cannot delete Windows service")
		}
		_ = eventlog.Remove(serviceName)
		fmt.Println("TokenResetsMonitor service removed; configuration, state, and logs are preserved")
	}
	return nil
}

func install(manager *mgr.Mgr, configPath, executablePath string) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return errors.New("invalid configuration path")
	}
	if info, err := os.Stat(configPath); err != nil || info.IsDir() {
		return errors.New("configuration file must exist before installing the service")
	}
	if executablePath == "" {
		executablePath, err = os.Executable()
		if err != nil {
			return errors.New("cannot determine executable path")
		}
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return errors.New("invalid executable path")
	}
	if info, err := os.Stat(executablePath); err != nil || info.IsDir() {
		return errors.New("service executable must exist")
	}
	if existing, err := manager.OpenService(serviceName); err == nil {
		existing.Close()
		return errors.New("TokenResetsMonitor service is already installed; stop it before replacing the binary")
	}
	// mgr.CreateService quotes the executable and each argument with EscapeArg.
	service, err := manager.CreateService(serviceName, executablePath, mgr.Config{
		DisplayName:      serviceName,
		Description:      "Monitors public TokenResets announcements and delivers webhook and Telegram notifications.",
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: `NT AUTHORITY\LocalService`,
		DelayedAutoStart: true,
	}, "run", "--config", configPath)
	if err != nil {
		return errors.New("cannot install Windows service; check Administrator privileges")
	}
	defer service.Close()
	rollback := true
	defer func() {
		if rollback {
			_ = service.Delete()
		}
	}()
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: time.Minute},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Minute},
	}, 86400); err != nil {
		return errors.New("cannot configure Windows service recovery")
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return errors.New("cannot enable Windows service recovery")
	}
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return errors.New("cannot register Windows Event Log source")
	}
	rollback = false
	fmt.Println("TokenResetsMonitor service installed as LocalService (automatic, delayed start)")
	return nil
}

func stop(service *mgr.Service) error {
	state, err := service.Query()
	if err != nil {
		return errors.New("cannot query Windows service")
	}
	if state.State == svc.Stopped {
		return nil
	}
	if state.State != svc.StopPending {
		if _, err := service.Control(svc.Stop); err != nil {
			return errors.New("cannot stop Windows service")
		}
	}
	return waitState(service, svc.Stopped)
}

func waitState(service *mgr.Service, target svc.State) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		state, err := service.Query()
		if err != nil {
			return errors.New("cannot query Windows service")
		}
		if state.State == target {
			return nil
		}
		if target == svc.Running && state.State == svc.Stopped {
			return errors.New("Windows service stopped during startup; inspect Windows Event Viewer and application logs")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out waiting for Windows service state")
}

func stateName(state svc.State) string {
	switch state {
	case svc.Running:
		return "running"
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	default:
		return fmt.Sprintf("state %d", state)
	}
}
