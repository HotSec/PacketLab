//go:build linux || darwin

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
	return e.resolveProcessCached(srcIP, srcPort, buildProcTable)
}

func buildProcTable() map[string]*models.ProcessInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-i", "tcp", "-n", "-P", "-sTCP:ESTABLISHED")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	table := make(map[string]*models.ProcessInfo)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := fields[0]
		pid := 0
		if _, err := fmt.Sscanf(fields[1], "%d", &pid); err != nil {
			continue // PID 列解析失败，跳过该行
		}
		addr := fields[len(fields)-1]
		if idx := strings.Index(addr, "->"); idx > 0 {
			addr = addr[:idx]
		}
		table[addr] = &models.ProcessInfo{Name: name, PID: pid}
	}
	return table
}
