//go:build windows

package capture

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"packetlab/internal/models"
)

func (e *Engine) ResolveProcess(srcIP string, srcPort uint16) *models.ProcessInfo {
	return e.resolveProcessCached(srcIP, srcPort, buildProcTableWindows)
}

func buildProcTableWindows() map[string]*models.ProcessInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "netstat", "-ano", "-p", "tcp")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	table := make(map[string]*models.ProcessInfo)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "TCP") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		localAddr := fields[1]
		pid := 0
		fmt.Sscanf(fields[len(fields)-1], "%d", &pid)
		if idx := strings.LastIndex(localAddr, ":"); idx >= 0 {
			ipPart := localAddr[:idx]
			portPart := localAddr[idx+1:]
			table[fmt.Sprintf("%s:%s", ipPart, portPart)] = &models.ProcessInfo{PID: pid}
		}
	}
	return table
}
