//go:build windows

// streamy-updater is a deliberately small detached Windows handoff process.
// Streamy starts it before quitting so the installer never runs while the main
// executable is still holding its own files open.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const waitTimeout = 2 * time.Minute

func main() {
	installer := flag.String("installer", "", "path to the downloaded Windows installer")
	pid := flag.Int("pid", 0, "PID of the Streamy process to wait for")
	executable := flag.String("exe", "", "path to Streamy to relaunch")
	logPath := flag.String("log", filepath.Join(os.TempDir(), "Streamy-update.log"), "durable updater log path")
	flag.Parse()

	if err := run(*installer, *pid, *executable, *logPath); err != nil {
		writeStatus(*logPath, "failed: "+err.Error())
		log.Printf("update failed: %v", err)
		os.Exit(1)
	}
	writeStatus(*logPath, "complete")
}

func run(installer string, pid int, executable, logPath string) error {
	if pid <= 0 {
		return errors.New("invalid Streamy PID")
	}
	if !isFile(installer) || !strings.EqualFold(filepath.Ext(installer), ".exe") {
		return fmt.Errorf("invalid installer path: %s", installer)
	}
	if !isFile(executable) || !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return fmt.Errorf("invalid Streamy executable path: %s", executable)
	}
	defer os.Remove(installer)

	if err := appendLog(logPath, "waiting for Streamy PID "+strconv.Itoa(pid)); err != nil {
		return err
	}
	if err := waitForProcess(pid, waitTimeout); err != nil {
		return err
	}
	if err := appendLog(logPath, "Streamy exited; running silent installer"); err != nil {
		return err
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open updater log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(installer, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		_ = appendLog(logPath, "installer failed: "+err.Error()+"; relaunching original Streamy")
		relaunch := exec.Command(executable)
		if err := relaunch.Start(); err != nil {
			return fmt.Errorf("silent installer failed: %w; also failed to relaunch: %w", err, err)
		}
		if relaunch.Process != nil {
			_ = relaunch.Process.Release()
		}
		return fmt.Errorf("silent installer failed: %w", err)
	}
	if err := appendLog(logPath, "installer completed; relaunching Streamy"); err != nil {
		return err
	}

	relaunch := exec.Command(executable)
	if err := relaunch.Start(); err != nil {
		return fmt.Errorf("relaunch Streamy: %w", err)
	}
	if relaunch.Process != nil {
		_ = relaunch.Process.Release()
	}
	return nil
}

func waitForProcess(pid int, timeout time.Duration) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find Streamy process: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := process.Wait()
		done <- waitErr
	}()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("wait for Streamy: %w", err)
		}
		return nil
	case <-time.After(timeout):
		return errors.New("timed out waiting for Streamy to exit")
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func appendLog(path, message string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open updater log: %w", err)
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339), message)
	return err
}

func writeStatus(logPath, status string) {
	if strings.TrimSpace(logPath) == "" {
		return
	}
	_ = os.WriteFile(logPath+".status", []byte(status+"\n"), 0o600)
	_ = appendLog(logPath, status)
}
